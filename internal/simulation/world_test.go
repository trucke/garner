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
	"syscall"
	"time"

	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/inspect"
	"github.com/trucke/garner/internal/store"
)

// The world is everything outside Garner: the shows, the releases the
// indexers hold, SABnzbd, Jellyfin, FFprobe, the files on the share, its
// disks and the clock. Its fakes answer from its state alone and only
// events change it, so the order of Garner's requests never draws
// randomness.
//
// The share holds:
//
//	usenet/complete/tv   SABnzbd's finished jobs
//	library/tv, anime    Garner's library roots
//	media-old/tv         an older library Jellyfin also holds
//	elsewhere            files nobody Garner works for owns
//	unplugged            where an unplugged library folder waits
//	garner               Garner's database

const (
	prowlarrHost = "prowlarr.sim"
	sabnzbdHost  = "sabnzbd.sim"
	jellyfinHost = "jellyfin.sim"
	apiKey       = "sim-key"
)

// start is when every run begins.
var start = time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

// roots are the library folders Garner or Jellyfin hold; none of them may
// ever go, except when the world unplugs one.
var roots = []string{"library/tv", "library/anime", "media-old/tv"}

type world struct {
	seed   uint64
	root   string // the share
	mapped bool   // SABnzbd and Jellyfin see the share under /data
	// disks says which top folders lie on another disk than the library:
	// none, "usenet" (no import can move its files) or "media-old" (files
	// taken over there cannot be renamed into Garner's folder).
	disks  string
	now    time.Time
	rnd    *rand.Rand // the world's choices
	faults faults

	shows    []*show
	postings []*posting
	videos   int // video IDs handed out
	// foreign are files no one gave Garner, by path, with their content and
	// how the file looked when it was written.
	foreign map[string]foreignFile

	prowlarrDown bool
	indexerError map[int]bool
	sab          sab
	jf           jellyfin
	unplugged    string // the root that is unplugged, or ""
	readOnly     string // the folder that refuses writes, or ""
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
	unplug, readOnly, catalogue, strays        bool
	restarts                                   bool
}

func randomFaults(r *rand.Rand) faults {
	on := func() bool { return r.IntN(2) == 0 }
	return faults{
		prowlarrDown: on(), indexerErrors: on(),
		sabDown: on(), sabLoseAnswer: on(), sabHide: on(), sabVanish: on(), sabFail: on(),
		jfDown: on(), jfIgnoreHints: on(), jfLate: on(), jfNever: on(), jfWrong: on(),
		mislabeled: on(), wrongRuntime: on(), corrupt: on(),
		unplug: on(), readOnly: on(), catalogue: on(), strays: on(),
		restarts: on(),
	}
}

