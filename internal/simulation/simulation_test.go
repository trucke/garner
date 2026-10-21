// Package simulation runs Garner's server against a simulated world:
// SABnzbd, Prowlarr, Jellyfin, FFprobe, the share and the clock. A seed
// drives every choice; after every step the test checks the promises
// Garner makes about the user's files. CONTRIBUTING.md explains how to
// read and replay a failure.
package simulation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/server"
	"github.com/trucke/garner/internal/store"
)

var (
	seedFlag  = flag.Uint64("seed", 0, "run only this seed")
	seedsFlag = flag.Int("seeds", 0, "run this many fresh seeds instead of the fixed ones")
	stepsFlag = flag.Int("steps", 300, "random steps per seed, before the calm period")
)

// fixedSeeds run with every go test. A seed that once failed joins them,
// next to a focused test for its bug: after later changes, the seed may
// take another path.
var fixedSeeds = []uint64{1, 2, 3, 4, 1792577795629604037}

// TestSimulation runs each seed twice: the rules must hold in the first
// run, and the second must tell the same story.
func TestSimulation(t *testing.T) {
	seeds := fixedSeeds
	switch {
	case *seedFlag != 0:
		seeds = []uint64{*seedFlag}
	case *seedsFlag > 0:
		base := uint64(time.Now().UnixNano())
		t.Logf("fresh seeds from %d", base)
		seeds = nil
		for i := range *seedsFlag {
			seeds = append(seeds, base+uint64(i))
		}
	}
	for _, seed := range seeds {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			first := run(t, seed, *stepsFlag)
			if *seedFlag != 0 && testing.Verbose() {
				t.Log("\n" + strings.Join(first.trace, "\n"))
			}
			if first.failure != "" {
				t.Fatal(first.failure)
			}
			second := run(t, seed, *stepsFlag)
			if second.failure != "" {
				t.Fatalf("seed %d failed only on its second run, so it is not deterministic:\n%s", seed, second.failure)
			}
			if d := difference(first.trace, second.trace); d != "" {
				t.Fatalf("seed %d is not deterministic; its runs first differ at\n%s", seed, d)
			}
		})
	}
}

type outcome struct {
	trace   []string
	failure string
}

// sim is one run: the world, Garner on top of it and the story so far.
type sim struct {
	ctx   context.Context
	seed  uint64
	steps int
	w     *world
	cfg   *config.Config
	st    *store.Store
	srv   *server.Server
	api   http.Handler
	token string
	pick  *rand.Rand // the next event
	user  *rand.Rand // the user's choices
	trace []string
	step  int
	// deleting is the show the user deleted with its files in this step.
	deleting int64
	// retrying is the download the user retries in this step.
	retrying int64
	failure  string
	sentinel string
	// last is the snapshot after the last step: nothing changes between
	// steps, so it is the next step's before.
	last *snapshot
}

