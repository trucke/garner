package simulation

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/inspect"
	"github.com/trucke/garner/internal/store"
)

// The world is everything outside Garner: the shows, the releases the
// indexers hold, SABnzbd, Jellyfin, FFprobe, the files on the share and the
// clock. Its fakes answer from its state alone and only events change it,
// so the order of Garner's requests never draws randomness.

const (
	prowlarrHost = "prowlarr.sim"
	sabnzbdHost  = "sabnzbd.sim"
	jellyfinHost = "jellyfin.sim"
	apiKey       = "sim-key"
)

// start is when every run begins.
var start = time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

type world struct {
	seed   uint64
	root   string // the share
	mapped bool   // SABnzbd and Jellyfin see the share under /data
	now    time.Time
	rnd    *rand.Rand // the world's choices
	faults faults

	shows    []*show
	postings []*posting
	videos   int // video IDs handed out

	prowlarrDown bool
	indexerError map[int]bool
	sab          sab
	jf           jellyfin
}

// faults are the ways the world goes wrong in one run. Each seed turns on
// its own subset, which reaches more combinations than every fault at a
// low rate (swarm testing).
type faults struct {
	prowlarrDown, indexerErrors                bool
	sabDown, sabLoseAnswer, sabHide, sabVanish bool
	sabFail                                    bool
	jfDown, jfIgnoreHints, jfLate, jfNever     bool
	jfWrong                                    bool
	mislabeled, wrongRuntime, corrupt          bool
	restarts                                   bool
}

func randomFaults(r *rand.Rand) faults {
	on := func() bool { return r.IntN(2) == 0 }
	return faults{
		prowlarrDown: on(), indexerErrors: on(),
		sabDown: on(), sabLoseAnswer: on(), sabHide: on(), sabVanish: on(), sabFail: on(),
		jfDown: on(), jfIgnoreHints: on(), jfLate: on(), jfNever: on(), jfWrong: on(),
		mislabeled: on(), wrongRuntime: on(), corrupt: on(),
		restarts: on(),
	}
}

func (f faults) String() string {
	var on []string
	for name, set := range map[string]bool{
		"prowlarr-down": f.prowlarrDown, "indexer-errors": f.indexerErrors,
		"sab-down": f.sabDown, "sab-lose-answer": f.sabLoseAnswer, "sab-hide": f.sabHide, "sab-vanish": f.sabVanish, "sab-fail": f.sabFail,
		"jf-down": f.jfDown, "jf-ignore-hints": f.jfIgnoreHints, "jf-late": f.jfLate, "jf-never": f.jfNever, "jf-wrong": f.jfWrong,
		"mislabeled": f.mislabeled, "wrong-runtime": f.wrongRuntime, "corrupt": f.corrupt, "restarts": f.restarts,
	} {
		if set {
			on = append(on, name)
		}
	}
	slices.Sort(on)
	return strings.Join(on, " ")
}

type show struct {
	id       int64
	name     string // "Harbor Precinct"
	year     int
	monitor  string
	episodes []*episode
	deleted  bool
}

func (s *show) dotted() string { return strings.ReplaceAll(s.name, " ", ".") }

type episode struct {
	id             int64
	show           *show
	season, number int
	aired          time.Time
	runtime        int // minutes
}

// posting is a release an indexer holds, with the truth about its file.
type posting struct {
	id        int
	title     string
	episode   *episode
	indexers  []int
	posted    time.Time
	languages []string // as the indexers report them
	size      int64
	truth     video
	corrupt   bool // FFprobe cannot read the file
}

// video is what a simulated video file holds: who it is and what FFprobe
// finds in it. The truth travels with the file when Garner moves it.
type video struct {
	ID      string   `json:"id"`
	Posting int      `json:"posting"`
	Audio   []string `json:"audio"` // FFprobe's language tags
	Height  int      `json:"height"`
	Minutes int      `json:"minutes"`
}

var showNames = []string{"Harbor Precinct", "Quiet Harbour", "Lanterns and Tides", "Copper Valley", "Night Ferry", "Glass Orchard"}

var groups = []string{"GRP", "WEBTEAM", "TIDE", "ORCHARD", "FERRY"}