func (f faults) String() string {
	var on []string
	for name, set := range map[string]bool{
		"prowlarr-down": f.prowlarrDown, "indexer-errors": f.indexerErrors,
		"sab-down": f.sabDown, "sab-lose-answer": f.sabLoseAnswer, "sab-hide": f.sabHide, "sab-vanish": f.sabVanish, "sab-fail": f.sabFail,
		"jf-down": f.jfDown, "jf-ignore-hints": f.jfIgnoreHints, "jf-late": f.jfLate, "jf-never": f.jfNever, "jf-wrong": f.jfWrong,
		"mislabeled": f.mislabeled, "wrong-runtime": f.wrongRuntime, "corrupt": f.corrupt,
		"unplug": f.unplug, "read-only": f.readOnly, "catalogue": f.catalogue, "strays": f.strays, "restarts": f.restarts,
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
	since    time.Time // when the monitor mode was chosen
	profile  string
	added    bool // Garner has the show
	episodes []*episode
	numbered int // episode IDs handed out
}

func (s *show) dotted() string { return strings.ReplaceAll(s.name, " ", ".") }

// garnerName is the show's part of the names Garner gives folders and
// files.
func (s *show) garnerName() string { return fmt.Sprintf("%s.%d", s.dotted(), s.year) }

type episode struct {
	id             int64
	show           *show
	season, number int
	aired          time.Time
	runtime        int // minutes
}

// posting is a release: on the indexers, or the one a file in the library
// came from.
type posting struct {
	id        int
	title     string
	show      *show
	season    int
	episodes  []*episode // one, or a season's for a pack
	pack      bool
	extra     bool  // a pack also holds an extra that names no episode
	indexers  []int // empty: on no indexer
	posted    time.Time
	languages []string // as the indexers report them
	size      int64
	audio     []string // what FFprobe finds
	height    int
	longer    int  // the files run this many times their episodes' length
	corrupt   bool // FFprobe cannot read the files
}

// video is what a simulated video file holds: who it is, what it holds and
// what FFprobe finds in it. The truth travels with the file.
type video struct {
	ID       string   `json:"id"`
	Posting  int      `json:"posting"`
	Episodes []int64  `json:"episodes,omitempty"`
	Extra    bool     `json:"extra,omitempty"` // names no episode
	Audio    []string `json:"audio"`           // FFprobe's language tags
	Height   int      `json:"height"`
	Minutes  int      `json:"minutes"`
}

var showNames = []string{"Harbor Precinct", "Quiet Harbour", "Lanterns and Tides", "Copper Valley", "Night Ferry", "Glass Orchard"}

var groups = []string{"GRP", "WEBTEAM", "TIDE", "ORCHARD", "FERRY"}

type kind struct {
	tags      string
	languages []string
	audio     []string
	height    int
}

var kinds = []kind{
	{"German.DL.1080p.WEB.h264", []string{"German", "English"}, []string{"ger", "eng"}, 1080},
	{"German.720p.WEB.h264", []string{"German"}, []string{"ger"}, 720},
	{"German.DL.720p.WEB.h264", []string{"German", "English"}, []string{"ger", "eng"}, 720},
	{"1080p.WEB.h264", nil, []string{"eng"}, 1080},
}

// newWorld builds the share, the shows, their releases and the files the
// library already holds.
func newWorld(seed uint64, root string) (*world, error) {
	w := &world{seed: seed, root: root, now: start, rnd: rand.New(rand.NewPCG(seed, 1)),
		indexerError: map[int]bool{}, foreign: map[string]foreignFile{}}
	w.faults = randomFaults(rand.New(rand.NewPCG(seed, 2)))
	w.mapped = w.rnd.IntN(2) == 0
	w.disks = []string{"", "", "usenet", "media-old"}[w.rnd.IntN(4)]
	w.jf.items, w.jf.folders = map[string]*jfItem{}, map[string]int64{}
	for _, dir := range []string{"usenet/complete/tv", "usenet/incomplete", "library/tv", "library/anime", "media-old/tv", "elsewhere/notes", "unplugged", "garner"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return nil, err
		}
	}
	w.addForeign("elsewhere/holiday.mkv", `{"id":"private"}`)
	w.addForeign("elsewhere/notes/todo.txt", "buy milk")

	names := slices.Clone(showNames)
	w.rnd.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	for i, name := range names[:2+w.rnd.IntN(2)] {
		s := &show{id: int64(7001 + i), name: name, year: 2010 + w.rnd.IntN(15),
			monitor: []string{"all", "new", "off"}[w.rnd.IntN(3)], profile: "tv", added: i == 0 || w.rnd.IntN(2) == 0}
		seasons := 1 + w.rnd.IntN(2)
		// One episode a week, from 3 to 13 weeks ago: some aired long ago,
		// the last ones may still be to come.
		first := start.AddDate(0, 0, -7*(3+w.rnd.IntN(6)*seasons))
		for season := 1; season <= seasons; season++ {
			for number := 1; number <= 3+w.rnd.IntN(3); number++ {
				w.newEpisode(s, season, number, first.AddDate(0, 0, 7*len(s.episodes)))
			}
			w.postPack(s, season)
		}
		w.shows = append(w.shows, s)
		if err := w.libraryFiles(s); err != nil {
			return nil, err
		}
	}
	// A show Garner does not know, which Jellyfin holds.
	unknown := "library/tv/Unknown Harbor (2001)"
	w.jf.folders[filepath.Join(root, unknown)] = 9999
	w.addForeign(unknown+"/Season 1/Unknown Harbor - S01E01 - Arrival.mkv", `{"id":"unknown-show"}`)
	w.addForeign(unknown+"/tvshow.nfo", "<tvshow/>")
	// Jellyfin scanned all of it before the run began.
	w.jf.refresh = true
	w.scan()
	return w, nil
}

type foreignFile struct {
	content string
	size    int64
	mod     time.Time
}