func run(t *testing.T, seed uint64, steps int) outcome {
	t.Helper()
	w, err := newWorld(seed, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &sim{ctx: context.Background(), seed: seed, steps: steps, w: w, cfg: w.config(),
		pick: rand.New(rand.NewPCG(seed, 3)), user: rand.New(rand.NewPCG(seed, 4))}
	s.sentinel = s.digest(filepath.Join(w.root, "elsewhere"))
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	defer func() { s.st.Close() }()
	if err := w.seedStore(s.ctx, s.st); err != nil {
		t.Fatal(err)
	}
	if s.token, err = s.st.CreateToken(s.ctx, "sim"); err != nil {
		t.Fatal(err)
	}
	s.note("setup", fmt.Sprintf("paths mapped %v; faults: %s", w.mapped, w.faults))
	for _, sh := range w.shows {
		s.note("setup", fmt.Sprintf("show %d %q, monitor %s, %d episodes from %s", sh.id, sh.name, sh.monitor, len(sh.episodes), sh.episodes[0].aired.Format(time.DateOnly)))
	}

	events := s.events()
	total := 0
	for _, e := range events {
		total += e.weight
	}
	for s.step < steps && s.failure == "" {
		n := s.pick.IntN(total)
		for _, e := range events {
			if n -= e.weight; n < 0 {
				s.do(e.actor, e.name, e.run)
				break
			}
		}
	}
	if s.failure == "" {
		s.calm()
	}
	s.dump()
	return outcome{trace: s.trace, failure: s.failure}
}

// start opens the database and sets Garner up on the world, as a restart
// does.
func (s *sim) start() error {
	st, err := store.Open(s.ctx, s.cfg.DataDir, slog.New(slog.DiscardHandler))
	if err != nil {
		return err
	}
	deps := server.Deps{
		Now:       func() time.Time { return s.w.now },
		Sleep:     func(_ context.Context, d time.Duration) error { s.w.now = s.w.now.Add(d); return nil },
		Transport: transport{s.w},
		Prober:    prober{},
	}
	srv, err := server.New(s.cfg, st, "sim", deps, slog.New(slog.DiscardHandler))
	if err != nil {
		st.Close()
		return err
	}
	s.st, s.srv, s.api = st, srv, srv.Routes()
	return nil
}

type event struct {
	actor, name string
	weight      int
	run         func() string // what happened, for the trace
}

// events are what can happen next, with their weights. Faults the seed
// did not turn on never happen.
func (s *sim) events() []event {
	w, f := s.w, s.w.faults
	events := []event{
		{"garner", "poll downloads", 16, func() string { s.srv.PollDownloads(s.ctx); return "" }},
		{"garner", "import step", 16, func() string { s.srv.WorkImports(s.ctx); return "" }},
		{"garner", "monitor step", 6, func() string { s.srv.StepMonitor(s.ctx); return "" }},
		{"user", "search episode", 4, s.searchEpisode},
		{"user", "download candidate", 6, s.downloadCandidate},
		{"user", "download best", 2, s.downloadBest},
		{"user", "search show", 2, s.searchShow},
		{"user", "cancel", 2, s.cancel},
		{"user", "retry import", 4, s.retry},
		{"user", "delete show", 1, s.deleteShow},
		{"user", "add show again", 2, s.addShow},
		{"world", "sabnzbd progress", 24, s.sabProgress},
		{"world", "jellyfin scans", 10, w.scan},
		{"world", "clock", 16, s.clock},
	}
	// An outage lasts until the clock moves on.
	outage := func(name string, down *bool) event {
		return event{"world", name, 2, func() string { *down = true; return "" }}
	}
	if f.prowlarrDown {
		events = append(events, outage("prowlarr down", &w.prowlarrDown))
	}
	if f.indexerErrors {
		events = append(events, event{"world", "indexer error", 1, func() string {
			n := 1 + w.rnd.IntN(2)
			w.indexerError[n] = !w.indexerError[n]
			return fmt.Sprintf("indexer %d: %v", n, w.indexerError[n])
		}})
	}
	if f.sabDown {
		events = append(events, outage("sabnzbd down", &w.sab.down))
	}
	if f.sabLoseAnswer {
		events = append(events, event{"world", "sabnzbd loses answer", 1, func() string { w.sab.loseNext = true; return "next addurl" }})
	}
	if f.sabHide {
		events = append(events, event{"world", "sabnzbd hides job", 1, func() string { w.sab.hideNext = true; return "next job" }})
	}
	if f.sabVanish {
		events = append(events, event{"world", "sabnzbd job vanishes", 1, func() string {
			if j := s.anyJob((*job).active); j != nil {
				j.gone, j.vanished = true, true
				return j.id
			}
			return ""
		}})
	}
	if f.jfDown {
		events = append(events, outage("jellyfin down", &w.jf.down))
	}
	if f.restarts {
		events = append(events, event{"process", "restart", 2, s.restart})
	}
	return events
}

// do runs one step and checks the rules about it.
func (s *sim) do(actor, name string, f func() string) {
	if s.failure != "" {
		return
	}
	s.deleting, s.retrying = 0, 0
	if s.last == nil {
		sn, err := s.snapshot()
		if err != nil {
			s.fail("setup", err.Error())
			return
		}
		s.last = &sn
	}
	before := *s.last
	s.step++
	s.note(actor+" "+name, f())
	if s.failure != "" {
		return
	}
	after, err := s.snapshot()
	if err != nil {
		s.fail("setup", err.Error())
		return
	}
	s.last = &after
	for _, rule := range []func(snapshot, snapshot) (string, string){s.filesStay, s.recordsMatch, s.outsideStays, s.statesMove} {
		if id, msg := rule(before, after); msg != "" {
			s.fail(id, msg)
			return
		}
	}
}

func (s *sim) note(what, detail string) {
	line := fmt.Sprintf("%04d %-24s %s", s.step, what, detail)
	s.trace = append(s.trace, strings.TrimRight(strings.ReplaceAll(line, s.w.root, "<share>"), " "))
}

// fail records the first failure; what follows it says nothing new.
func (s *sim) fail(rule, msg string) {
	if s.failure != "" {
		return
	}
	last := s.trace[max(0, len(s.trace)-15):]
	s.failure = fmt.Sprintf("seed %d, step %d: %s broken\n  %s\nlast steps:\n  %s\nreplay: go test ./internal/simulation -run TestSimulation -seed %d -steps %d -v",
		s.seed, s.step, rule, strings.ReplaceAll(msg, s.w.root, "<share>"), strings.Join(last, "\n  "), s.seed, s.steps)
}

// restart throws Garner away and sets it up again from the database.
func (s *sim) restart() string {
	if err := s.st.Close(); err != nil {
		return "close: " + err.Error()
	}
	if err := s.start(); err != nil {
		s.fail("setup", "restart: "+err.Error())
	}
	return ""
}

// clock moves time on: mostly minutes, sometimes hours, rarely days.
func (s *sim) clock() string {
	var d time.Duration
	switch n := s.w.rnd.IntN(20); {
	case n < 14:
		d = time.Duration(1+s.w.rnd.IntN(10)) * time.Minute
	case n < 19:
		d = time.Duration(1+s.w.rnd.IntN(12)) * time.Hour
	default:
		d = time.Duration(1+s.w.rnd.IntN(3)) * 24 * time.Hour
	}
	s.w.now = s.w.now.Add(d)
	s.w.prowlarrDown, s.w.sab.down, s.w.jf.down = false, false, false
	clear(s.w.indexerError)
	return fmt.Sprintf("+%s → %s", d, s.w.now.Format(time.DateTime))
}

func (s *sim) anyJob(want func(*job) bool) *job {
	var jobs []*job
	for _, j := range s.w.sab.jobs {
		if want(j) {
			jobs = append(jobs, j)
		}
	}
	if len(jobs) == 0 {
		return nil
	}
	return jobs[s.w.rnd.IntN(len(jobs))]
}

func (s *sim) sabProgress() string {
	j := s.anyJob((*job).active)
	if j == nil {
		return ""
	}
	detail, err := s.w.progress(j)
	if err != nil {
		s.fail("setup", err.Error())
	}
	return detail
}

// call sends a request to Garner's API as the terminal interface does.
func (s *sim) call(method, path string) (int, []byte) {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+s.token)
	rec := httptest.NewRecorder()
	s.api.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func answer(code int, body []byte) string {
	if code < 300 {
		return fmt.Sprint(code)
	}
	var e struct{ Error string }
	json.Unmarshal(body, &e)
	return fmt.Sprintf("%d %s", code, e.Error)
}

// someEpisode is an aired episode of a show Garner still has.
func (s *sim) someEpisode() *episode {
	var aired []*episode
	for _, sh := range s.w.shows {
		for _, e := range sh.episodes {
			if !sh.deleted && !e.aired.After(s.w.now) {
				aired = append(aired, e)
			}
		}
	}
	if len(aired) == 0 {
		return nil
	}
	return aired[s.user.IntN(len(aired))]
}

type searchResult struct {
	Outcome    string `json:"outcome"`
	Candidates []struct {
		ID      int64  `json:"id"`
		Verdict string `json:"verdict"`
	} `json:"candidates"`
}

// search searches an episode as the terminal interface does.
func (s *sim) search(e *episode) (searchResult, string) {
	var sr searchResult
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/episodes/%d/search", e.id))
	if code != http.StatusOK {
		return sr, fmt.Sprintf("episode %d: %s", e.id, answer(code, body))
	}
	json.Unmarshal(body, &sr)
	accepted := 0
	for _, c := range sr.Candidates {
		if c.Verdict == "accept" {
			accepted++
		}
	}
	return sr, fmt.Sprintf("episode %d: %s, %d candidates, %d accepted", e.id, sr.Outcome, len(sr.Candidates), accepted)
}

func (s *sim) searchEpisode() string {
	e := s.someEpisode()
	if e == nil {
		return ""
	}
	_, detail := s.search(e)
	return detail
}

func (s *sim) downloadCandidate() string {
	e := s.someEpisode()
	if e == nil {
		return ""
	}
	sr, detail := s.search(e)
	if len(sr.Candidates) == 0 {
		return detail
	}
	c := sr.Candidates[0]
	if s.user.IntN(3) == 0 {
		c = sr.Candidates[s.user.IntN(len(sr.Candidates))]
	}
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/candidates/%d/download", c.ID))
	return fmt.Sprintf("%s; candidate %d (%s): %s", detail, c.ID, c.Verdict, answer(code, body))
}

func (s *sim) downloadBest() string {
	e := s.someEpisode()
	if e == nil {
		return ""
	}
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/episodes/%d/download", e.id))
	return fmt.Sprintf("episode %d: %s", e.id, answer(code, body))
}

func (s *sim) someShow() *show {
	var shows []*show
	for _, sh := range s.w.shows {
		if !sh.deleted {
			shows = append(shows, sh)
		}
	}
	if len(shows) == 0 {
		return nil
	}
	return shows[s.user.IntN(len(shows))]
}

func (s *sim) searchShow() string {
	sh := s.someShow()
	if sh == nil {
		return ""
	}
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/series/%d/search", sh.id))
	return fmt.Sprintf("show %d: %s", sh.id, answer(code, body))
}

func (s *sim) someDownload(want func(store.Download) bool) *store.Download {
	all, err := s.st.Downloads(s.ctx, 100000)
	if err != nil {
		return nil
	}
	all = slices.DeleteFunc(all, func(d store.Download) bool { return !want(d) })
	if len(all) == 0 {
		return nil
	}
	slices.SortFunc(all, func(a, b store.Download) int { return int(a.ID - b.ID) })
	return &all[s.user.IntN(len(all))]
}

func (s *sim) cancel() string {
	d := s.someDownload(store.Download.Active)
	if d == nil {
		return ""
	}
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/downloads/%d/cancel", d.ID))
	return fmt.Sprintf("download %d: %s", d.ID, answer(code, body))
}

func (s *sim) retry() string {
	d := s.someDownload(func(d store.Download) bool {
		return d.Import.State == store.ImportFailed || d.Import.State == store.ImportUnverified
	})
	if d == nil {
		return ""
	}
	s.retrying = d.ID
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/downloads/%d/import", d.ID))
	return fmt.Sprintf("download %d (%s): %s", d.ID, d.Import.State, answer(code, body))
}

func (s *sim) deleteShow() string {
	sh := s.someShow()
	if sh == nil {
		return ""
	}
	files := []string{"keep", "delete"}[s.user.IntN(2)]
	if files == "delete" {
		s.deleting = sh.id
	}
	code, body := s.call(http.MethodDelete, fmt.Sprintf("/api/v1/series/%d?files=%s", sh.id, files))
	if code == http.StatusOK {
		sh.deleted = true
	} else {
		s.deleting = 0 // refused: nothing of the show may go
	}
	return fmt.Sprintf("show %d, files=%s: %s", sh.id, files, answer(code, body))
}

// addShow adds a deleted show back. The catalogue is not simulated yet, so
// the show goes into the database the way adding it would put it there,
// without a take-over.
func (s *sim) addShow() string {
	var deleted []*show
	for _, sh := range s.w.shows {
		if sh.deleted {
			deleted = append(deleted, sh)
		}
	}
	if len(deleted) == 0 {
		return ""
	}
	sh := deleted[s.user.IntN(len(deleted))]
	if err := s.w.seedShow(s.ctx, s.st, sh); err != nil {
		s.fail("setup", err.Error())
		return ""
	}
	sh.deleted = false
	return fmt.Sprintf("show %d, monitor %s", sh.id, sh.monitor)
}

// snapshot is what the rules compare before and after a step.
type snapshot struct {
	downloads map[int64]store.Download
	records   map[int64]string  // the recorded file, by episode
	library   map[string]string // the file of each video in the library, by video
	dupes     []string
	sentinel  string
	top       []string        // what the share holds at its top
	jobs      map[string]bool // SABnzbd's job folders that exist, by job
}

func (s *sim) snapshot() (snapshot, error) {
	sn := snapshot{downloads: map[int64]store.Download{}, records: map[int64]string{}, library: map[string]string{}, jobs: map[string]bool{}}
	for _, j := range s.w.sab.jobs {
		if _, err := os.Stat(j.dir); j.dir != "" && !j.lost && err == nil {
			sn.jobs[j.id] = true
		}
	}
	all, err := s.st.Downloads(s.ctx, 100000)
	if err != nil {
		return sn, err
	}
	for _, d := range all {
		sn.downloads[d.ID] = d
	}
	for _, sh := range s.w.shows {
		for _, e := range sh.episodes {
			f, found, err := s.st.EpisodeFile(s.ctx, e.id)
			if err != nil {
				return sn, err
			}
			if found {
				sn.records[e.id] = f.Path
			}
		}
	}
	err = filepath.WalkDir(filepath.Join(s.w.root, "library"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if v, ok := readVideo(p); ok {
			if other, dup := sn.library[v.ID]; dup {
				sn.dupes = append(sn.dupes, fmt.Sprintf("%s at %s and %s", v.ID, other, p))
			}
			sn.library[v.ID] = p
		}
		return nil
	})
	if err != nil {
		return sn, err
	}
	sn.sentinel = s.digest(filepath.Join(s.w.root, "elsewhere"))
	entries, err := os.ReadDir(s.w.root)
	for _, e := range entries {
		sn.top = append(sn.top, e.Name())
	}
	for _, root := range []string{"library/tv", "library/anime"} {
		if st, err := os.Stat(filepath.Join(s.w.root, root)); err != nil || !st.IsDir() {
			sn.top = append(sn.top, "missing "+root)
		}
	}
	return sn, err
}

// digest sums a folder's names and contents.
func (s *sim) digest(dir string) string {
	h := sha256.New()
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		fmt.Fprintln(h, s.w.rel(p), err)
		if err == nil && !d.IsDir() {
			data, _ := os.ReadFile(p)
			h.Write(data)
		}
		return nil
	})
	return fmt.Sprintf("%x", h.Sum(nil))
}