// newWorld builds the share, the shows and the releases for a seed.
func newWorld(seed uint64, root string) (*world, error) {
	w := &world{seed: seed, root: root, now: start, rnd: rand.New(rand.NewPCG(seed, 1)), indexerError: map[int]bool{}}
	w.faults = randomFaults(rand.New(rand.NewPCG(seed, 2)))
	w.mapped = w.rnd.IntN(2) == 0
	w.jf.items = map[string]*jfItem{}
	for _, dir := range []string{"usenet/complete/tv", "usenet/incomplete", "library/tv", "library/anime", "elsewhere/notes", "garner"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return nil, err
		}
	}
	// Files that belong to nobody Garner works for; they must never change.
	for name, content := range map[string]string{"elsewhere/holiday.mkv": `{"id":"private"}`, "elsewhere/notes/todo.txt": "buy milk"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			return nil, err
		}
	}

	names := slices.Clone(showNames)
	w.rnd.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	for i, name := range names[:2+w.rnd.IntN(2)] {
		s := &show{id: int64(7001 + i), name: name, year: 2010 + w.rnd.IntN(15), monitor: []string{"all", "new", "off"}[w.rnd.IntN(3)]}
		seasons := 1 + w.rnd.IntN(2)
		// One episode a week, from 3 to 13 weeks ago: some aired long ago,
		// the last ones may still be to come.
		first := start.AddDate(0, 0, -7*(3+w.rnd.IntN(6)*seasons))
		for season := 1; season <= seasons; season++ {
			for number := 1; number <= 3+w.rnd.IntN(3); number++ {
				e := &episode{id: s.id*100 + int64(len(s.episodes)+1), show: s, season: season, number: number,
					aired: first.AddDate(0, 0, 7*len(s.episodes)), runtime: 22 + 23*w.rnd.IntN(2)}
				s.episodes = append(s.episodes, e)
				w.post(e)
			}
		}
		w.shows = append(w.shows, s)
	}
	return w, nil
}

// post puts one to three releases of an episode on the indexers, the
// first hours after it aired, an upgrade days later.
func (w *world) post(e *episode) {
	type kind struct {
		tags      string
		languages []string
		audio     []string
		height    int
	}
	kinds := []kind{
		{"German.DL.1080p.WEB.h264", []string{"German", "English"}, []string{"ger", "eng"}, 1080},
		{"German.720p.WEB.h264", []string{"German"}, []string{"ger"}, 720},
		{"German.DL.720p.WEB.h264", []string{"German", "English"}, []string{"ger", "eng"}, 720},
		{"1080p.WEB.h264", nil, []string{"eng"}, 1080},
	}
	for i := range 1 + w.rnd.IntN(3) {
		k := kinds[w.rnd.IntN(len(kinds))]
		if i > 0 {
			k = kinds[0] // a later, better release
		}
		p := &posting{
			id: len(w.postings) + 1, episode: e, languages: k.languages, size: int64(300+w.rnd.IntN(1500)) << 20,
			title:  fmt.Sprintf("%s.S%02dE%02d.%s-%s", e.show.dotted(), e.season, e.number, k.tags, groups[w.rnd.IntN(len(groups))]),
			posted: e.aired.Add(time.Duration(1+w.rnd.IntN(30))*time.Hour + time.Duration(i*(3+w.rnd.IntN(6)))*24*time.Hour),
			truth:  video{Audio: k.audio, Height: k.height, Minutes: e.runtime},
		}
		if w.faults.mislabeled && w.rnd.IntN(4) == 0 {
			p.truth.Audio = []string{"eng"} // the name says German; the file has none
		}
		if w.faults.wrongRuntime && w.rnd.IntN(5) == 0 {
			p.truth.Minutes = 3 * e.runtime
		}
		p.corrupt = w.faults.corrupt && w.rnd.IntN(6) == 0
		p.indexers = []int{1 + w.rnd.IntN(2)}
		if w.rnd.IntN(3) == 0 {
			p.indexers = []int{1, 2}
		}
		w.postings = append(w.postings, p)
	}
}