// addForeign writes a file no one gives Garner and reports whether it
// could: a folder that refuses writes refuses the user too.
func (w *world) addForeign(rel, content string) bool {
	p := filepath.Join(w.root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if os.WriteFile(p, []byte(content), 0o644) != nil {
		return false
	}
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	w.foreign[p] = foreignFile{content: content, size: info.Size(), mod: info.ModTime()}
	return true
}

// newEpisode adds an episode and puts its releases on the indexers.
func (w *world) newEpisode(s *show, season, number int, aired time.Time) *episode {
	s.numbered++
	e := &episode{id: s.id*100 + int64(s.numbered), show: s, season: season, number: number, aired: aired, runtime: 22 + 23*w.rnd.IntN(2)}
	s.episodes = append(s.episodes, e)
	w.post(e)
	return e
}

// post puts one to three releases of an episode on the indexers, the
// first hours after it aired, an upgrade days later.
func (w *world) post(e *episode) {
	for i := range 1 + w.rnd.IntN(3) {
		k := kinds[w.rnd.IntN(len(kinds))]
		if i > 0 {
			k = kinds[0] // a later, better release
		}
		title := fmt.Sprintf("%s.S%02dE%02d.%s-%s", e.show.dotted(), e.season, e.number, k.tags, groups[w.rnd.IntN(len(groups))])
		posted := e.aired.Add(time.Duration(1+w.rnd.IntN(30))*time.Hour + time.Duration(i*(3+w.rnd.IntN(6)))*24*time.Hour)
		w.addPosting(&posting{title: title, show: e.show, season: e.season, episodes: []*episode{e}, posted: posted}, k, true)
	}
}

// postPack puts a season pack on the indexers some days after the season's
// last episode aired, sometimes with an extra.
func (w *world) postPack(s *show, season int) {
	var eps []*episode
	for _, e := range s.episodes {
		if e.season == season {
			eps = append(eps, e)
		}
	}
	k := kinds[w.rnd.IntN(3)]
	title := fmt.Sprintf("%s.S%02d.%s-%s", s.dotted(), season, k.tags, groups[w.rnd.IntN(len(groups))])
	p := &posting{title: title, show: s, season: season, episodes: eps, pack: true, extra: w.rnd.IntN(2) == 0,
		posted: eps[len(eps)-1].aired.AddDate(0, 0, 1+w.rnd.IntN(4))}
	w.addPosting(p, k, true)
}

// addPosting fills in a posting's kind, its faults and its indexers.
func (w *world) addPosting(p *posting, k kind, indexed bool) {
	p.id = len(w.postings) + 1
	p.languages, p.audio, p.height, p.longer = k.languages, k.audio, k.height, 1
	p.size = int64(len(p.episodes)) * int64(300+w.rnd.IntN(1500)) << 20
	if w.faults.mislabeled && w.rnd.IntN(4) == 0 {
		p.audio = []string{"eng"} // the name says German; the files have none
	}
	if w.faults.wrongRuntime && w.rnd.IntN(5) == 0 {
		p.longer = 3
	}
	p.corrupt = w.faults.corrupt && w.rnd.IntN(6) == 0
	if indexed {
		p.indexers = [][]int{{1}, {2}, {1, 2}}[w.rnd.IntN(3)]
	}
	w.postings = append(w.postings, p)
}

// libraryFiles puts files of a show's first episodes into the library
// before the run, the way an older setup left them: in Garner's own folder,
// or in the older library under other names, next to files no one gave
// Garner.
func (w *world) libraryFiles(s *show) error {
	switch w.rnd.IntN(3) {
	case 0:
		return nil
	case 1:
		// Files of releases on the indexers, named the way Garner names
		// them, as a Garner whose database was lost left them: importing
		// the same release again meets a file in the way.
		for _, e := range s.episodes[:min(2, len(s.episodes))] {
			var same []*posting
			for _, p := range w.postings {
				if !p.pack && p.episodes[0] == e && len(p.indexers) > 0 && !p.corrupt && p.longer == 1 {
					same = append(same, p)
				}
			}
			if e.aired.After(w.now) || len(same) == 0 {
				continue
			}
			p := same[w.rnd.IntN(len(same))]
			dir := filepath.Join(w.root, "library/tv", fmt.Sprintf("%s.[tvdbid-%d]", s.garnerName(), s.id), fmt.Sprintf("Season.%02d", e.season))
			if err := w.newVideo(filepath.Join(dir, garnerFileName(e, p)), p, []*episode{e}, false); err != nil {
				return err
			}
		}
	default:
		folder := filepath.Join(w.root, "media-old/tv", fmt.Sprintf("%s (%d)", s.name, s.year))
		w.jf.folders[folder] = s.id
		for _, e := range s.episodes[:min(3, len(s.episodes))] {
			if e.aired.After(w.now) {
				continue
			}
			p := &posting{title: "old", show: s, season: e.season, episodes: []*episode{e}}
			w.addPosting(p, kinds[w.rnd.IntN(2)], false)
			p.corrupt, p.longer = false, 1
			base := filepath.Join(folder, fmt.Sprintf("Season %d", e.season), fmt.Sprintf("%s - S%02dE%02d - Part %d", s.name, e.season, e.number, e.number))
			if err := w.newVideo(base+".mkv", p, []*episode{e}, false); err != nil {
				return err
			}
			if w.rnd.IntN(2) == 0 {
				os.WriteFile(base+".en.srt", []byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n"), 0o644)
			}
		}
		rel, _ := filepath.Rel(w.root, folder)
		w.addForeign(rel+"/tvshow.nfo", "<tvshow/>")
		w.addForeign(rel+"/poster.jpg", "jpeg")
		w.addForeign(rel+"/Behind the Scenes.mkv", `{"id":"extra-`+strconv.FormatInt(s.id, 10)+`"}`)
	}
	return nil
}

// garnerFileName is the name Garner gives the file of a release of an
// episode in its library, such as
// "Harbor.Precinct.2013.S01E02.Part.2.WEB-1080p.DE.EN-GRP.mkv".
func garnerFileName(e *episode, p *posting) string {
	var langs []string
	for _, a := range p.audio {
		langs = append(langs, map[string]string{"ger": "DE", "eng": "EN"}[a])
	}
	group := p.title[strings.LastIndex(p.title, "-")+1:]
	return fmt.Sprintf("%s.S%02dE%02d.Part.%d.WEB-%dp.%s-%s.mkv", e.show.garnerName(), e.season, e.number, e.number, p.height, strings.Join(langs, "."), group)
}

// seedShow records a show in Garner's database the way adding it does.
func (w *world) seedShow(ctx context.Context, st *store.Store, s *show, monitor string) error {
	if err := st.SaveSeries(ctx, w.seriesData(s), true, w.now); err != nil {
		return err
	}
	return st.SetMonitor(ctx, s.id, monitor, w.now)
}

// seriesData is the show as TheTVDB tells it.
func (w *world) seriesData(s *show) store.SeriesData {
	d := store.SeriesData{Series: store.Series{
		ID: s.id, Slug: strings.ToLower(strings.ReplaceAll(s.name, " ", "-")), Name: s.name, NameEng: s.name,
		OriginalLanguage: "eng", OriginalCountry: "usa", Status: "Continuing", Year: s.year,
		DefaultOrder: "official", TVDBUpdated: "2026-01-01", Profile: s.profile,
	}}
	for _, e := range s.episodes {
		d.Episodes = append(d.Episodes, store.Episode{
			ID: e.id, Aired: e.aired.Format(time.DateOnly), Runtime: e.runtime, NameEng: fmt.Sprintf("Part %d", e.number),
			Numbers: map[string]store.Position{"official": {Season: e.season, Number: e.number}},
		})
	}
	return d
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
		cfg.Jellyfin.Paths = config.PathMap{"/data/library": filepath.Join(w.root, "library"), "/data/old": filepath.Join(w.root, "media-old")}
	}
	return cfg
}

// The mounts SABnzbd and Jellyfin see the share's folders under, when
// they see it mapped.
var mounts = map[string]string{"usenet": "/data/usenet", "library": "/data/library", "media-old": "/data/old"}

// view is where SABnzbd or Jellyfin see a file on the share; local turns
// their view back.
func (w *world) view(local string) string {
	if !w.mapped {
		return local
	}
	for top, mount := range mounts {
		if rel, err := filepath.Rel(filepath.Join(w.root, top), local); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join(mount, rel)
		}
	}
	return local
}