// filesStay is S2 and S3: a video leaves the library only when a newer
// file of its episodes replaced it, or when the user deleted its show with
// its files; Garner copies nothing and overwrites nothing else.
func (s *sim) filesStay(before, after snapshot) (string, string) {
	if len(after.dupes) > 0 {
		return "S3", "a video is in the library twice: " + after.dupes[0]
	}
	for _, id := range sortedKeys(before.library) {
		if _, ok := after.library[id]; ok {
			continue
		}
		path := before.library[id]
		var episodes []int64
		for _, e := range sortedKeys64(before.records) {
			if before.records[e] == path {
				episodes = append(episodes, e)
			}
		}
		if len(episodes) == 0 {
			return "S2", fmt.Sprintf("video %s at %s left the library, and no episode recorded it", id, path)
		}
		if s.deleting != 0 && s.w.episode(episodes[0]).show.id == s.deleting {
			continue
		}
		for _, e := range episodes {
			p, ok := after.records[e]
			if v, read := readVideo(p); !ok || !read || v.ID == id {
				return "S2", fmt.Sprintf("video %s of episode %d left the library from %s, and no other file replaced it", id, e, path)
			}
		}
	}
	return "", ""
}

// recordsMatch is S4: every recorded file exists.
func (s *sim) recordsMatch(_, after snapshot) (string, string) {
	for _, e := range sortedKeys64(after.records) {
		if _, err := os.Stat(after.records[e]); err != nil {
			return "S4", fmt.Sprintf("episode %d records %s, which is gone", e, after.records[e])
		}
	}
	return "", ""
}