// seedStore records the world's shows in Garner's database, as if the user had
// added them.
func (w *world) seedStore(ctx context.Context, st *store.Store) error {
	for _, s := range w.shows {
		if err := w.seedShow(ctx, st, s); err != nil {
			return err
		}
	}
	return nil
}

// seedShow records one show.
func (w *world) seedShow(ctx context.Context, st *store.Store, s *show) error {
	d := store.SeriesData{Series: store.Series{
		ID: s.id, Slug: strings.ToLower(strings.ReplaceAll(s.name, " ", "-")), Name: s.name, NameEng: s.name,
		OriginalLanguage: "eng", OriginalCountry: "usa", Status: "Continuing", Year: s.year,
		DefaultOrder: "official", TVDBUpdated: "2026-01-01", Profile: "tv",
	}}
	for _, e := range s.episodes {
		d.Episodes = append(d.Episodes, store.Episode{
			ID: e.id, Aired: e.aired.Format(time.DateOnly), Runtime: e.runtime, NameEng: fmt.Sprintf("Part %d", e.number),
			Numbers: map[string]store.Position{"official": {Season: e.season, Number: e.number}},
		})
	}
	if err := st.SaveSeries(ctx, d, true, w.now); err != nil {
		return err
	}
	return st.SetMonitor(ctx, s.id, s.monitor, w.now)
}

// config is garner.toml for the world.
func (w *world) config() *config.Config {
	cfg := &config.Config{
		DataDir:  filepath.Join(w.root, "garner"),
		Library:  config.Library{TV: filepath.Join(w.root, "library/tv"), Anime: filepath.Join(w.root, "library/anime")},
		Prowlarr: config.Service{URL: "http://" + prowlarrHost, APIKey: apiKey},
		SABnzbd:  config.SABnzbd{Service: config.Service{URL: "http://" + sabnzbdHost, APIKey: apiKey}, Category: "tv"},
		Jellyfin: config.Service{URL: "http://" + jellyfinHost, APIKey: apiKey},
		Monitor: config.Monitor{
			Enabled: true, RecentDays: 14,
			MissingRecent: config.Duration(6 * time.Hour), MissingOlder: config.Duration(24 * time.Hour),
			Upgrades: true, UpgradeRecent: config.Duration(24 * time.Hour), UpgradeOlder: config.Duration(7 * 24 * time.Hour),
		},
	}
	if w.mapped {
		cfg.SABnzbd.Paths = config.PathMap{"/data/usenet": filepath.Join(w.root, "usenet")}
		cfg.Jellyfin.Paths = config.PathMap{"/data/library": filepath.Join(w.root, "library")}
	}
	return cfg
}

// sabView and jfView are where SABnzbd and Jellyfin see a file on the
// share; fromJF turns Jellyfin's view back.
func (w *world) sabView(local string) string { return w.view(local, "usenet") }
func (w *world) jfView(local string) string  { return w.view(local, "library") }

func (w *world) view(local, mount string) string {
	if !w.mapped {
		return local
	}
	rel, err := filepath.Rel(filepath.Join(w.root, mount), local)
	if err != nil || strings.HasPrefix(rel, "..") {
		return local
	}
	return filepath.Join("/data", mount, rel)
}

func (w *world) fromJF(remote string) string {
	if rel, ok := strings.CutPrefix(remote, "/data/library"); ok && w.mapped {
		return filepath.Join(w.root, "library", rel)
	}
	return remote
}

// hash spreads decisions that must not depend on the order of Garner's
// requests, such as how Jellyfin identifies one file, over the seed.
func (w *world) hash(s string) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%s", w.seed, s)
	return h.Sum64()
}