func (w *world) local(remote string) string {
	if !w.mapped {
		return remote
	}
	for top, mount := range mounts {
		if rel, ok := strings.CutPrefix(remote, mount); ok && (rel == "" || rel[0] == '/') {
			return filepath.Join(w.root, top, rel)
		}
	}
	return remote
}

// rename moves a file for Garner, failing as a rename does between disks.
func (w *world) rename(oldpath, newpath string) error {
	if disk := func(p string) bool { return w.disks != "" && strings.HasPrefix(w.rel(p), w.disks+"/") }; disk(oldpath) != disk(newpath) {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
	}
	return os.Rename(oldpath, newpath)
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

// present is where a file of the share is now: under the unplugged
// folder while its root is unplugged.
func (w *world) present(p string) string {
	if w.unplugged == "" {
		return p
	}
	root := filepath.Join(w.root, w.unplugged)
	if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join(w.root, "unplugged", w.unplugged, rel)
	}
	return p
}

// unplug takes a library root away, as an unmounted share or an unplugged
// disk does; plug brings it back.
func (w *world) unplug() string {
	if w.unplugged != "" {
		return ""
	}
	root := roots[w.rnd.IntN(len(roots))]
	aside := filepath.Join(w.root, "unplugged", root)
	os.MkdirAll(filepath.Dir(aside), 0o755)
	if err := os.Rename(filepath.Join(w.root, root), aside); err != nil {
		return "" // a folder that refuses writes may not move: no change
	}
	w.unplugged = root
	return root
}