// outsideStays is S5: nothing outside the library, SABnzbd's folders and
// Garner's data changes, and the library roots stay.
func (s *sim) outsideStays(before, after snapshot) (string, string) {
	if after.sentinel != s.sentinel {
		return "S5", "the folder elsewhere changed"
	}
	if !slices.Equal(before.top, after.top) {
		return "S5", fmt.Sprintf("the share's top changed from %v to %v", before.top, after.top)
	}
	// A job folder goes when someone removes the job in SABnzbd by hand,
	// when Garner is done with its download (imported or rejected), when
	// Garner cancelled the download and SABnzbd deleted its files, or with
	// its show.
	done := []string{store.ImportScanning, store.ImportDone, store.ImportUnverified, store.ImportRejected}
	for _, j := range s.w.sab.jobs {
		if !before.jobs[j.id] || after.jobs[j.id] {
			continue
		}
		j.lost = true
		d, ok := downloadOfJob(after, j.id)
		switch {
		case j.vanished:
		case !ok && j.posting.episode.show.deleted:
		case ok && j.gone && d.State == store.Cancelled:
		case ok && !j.gone && slices.Contains(done, d.Import.State):
		default:
			return "S5", fmt.Sprintf("SABnzbd's folder %s of %s went, but Garner was not done with its download (%s, import %q)", j.dir, j.id, d.State, d.Import.State)
		}
	}
	return "", ""
}