func (w *world) rel(p string) string {
	if r, err := filepath.Rel(w.root, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

// newVideo writes a video file of a posting.
func (w *world) newVideo(path string, p *posting) error {
	w.videos++
	v := p.truth
	v.ID, v.Posting = fmt.Sprintf("v%04d", w.videos), p.id
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if p.corrupt {
		data = append([]byte("garbage "), data...)
	}
	return os.WriteFile(path, data, 0o644)
}

// readVideo reads a simulated video's identity; ok is false for anything
// else, including corrupt videos, which keep their identity after the junk.
func readVideo(path string) (v video, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return v, false
	}
	data = bytes.TrimPrefix(data, []byte("garbage "))
	return v, json.Unmarshal(data, &v) == nil && v.ID != ""
}

// transport carries Garner's requests to the fakes, in memory and in the
// calling goroutine.
type transport struct{ w *world }

var errDown = errors.New("connection refused")

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := t.w
	var h http.HandlerFunc
	switch r.URL.Host {
	case prowlarrHost:
		if w.prowlarrDown {
			return nil, errDown
		}
		h = w.prowlarr
	case sabnzbdHost:
		if w.sab.down {
			return nil, errDown
		}
		h = w.sabnzbd
	case jellyfinHost:
		if w.jf.down {
			return nil, errDown
		}
		h = w.jellyfin
	default:
		return nil, fmt.Errorf("no such host: %s", r.URL.Host)
	}
	rec := httptest.NewRecorder()
	h(rec, r)
	if w.sab.dropAnswer {
		// SABnzbd acted, but the answer never arrived.
		w.sab.dropAnswer = false
		return nil, errors.New("connection reset by peer")
	}
	res := rec.Result()
	res.Request = r
	return res, nil
}

// prober reads a video's truth instead of running FFprobe.
type prober struct{}

func (prober) Probe(_ context.Context, path string) (inspect.Probe, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return inspect.Probe{}, err
	}
	var v video
	if err := json.Unmarshal(data, &v); err != nil {
		return inspect.Probe{}, fmt.Errorf("%w: invalid data found when processing input", inspect.ErrUnreadable)
	}
	p := inspect.Probe{Duration: time.Duration(v.Minutes) * time.Minute, Video: []inspect.Video{{Codec: "h264", Width: v.Height * 16 / 9, Height: v.Height}}}
	for _, l := range v.Audio {
		p.Audio = append(p.Audio, inspect.Track{Codec: "aac", Language: l})
	}
	return p, nil
}

func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(v)
}