func (w *world) plug() error {
	if w.unplugged == "" {
		return nil
	}
	root := filepath.Join(w.root, w.unplugged)
	if _, err := os.Lstat(root); err == nil {
		return fmt.Errorf("%s came back while unplugged", w.unplugged)
	}
	if err := os.Rename(filepath.Join(w.root, "unplugged", w.unplugged), root); err != nil {
		return err
	}
	w.unplugged = ""
	return nil
}

// lock makes a library folder refuse writes; unlock lets it write again.
// Root may write anyway, so in a container this fault changes nothing.
func (w *world) lock() string {
	if w.readOnly != "" {
		return ""
	}
	var dirs []string
	for _, root := range []string{"library/tv", "library/anime"} {
		dirs = append(dirs, filepath.Join(w.root, root))
		entries, _ := os.ReadDir(filepath.Join(w.root, root))
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(w.root, root, e.Name()))
			}
		}
	}
	dir := dirs[w.rnd.IntN(len(dirs))]
	if err := os.Chmod(dir, 0o555); err != nil {
		return ""
	}
	w.readOnly = dir
	return w.rel(dir)
}

func (w *world) unlock() {
	if w.readOnly != "" {
		os.Chmod(w.present(w.readOnly), 0o755)
		w.readOnly = ""
	}
}