func downloadOfJob(sn snapshot, jobID string) (store.Download, bool) {
	for _, id := range sortedKeys64(sn.downloads) {
		if d := sn.downloads[id]; d.JobID == jobID {
			return d, true
		}
	}
	return store.Download{}, false
}

// The transitions downloads and imports may take in a step, step by step.
var (
	downloadNext = map[string][]string{
		store.Submitting:  {store.Queued, store.Downloading, store.Processing, store.Completed, store.Failed, store.Cancelled},
		store.Queued:      {store.Downloading, store.Processing, store.Completed, store.Failed, store.Cancelled},
		store.Downloading: {store.Queued, store.Processing, store.Completed, store.Failed, store.Cancelled},
		store.Processing:  {store.Completed, store.Failed, store.Cancelled},
		store.Completed:   {store.Failed}, // the file check rejected the release
	}
	importNext = map[string][]string{
		"":                     {store.ImportPending},
		store.ImportPending:    {store.ImportMoving, store.ImportRejected, store.ImportFailed},
		store.ImportMoving:     {store.ImportScanning, store.ImportDone, store.ImportUnverified, store.ImportFailed},
		store.ImportScanning:   {store.ImportDone, store.ImportUnverified},
		store.ImportFailed:     nil,
		store.ImportRejected:   nil,
		store.ImportDone:       nil,
		store.ImportUnverified: nil,
	}
	// retryNext are the transitions only Retry makes.
	retryNext = map[string][]string{
		store.ImportFailed:     {store.ImportPending, store.ImportMoving},
		store.ImportUnverified: {store.ImportScanning},
	}
	firstStates = []string{store.Submitting, store.Queued, store.Failed}
)