// prowlarr answers Prowlarr's indexer list and each indexer's Newznab feed.
func (w *world) prowlarr(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/indexer" {
		writeJSON(rw, []map[string]any{
			{"id": 1, "name": "One", "enable": true, "protocol": "usenet"},
			{"id": 2, "name": "Two", "enable": true, "protocol": "usenet"},
		})
		return
	}
	var n int
	if _, err := fmt.Sscanf(r.URL.Path, "/%d/api", &n); err != nil {
		http.NotFound(rw, r)
		return
	}
	if w.indexerError[n] {
		fmt.Fprint(rw, `<?xml version="1.0" encoding="UTF-8"?><error code="900" description="Indexer is not responding"/>`)
		return
	}
	q := r.URL.Query()
	tvdb, _ := strconv.ParseInt(q.Get("tvdbid"), 10, 64)
	season, _ := strconv.Atoi(q.Get("season"))
	ep, _ := strconv.Atoi(q.Get("ep"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	var found []*posting
	for _, p := range w.postings {
		e := p.episode
		if !p.posted.After(w.now) && slices.Contains(p.indexers, n) && e.show.id == tvdb && e.season == season && (ep == 0 || e.number == ep) {
			found = append(found, p)
		}
	}
	found = found[min(offset, len(found)):min(offset+max(limit, 1), len(found))]
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0" xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/"><channel>`)
	for _, p := range found {
		link := fmt.Sprintf("http://%s/%d/download?id=%d&apikey=%s", prowlarrHost, n, p.id, apiKey)
		fmt.Fprintf(&b, "<item><title>%s</title><link>%s</link><size>%d</size><pubDate>%s</pubDate>", esc(p.title), esc(link), p.size, p.posted.Format(time.RFC1123Z))
		fmt.Fprintf(&b, `<newznab:attr name="tvdbid" value="%d"/>`, p.episode.show.id)
		for _, l := range p.languages {
			fmt.Fprintf(&b, `<newznab:attr name="language" value="%s"/>`, l)
		}
		b.WriteString("</item>")
	}
	b.WriteString("</channel></rss>")
	fmt.Fprint(rw, b.String())
}

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// sab is SABnzbd: its jobs, queued, finished or failed.
type sab struct {
	jobs       []*job
	down       bool
	loseNext   bool // the answer to the next addurl gets lost
	hideNext   bool // the next job never shows up in the queue or history
	dropAnswer bool
}

type job struct {
	id      string
	name    string
	cat     string
	posting *posting
	added   time.Time
	status  string // Queued, Downloading, Completed or Failed
	percent int
	dir     string // the job folder, once completed
	fail    string
	hidden  bool // SABnzbd never lists it
	gone    bool // deleted, or removed in SABnzbd by hand
	// vanished means someone removed the job in SABnzbd by hand, outside
	// Garner.
	vanished bool
	// lost means the job's folder went; SABnzbd may give a later job of the
	// same release the same folder.
	lost bool
}

func (j *job) active() bool { return !j.gone && (j.status == "Queued" || j.status == "Downloading") }
func (j *job) done() bool   { return !j.gone && (j.status == "Completed" || j.status == "Failed") }

func (w *world) sabnzbd(rw http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	find := func(id string, want func(*job) bool) *job {
		for _, j := range w.sab.jobs {
			if j.id == id && !j.hidden && want(j) {
				return j
			}
		}
		return nil
	}
	matches := func(j *job, name string) bool {
		if ids := r.PostFormValue("nzo_ids"); ids != "" {
			return slices.Contains(strings.Split(ids, ","), j.id)
		}
		return strings.Contains(strings.ToLower(name), strings.ToLower(r.PostFormValue("search")))
	}
	switch r.PostFormValue("mode") {
	case "addurl":
		u, err := url.Parse(r.PostFormValue("name"))
		id, _ := strconv.Atoi(u.Query().Get("id"))
		if err != nil || id < 1 || id > len(w.postings) {
			writeJSON(rw, map[string]any{"status": false, "error": "fetching failed"})
			return
		}
		j := &job{id: fmt.Sprintf("SABnzbd_nzo_%d", len(w.sab.jobs)+1), name: r.PostFormValue("nzbname"), cat: r.PostFormValue("cat"),
			posting: w.postings[id-1], added: w.now, status: "Queued", hidden: w.sab.hideNext}
		w.sab.hideNext = false
		w.sab.jobs = append(w.sab.jobs, j)
		w.sab.dropAnswer, w.sab.loseNext = w.sab.loseNext, false
		writeJSON(rw, map[string]any{"status": true, "nzo_ids": []string{j.id}})
	case "queue":
		if r.PostFormValue("name") == "delete" {
			if j := find(r.PostFormValue("value"), (*job).active); j != nil {
				j.gone = true
				writeJSON(rw, map[string]any{"status": true})
			} else {
				writeJSON(rw, map[string]any{"status": false})
			}
			return
		}
		slots := []map[string]any{}
		for _, j := range w.sab.jobs {
			if j.active() && !j.hidden && matches(j, j.name) {
				slots = append(slots, map[string]any{"nzo_id": j.id, "filename": j.name, "cat": j.cat, "status": j.status,
					"percentage": strconv.Itoa(j.percent), "timeleft": "0:10:00", "time_added": j.added.Unix()})
			}
		}
		writeJSON(rw, map[string]any{"queue": map[string]any{"slots": slots}})
	case "history":
		if r.PostFormValue("name") == "delete" {
			j := find(r.PostFormValue("value"), (*job).done)
			if j == nil {
				writeJSON(rw, map[string]any{"status": false})
				return
			}
			j.gone = true
			if j.dir != "" && r.PostFormValue("del_files") == "1" {
				os.RemoveAll(j.dir)
			}
			writeJSON(rw, map[string]any{"status": true})
			return
		}
		slots := []map[string]any{}
		for _, j := range w.sab.jobs {
			if j.done() && !j.hidden && matches(j, j.name) {
				storage := ""
				if j.dir != "" {
					storage = w.sabView(j.dir)
				}
				slots = append(slots, map[string]any{"nzo_id": j.id, "name": j.name, "category": j.cat, "status": j.status,
					"fail_message": j.fail, "storage": storage, "bytes": j.posting.size, "time_added": j.added.Unix(), "stage_log": []any{}})
			}
		}
		writeJSON(rw, map[string]any{"history": map[string]any{"slots": slots}})
	default:
		writeJSON(rw, map[string]any{"status": false, "error": "unknown mode"})
	}
}

var sabFailures = []string{
	"Repair failed, not enough repair blocks (14 short)",
	"Download failed - Not on your server(s)",
	"Unpacking failed, write error or disk is full?",
}

// progress moves a job on: from the queue into downloading, then a quarter
// at a time, then into the history, completed or failed.
func (w *world) progress(j *job) (string, error) {
	switch {
	case j.status == "Queued":
		j.status = "Downloading"
	case j.percent < 100:
		j.percent += 25
	case w.faults.sabFail && w.rnd.IntN(4) == 0:
		j.status, j.fail = "Failed", sabFailures[w.rnd.IntN(len(sabFailures))]
	default:
		if err := w.complete(j); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s Completed in %s", j.id, w.rel(j.dir)), nil
	}
	return fmt.Sprintf("%s %s %d%%", j.id, j.status, j.percent), nil
}

// complete puts a job's files into its folder, as SABnzbd does after
// repairing and unpacking: an obfuscated video, sometimes a sample, an NFO
// and a subtitle.
func (w *world) complete(j *job) error {
	dir := filepath.Join(w.root, "usenet/complete", j.cat, j.name)
	for n := 1; ; n++ {
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			break
		}
		dir = filepath.Join(w.root, "usenet/complete", j.cat, fmt.Sprintf("%s.%d", j.name, n))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("%08x", w.rnd.Uint32())
	if err := w.newVideo(filepath.Join(dir, name+".mkv"), j.posting); err != nil {
		return err
	}
	if w.rnd.IntN(2) == 0 {
		os.WriteFile(filepath.Join(dir, j.name+".sample.mkv"), []byte("sample"), 0o644)
	}
	if w.rnd.IntN(2) == 0 {
		os.WriteFile(filepath.Join(dir, j.name+".nfo"), []byte("info"), 0o644)
	}
	if w.rnd.IntN(3) == 0 {
		os.WriteFile(filepath.Join(dir, name+".de.srt"), []byte("1\n00:00:01,000 --> 00:00:02,000\nHallo\n"), 0o644)
	}
	j.status, j.percent, j.dir = "Completed", 100, dir
	return nil
}

// jellyfin is Jellyfin with the TheTVDB plugin: it lists the files it
// scanned and identifies them as episodes, sometimes late, never or wrong.
type jellyfin struct {
	down    bool
	hints   []string // files Garner told it about, not scanned yet
	refresh bool
	items   map[string]*jfItem // by file, as Garner reaches it
}

type jfItem struct {
	show    int64 // TVDB show, from the folder name
	episode int64 // TVDB episode once identified; 0 before
	scans   int   // scans until identified; -1 never
	wrong   bool
}

var (
	tvdbFolder = regexp.MustCompile(`\[tvdbid-(\d+)\]`)
	sxxeyy     = regexp.MustCompile(`S(\d+)E(\d+)`)
)

// scan does what Jellyfin's scan does: it drops files that are gone, adds
// the files it was told about or, after a refresh, every file in the
// library, and identifies what it can.
func (w *world) scan() string {
	removed, added, identified := 0, 0, 0
	for _, p := range sortedKeys(w.jf.items) {
		if _, err := os.Stat(p); err != nil {
			delete(w.jf.items, p)
			removed++
		}
	}
	var todo []string
	switch {
	case w.jf.refresh:
		w.jf.refresh, w.jf.hints = false, nil
		filepath.WalkDir(filepath.Join(w.root, "library"), func(p string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() && strings.HasSuffix(p, ".mkv") {
				todo = append(todo, p)
			}
			return nil
		})
	case !w.faults.jfIgnoreHints:
		todo, w.jf.hints = w.jf.hints, nil
	}
	for _, p := range todo {
		if _, ok := w.jf.items[p]; ok {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		m := tvdbFolder.FindStringSubmatch(p)
		if m == nil {
			continue
		}
		id, _ := strconv.ParseInt(m[1], 10, 64)
		h := w.hash(w.rel(p))
		it := &jfItem{show: id}
		if w.faults.jfLate {
			it.scans = int(h % 3)
		}
		if w.faults.jfNever && h%5 == 0 {
			it.scans = -1
		}
		it.wrong = w.faults.jfWrong && h%7 == 0
		w.jf.items[p] = it
		added++
	}
	for _, p := range sortedKeys(w.jf.items) {
		it := w.jf.items[p]
		if it.episode != 0 || it.scans < 0 {
			continue
		}
		if it.scans > 0 {
			it.scans--
			continue
		}
		if e := w.identify(it, p); e != nil {
			it.episode = e.id
			identified++
		}
	}
	return fmt.Sprintf("-%d +%d identified %d", removed, added, identified)
}

// identify finds the episode a file name names, or with the wrong fault
// the one after it.
func (w *world) identify(it *jfItem, p string) *episode {
	m := sxxeyy.FindStringSubmatch(filepath.Base(p))
	if m == nil {
		return nil
	}
	season, _ := strconv.Atoi(m[1])
	number, _ := strconv.Atoi(m[2])
	for _, s := range w.shows {
		if s.id != it.show {
			continue
		}
		for i, e := range s.episodes {
			if e.season == season && e.number == number {
				if it.wrong {
					return s.episodes[(i+1)%len(s.episodes)]
				}
				return e
			}
		}
	}
	return nil
}

func (w *world) episode(id int64) *episode {
	for _, s := range w.shows {
		for _, e := range s.episodes {
			if e.id == id {
				return e
			}
		}
	}
	return nil
}

func (w *world) jellyfin(rw http.ResponseWriter, r *http.Request) {
	// A show is its folder, the one above the season folder.
	showOf := func(p string) string { return filepath.Dir(filepath.Dir(p)) }
	showID := func(folder string) string { return fmt.Sprintf("%x", w.hash(w.rel(folder))) }
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/Library/Media/Updated":
		var body struct {
			Updates []struct{ Path, UpdateType string }
		}
		json.NewDecoder(r.Body).Decode(&body)
		for _, u := range body.Updates {
			local := w.fromJF(u.Path)
			if u.UpdateType == "Deleted" {
				delete(w.jf.items, local)
			} else {
				w.jf.hints = append(w.jf.hints, local)
			}
		}
		rw.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/Library/Refresh":
		w.jf.refresh = true
		rw.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/Library/VirtualFolders":
		writeJSON(rw, []map[string]any{{"Name": "TV Shows", "CollectionType": "tvshows",
			"Locations": []string{w.jfView(filepath.Join(w.root, "library/tv")), w.jfView(filepath.Join(w.root, "library/anime"))}}})
	case r.URL.Path == "/Items":
		seen := map[string]bool{}
		items := []map[string]any{}
		for _, p := range sortedKeys(w.jf.items) {
			folder := showOf(p)
			if seen[folder] {
				continue
			}
			seen[folder] = true
			items = append(items, map[string]any{"Id": showID(folder), "Name": filepath.Base(folder), "Path": w.jfView(folder),
				"ProviderIds": map[string]string{"Tvdb": strconv.FormatInt(w.jf.items[p].show, 10)}})
		}
		writeJSON(rw, map[string]any{"Items": items, "TotalRecordCount": len(items)})
	case strings.HasPrefix(r.URL.Path, "/Shows/") && strings.HasSuffix(r.URL.Path, "/Episodes"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/Shows/"), "/Episodes")
		items := []map[string]any{}
		for _, p := range sortedKeys(w.jf.items) {
			if showID(showOf(p)) != id {
				continue
			}
			it := w.jf.items[p]
			item := map[string]any{"Path": w.jfView(p), "Name": filepath.Base(p), "ProviderIds": map[string]string{}}
			if e := w.episode(it.episode); e != nil {
				item["Name"], item["ParentIndexNumber"], item["IndexNumber"] = fmt.Sprintf("Part %d", e.number), e.season, e.number
				item["ProviderIds"] = map[string]string{"Tvdb": strconv.FormatInt(e.id, 10)}
			}
			items = append(items, item)
		}
		writeJSON(rw, map[string]any{"Items": items})
	default:
		http.NotFound(rw, r)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