// newVideo writes a video file of a posting that holds episodes.
func (w *world) newVideo(path string, p *posting, eps []*episode, extra bool) error {
	w.videos++
	v := video{ID: fmt.Sprintf("v%04d", w.videos), Posting: p.id, Extra: extra, Audio: p.audio, Height: p.height}
	for _, e := range eps {
		v.Episodes = append(v.Episodes, e.id)
		v.Minutes += e.runtime * p.longer
	}
	if extra {
		v.Minutes = 12
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if p.corrupt {
		data = append([]byte("garbage "), data...)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// readVideo reads a simulated video's identity; ok is false for anything
// else. Corrupt videos keep their identity after the junk.
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
// A query for an episode finds single episodes, a query for a season also
// the season's packs.
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
		if p.posted.After(w.now) || !slices.Contains(p.indexers, n) || p.show.id != tvdb || p.season != season {
			continue
		}
		if ep > 0 && (p.pack || p.episodes[0].number != ep) {
			continue
		}
		found = append(found, p)
	}
	found = found[min(offset, len(found)):min(offset+max(limit, 1), len(found))]
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0" xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/"><channel>`)
	for _, p := range found {
		link := fmt.Sprintf("http://%s/%d/download?id=%d&apikey=%s", prowlarrHost, n, p.id, apiKey)
		fmt.Fprintf(&b, "<item><title>%s</title><link>%s</link><size>%d</size><pubDate>%s</pubDate>", esc(p.title), esc(link), p.size, p.posted.Format(time.RFC1123Z))
		fmt.Fprintf(&b, `<newznab:attr name="tvdbid" value="%d"/>`, p.show.id)
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
					storage = w.view(j.dir)
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
// repairing and unpacking: one obfuscated video for an episode, one named
// video per episode and maybe an extra for a pack, sometimes a sample, an
// NFO and a subtitle.
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
	p := j.posting
	name := fmt.Sprintf("%08x", w.rnd.Uint32())
	if p.pack {
		tags := strings.TrimPrefix(p.title, fmt.Sprintf("%s.S%02d.", p.show.dotted(), p.season))
		for _, e := range p.episodes {
			n := fmt.Sprintf("%s.S%02dE%02d.%s.mkv", p.show.dotted(), e.season, e.number, tags)
			if err := w.newVideo(filepath.Join(dir, n), p, []*episode{e}, false); err != nil {
				return err
			}
		}
		if p.extra {
			n := fmt.Sprintf("%s.S%02d.Making.Of.%s.mkv", p.show.dotted(), p.season, tags)
			if err := w.newVideo(filepath.Join(dir, n), p, nil, true); err != nil {
				return err
			}
		}
	} else if err := w.newVideo(filepath.Join(dir, name+".mkv"), p, p.episodes, false); err != nil {
		return err
	}
	if w.rnd.IntN(2) == 0 {
		os.WriteFile(filepath.Join(dir, j.name+".sample.mkv"), []byte("sample"), 0o644)
	}
	if w.rnd.IntN(2) == 0 {
		os.WriteFile(filepath.Join(dir, j.name+".nfo"), []byte("info"), 0o644)
	}
	if !p.pack && w.rnd.IntN(3) == 0 {
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
	// folders are the show folders it identified without a TVDB ID in
	// their name, with the show they hold.
	folders map[string]int64
}

type jfItem struct {
	show    int64 // TVDB show, from the folder
	episode int64 // TVDB episode once identified; 0 before
	scans   int   // scans until identified; -1 never
	wrong   bool
}

var (
	tvdbFolder = regexp.MustCompile(`\[tvdbid-(\d+)\]`)
	sxxeyy     = regexp.MustCompile(`S(\d+)E(\d+)`)
)

// showOf is a file's show folder: the one above its season folder.
func showOf(p string) string { return filepath.Dir(filepath.Dir(p)) }

// showID is the TVDB show of a show folder, from its name or from what
// Jellyfin identified; 0 when it holds none.
func (w *world) showID(folder string) int64 {
	if m := tvdbFolder.FindStringSubmatch(filepath.Base(folder)); m != nil {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		return id
	}
	return w.jf.folders[folder]
}

// scan does what Jellyfin's scan does: it drops files that are gone, adds
// the files it was told about or, after a refresh, every file in its
// libraries, and identifies what it can.
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
		for _, root := range roots {
			filepath.WalkDir(filepath.Join(w.root, root), func(p string, e fs.DirEntry, err error) error {
				if err == nil && !e.IsDir() && strings.HasSuffix(p, ".mkv") {
					todo = append(todo, p)
				}
				return nil
			})
		}
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
		id := w.showID(showOf(p))
		if id == 0 {
			continue
		}
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
	if removed+added+identified == 0 {
		return ""
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
	s := w.show(it.show)
	if s == nil {
		return nil
	}
	for i, e := range s.episodes {
		if e.season == season && e.number == number {
			if it.wrong {
				return s.episodes[(i+1)%len(s.episodes)]
			}
			return e
		}
	}
	return nil
}

func (w *world) show(id int64) *show {
	for _, s := range w.shows {
		if s.id == id {
			return s
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
	folderID := func(folder string) string { return fmt.Sprintf("%x", w.hash(w.rel(folder))) }
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/Library/Media/Updated":
		var body struct {
			Updates []struct{ Path, UpdateType string }
		}
		json.NewDecoder(r.Body).Decode(&body)
		for _, u := range body.Updates {
			local := w.local(u.Path)
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
		var locations []string
		for _, root := range roots {
			locations = append(locations, w.view(filepath.Join(w.root, root)))
		}
		writeJSON(rw, []map[string]any{{"Name": "TV Shows", "CollectionType": "tvshows", "Locations": locations}})
	case r.URL.Path == "/Items":
		seen := map[string]bool{}
		items := []map[string]any{}
		for _, p := range sortedKeys(w.jf.items) {
			folder := showOf(p)
			if seen[folder] {
				continue
			}
			seen[folder] = true
			items = append(items, map[string]any{"Id": folderID(folder), "Name": filepath.Base(folder), "Path": w.view(folder),
				"ProviderIds": map[string]string{"Tvdb": strconv.FormatInt(w.jf.items[p].show, 10)}})
		}
		writeJSON(rw, map[string]any{"Items": items, "TotalRecordCount": len(items)})
	case strings.HasPrefix(r.URL.Path, "/Shows/") && strings.HasSuffix(r.URL.Path, "/Episodes"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/Shows/"), "/Episodes")
		items := []map[string]any{}
		for _, p := range sortedKeys(w.jf.items) {
			if folderID(showOf(p)) != id {
				continue
			}
			it := w.jf.items[p]
			item := map[string]any{"Path": w.view(p), "Name": filepath.Base(p), "ProviderIds": map[string]string{}}
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