// statesMove is S6: downloads and imports change state only along the
// allowed transitions, and only a deleted show's downloads disappear.
func (s *sim) statesMove(before, after snapshot) (string, string) {
	for _, id := range sortedKeys64(before.downloads) {
		b := before.downloads[id]
		a, ok := after.downloads[id]
		if !ok {
			if !s.showDeleted(b.EpisodeID) {
				return "S6", fmt.Sprintf("download %d disappeared, and its show is still there", id)
			}
			continue
		}
		if !reachable(downloadNext, b.State, a.State) {
			return "S6", fmt.Sprintf("download %d went from %s to %s", id, b.State, a.State)
		}
		graph := importNext
		if id == s.retrying {
			graph = merge(importNext, retryNext)
		}
		if !reachable(graph, b.Import.State, a.Import.State) {
			return "S6", fmt.Sprintf("the import of download %d went from %q to %q", id, b.Import.State, a.Import.State)
		}
	}
	for _, id := range sortedKeys64(after.downloads) {
		if _, ok := before.downloads[id]; !ok && !slices.Contains(firstStates, after.downloads[id].State) {
			return "S6", fmt.Sprintf("download %d started as %s", id, after.downloads[id].State)
		}
	}
	return "", ""
}

func (s *sim) showDeleted(episodeID int64) bool {
	e := s.w.episode(episodeID)
	return e != nil && e.show.deleted
}

func reachable(graph map[string][]string, from, to string) bool {
	seen := map[string]bool{from: true}
	for queue := []string{from}; len(queue) > 0; queue = queue[1:] {
		if queue[0] == to {
			return true
		}
		for _, next := range graph[queue[0]] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

func merge(a, b map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range a {
		out[k] = slices.Clone(v)
	}
	for k, v := range b {
		out[k] = append(out[k], v...)
	}
	return out
}

// calm makes the world healthy and lets the clock run until nothing is
// left to do, then checks L1 and L2: every download and every import ended.
func (s *sim) calm() {
	w := s.w
	w.prowlarrDown, w.sab.down, w.jf.down, w.sab.loseNext, w.sab.hideNext = false, false, false, false, false
	clear(w.indexerError)
	s.note("calm", "the world turns healthy")
	quiet := 0
	for round := 0; round < 400 && quiet < 3 && s.failure == ""; round++ {
		s.do("world", "sabnzbd finishes", func() string {
			var done []string
			for _, j := range w.sab.jobs {
				if j.active() {
					j.status, j.percent = "Downloading", 100
					if err := w.complete(j); err != nil {
						s.fail("setup", err.Error())
					}
					done = append(done, j.id)
				}
			}
			return strings.Join(done, " ")
		})
		s.do("garner", "poll downloads", func() string { s.srv.PollDownloads(s.ctx); return "" })
		s.do("garner", "import step", func() string { s.srv.WorkImports(s.ctx); return "" })
		s.do("world", "jellyfin scans", w.scan)
		s.do("world", "clock", func() string { w.now = w.now.Add(time.Minute); return "+1m" })
		if s.settled() {
			quiet++
		} else {
			quiet = 0
		}
	}
	if s.failure != "" {
		return
	}
	all, err := s.st.Downloads(s.ctx, 100000)
	if err != nil {
		s.fail("setup", err.Error())
		return
	}
	slices.SortFunc(all, func(a, b store.Download) int { return int(a.ID - b.ID) })
	for _, d := range all {
		switch {
		case d.Active():
			s.fail("L1", fmt.Sprintf("download %d is still %s after the calm period", d.ID, d.State))
		case d.State == store.Completed && !slices.Contains([]string{store.ImportDone, store.ImportUnverified, store.ImportRejected, store.ImportFailed}, d.Import.State):
			s.fail("L2", fmt.Sprintf("the import of download %d is still %q after the calm period", d.ID, d.Import.State))
		case d.Import.State == store.ImportFailed && d.Import.Recovery == "":
			s.fail("L2", fmt.Sprintf("the import of download %d failed without a recovery step: %s", d.ID, d.Import.Detail))
		}
		if s.failure != "" {
			return
		}
	}
}

// settled reports whether nothing runs: no download and no import.
func (s *sim) settled() bool {
	all, err := s.st.Downloads(s.ctx, 100000)
	if err != nil {
		return false
	}
	for _, d := range all {
		if d.Active() || d.Import.State == store.ImportPending || d.Import.State == store.ImportMoving || d.Import.State == store.ImportScanning {
			return false
		}
	}
	return true
}

// dump ends the trace with the state of things, so two runs of a seed
// compare more than their steps.
func (s *sim) dump() {
	all, _ := s.st.Downloads(s.ctx, 100000)
	slices.SortFunc(all, func(a, b store.Download) int { return int(a.ID - b.ID) })
	for _, d := range all {
		s.note("end download", fmt.Sprintf("%d %s %s %s/%s %s", d.ID, d.JobID, d.Title, d.State, d.Import.State, d.Import.Detail))
	}
	for _, j := range s.w.sab.jobs {
		s.note("end job", fmt.Sprintf("%s %s %s gone=%v hidden=%v %s", j.id, j.name, j.status, j.gone, j.hidden, s.w.rel(j.dir)))
	}
	filepath.WalkDir(s.w.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == s.w.root {
			return err
		}
		if d.IsDir() && d.Name() == "garner" {
			return fs.SkipDir
		}
		if !d.IsDir() {
			v, _ := readVideo(p)
			s.note("end file", s.w.rel(p)+" "+v.ID)
		}
		return nil
	})
}

// difference is the first line where two traces differ, with its
// neighbours.
func difference(a, b []string) string {
	for i := range max(len(a), len(b)) {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return fmt.Sprintf("  first run:  %s\n  second run: %s", x, y)
		}
	}
	return ""
}

func sortedKeys64[V any](m map[int64]V) []int64 {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
