// Package simulation runs Garner's server against a simulated world:
// SABnzbd, Prowlarr, Jellyfin, FFprobe, the share and the clock. A seed
// drives every choice; after every step the test checks the promises
// Garner makes about the user's files. CONTRIBUTING.md explains how to
// read and replay a failure.
package simulation

import (
	"cmp"
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
	"github.com/trucke/garner/internal/importer/guard"
	"github.com/trucke/garner/internal/server"
	"github.com/trucke/garner/internal/store"
)

var (
	seedFlag  = flag.Uint64("seed", 0, "run only this seed")
	seedsFlag = flag.Int("seeds", 0, "run this many fresh seeds instead of the fixed ones")
	stepsFlag = flag.Int("steps", defaultSteps, "random steps per seed, before the calm period")
)

const defaultSteps = 300

// fixedSeeds run with every go test. Together, every event changes
// something in them (a refused request does not count), they cover both
// path setups and the three disk setups, and they catch the bugs built
// into Garner on purpose to test the simulation: overwriting a file
// Garner does not own, removing SABnzbd's whole folder, never giving up
// on Jellyfin and downloading again while an import waits for the user.
//
// A change to the world sends every seed down another path, and
// TestSimulation then fails when an event or a setup went missing. To
// choose again, swap in other seeds until it passes; -seed N -v shows
// what a seed does. Whether they still catch the bugs takes building them
// in again by hand. A bug a seed found gets a focused test of its own.
var fixedSeeds = []uint64{41, 69, 104}

// TestSimulation runs each seed twice: the rules must hold in the first
// run, and the second must tell the same story. The fixed seeds, all run
// with the default steps, must also cover every event and setup.
func TestSimulation(t *testing.T) {
	seeds := fixedSeeds
	fixed := *seedFlag == 0 && *seedsFlag <= 0 && *stepsFlag == defaultSteps
	covered, ran := map[string]bool{}, 0
	var all []string
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
			ran++
			all = first.all
			for _, what := range first.covered {
				covered[what] = true
			}
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
	// Only a full run of the fixed seeds tells; -run may pick some of them.
	if !fixed || ran != len(fixedSeeds) || t.Failed() {
		return
	}
	var missing []string
	for _, what := range all {
		if !covered[what] {
			missing = append(missing, what)
		}
	}
	if len(missing) > 0 {
		t.Errorf("in no fixed seed did these change anything: %s\nchoose the fixed seeds again as the comment on fixedSeeds says", strings.Join(missing, "; "))
	}
}

type outcome struct {
	trace   []string
	failure string
	// covered are the events that changed something and the setup, all
	// the events and setups there are.
	covered, all []string
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
	// removed is the episode the catalogue dropped in this step.
	removed int64
	// refused is whether Garner refused the step's last request.
	refused  bool
	covered  map[string]bool
	failure  string
	sentinel string
	// last is the snapshot after the last step: nothing changes between
	// steps, so it is the next step's before.
	last *snapshot
	// videos caches the videos read, by path; a file whose size and
	// modification time stay is not read again.
	videos map[string]cachedVideo
}

type cachedVideo struct {
	size int64
	mod  time.Time
	v    video
	ok   bool
}

// video reads the video at p, from the cache when the file did not change.
func (s *sim) video(p string, d fs.DirEntry) (video, bool) {
	info, err := d.Info()
	if err != nil {
		return video{}, false
	}
	if c, hit := s.videos[p]; hit && c.size == info.Size() && c.mod.Equal(info.ModTime()) {
		return c.v, c.ok
	}
	v, ok := readVideo(p)
	s.videos[p] = cachedVideo{size: info.Size(), mod: info.ModTime(), v: v, ok: ok}
	return v, ok
}

func run(t *testing.T, seed uint64, steps int) outcome {
	t.Helper()
	w, err := newWorld(seed, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The temporary folder can only go once every folder takes writes.
	defer w.unlock()
	s := &sim{ctx: context.Background(), seed: seed, steps: steps, w: w, cfg: w.config(),
		pick: rand.New(rand.NewPCG(seed, 3)), user: rand.New(rand.NewPCG(seed, 4)), videos: map[string]cachedVideo{},
		covered: map[string]bool{}}
	s.sentinel = s.digest(filepath.Join(w.root, "elsewhere"))
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	defer func() { s.st.Close() }()
	for _, sh := range w.shows {
		if sh.added {
			if err := w.seedShow(s.ctx, s.st, sh, sh.monitor); err != nil {
				t.Fatal(err)
			}
			sh.since = w.now
		}
	}
	if s.token, err = s.st.CreateToken(s.ctx, "sim"); err != nil {
		t.Fatal(err)
	}
	s.note("setup", fmt.Sprintf("paths mapped %v, other disk %q; faults: %s", w.mapped, w.disks, w.faults))
	for _, sh := range w.shows {
		s.note("setup", fmt.Sprintf("show %d %q, added %v, monitor %s, %d episodes from %s", sh.id, sh.name, sh.added, sh.monitor, len(sh.episodes), sh.episodes[0].aired.Format(time.DateOnly)))
	}

	var events, all []event
	for _, e := range s.events() {
		all = append(all, e)
		if e.on {
			events = append(events, e)
		}
	}
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
	o := outcome{trace: s.trace, failure: s.failure}
	for _, e := range all {
		o.all = append(o.all, e.actor+" "+e.name)
	}
	o.all = append(o.all, setups...)
	o.covered = append(sortedKeys(s.covered), setup(w)...)
	return o
}

// setups are the ways the share can be set up: whether SABnzbd and
// Jellyfin see it under other paths, and which folder sits on another
// disk.
var setups = []string{"paths mapped", "paths as they are", "other disk none", "other disk usenet", "other disk media-old"}

func setup(w *world) []string {
	paths := "paths as they are"
	if w.mapped {
		paths = "paths mapped"
	}
	return []string{paths, "other disk " + cmp.Or(w.disks, "none")}
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
		Rename:    s.w.rename,
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
	// run does it and says what happened, for the trace: nothing when
	// nothing changed.
	run func() string
	on  bool // the event can happen: its fault is on, if it is one
}

// events are all that can happen next, with their weights. Faults the
// seed did not turn on never happen.
func (s *sim) events() []event {
	w, f := s.w, s.w.faults
	// An outage lasts until the clock moves on.
	outage := func(name string, down *bool, on bool) event {
		return event{"world", name, 2, func() string { *down = true; return "until the clock moves on" }, on}
	}
	return []event{
		{"garner", "poll downloads", 16, func() string { s.srv.PollDownloads(s.ctx); return "" }, true},
		{"garner", "import step", 16, func() string { s.srv.WorkImports(s.ctx); return "" }, true},
		{"garner", "monitor step", 6, func() string { s.srv.StepMonitor(s.ctx); return "" }, true},
		{"user", "search episode", 4, s.searchEpisode, true},
		{"user", "download candidate", 6, s.downloadCandidate, true},
		{"user", "download best", 2, s.downloadBest, true},
		{"user", "download pack", 3, s.downloadPack, true},
		{"user", "search show", 2, s.searchShow, true},
		{"user", "cancel", 2, s.cancel, true},
		{"user", "retry import", 4, s.retry, true},
		{"user", "delete show", 1, s.deleteShow, true},
		{"user", "add show", 3, s.addShow, true},
		{"user", "take over", 1, s.takeOver, true},
		{"user", "rename", 2, s.rename, true},
		{"user", "change profile", 1, s.changeProfile, true},
		{"world", "sabnzbd progress", 24, s.sabProgress, true},
		{"world", "jellyfin scans", 10, w.scan, true},
		{"world", "clock", 16, s.clock, true},
		outage("prowlarr down", &w.prowlarrDown, f.prowlarrDown),
		{"world", "indexer error", 1, func() string {
			n := 1 + w.rnd.IntN(2)
			w.indexerError[n] = true
			return fmt.Sprint("indexer ", n)
		}, f.indexerErrors},
		outage("sabnzbd down", &w.sab.down, f.sabDown),
		{"world", "sabnzbd loses answer", 1, func() string { w.sab.loseNext = true; return "next addurl" }, f.sabLoseAnswer},
		{"world", "sabnzbd hides job", 1, func() string { w.sab.hideNext = true; return "next job" }, f.sabHide},
		{"world", "sabnzbd job vanishes", 1, func() string {
			if j := s.anyJob((*job).active); j != nil {
				j.gone, j.vanished = true, true
				return j.id
			}
			return ""
		}, f.sabVanish},
		outage("jellyfin down", &w.jf.down, f.jfDown),
		{"world", "unplug", 2, w.unplug, f.unplug},
		{"world", "refuse writes", 2, w.lock, f.readOnly},
		{"world", "catalogue changes", 2, s.catalogue, f.catalogue},
		{"user", "puts a file in the way", 2, s.stray, f.strays},
		{"process", "restart", 2, s.restart, f.restarts},
	}
}

// do runs one step and checks the rules about it.
func (s *sim) do(actor, name string, f func() string) {
	if s.failure != "" {
		return
	}
	s.deleting, s.retrying, s.removed = 0, 0, 0
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
	s.refused = false
	detail := f()
	s.note(actor+" "+name, detail)
	// Garner's own steps always run; the others changed something when
	// they say what, unless Garner refused the request.
	if actor == "garner" || detail != "" && !s.refused {
		s.covered[actor+" "+name] = true
	}
	if s.failure != "" {
		return
	}
	after, err := s.snapshot()
	if err != nil {
		s.fail("setup", err.Error())
		return
	}
	s.last = &after
	for _, rule := range []func(snapshot, snapshot) (string, string){s.foreignStays, s.filesStay, s.recordsMatch, s.outsideStays, s.statesMove} {
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
	return "Garner started again"
}

// clock moves time on: mostly minutes, sometimes hours, rarely days. The
// world heals meanwhile: outages end, unplugged folders come back and
// folders take writes again.
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
	return s.advance(d)
}

func (s *sim) advance(d time.Duration) string {
	w := s.w
	w.now = w.now.Add(d)
	w.prowlarrDown, w.sab.down, w.jf.down = false, false, false
	clear(w.indexerError)
	w.unlock()
	if err := w.plug(); err != nil {
		s.fail("S5", err.Error())
	}
	return fmt.Sprintf("+%s → %s", d, w.now.Format(time.DateTime))
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

// catalogue changes a show as TheTVDB does: a new episode is announced,
// one is dropped, or two swap their numbers.
func (s *sim) catalogue() string {
	sh := s.someShow()
	if sh == nil {
		return ""
	}
	w := s.w
	var what string
	switch op := w.rnd.IntN(3); {
	case op == 0 || len(sh.episodes) < 3:
		// The next number in the last season: numbers never repeat.
		season, number := 0, 0
		for _, e := range sh.episodes {
			if e.season > season || e.season == season && e.number > number {
				season, number = e.season, e.number
			}
		}
		e := w.newEpisode(sh, season, number+1, w.now.AddDate(0, 0, 1+w.rnd.IntN(10)))
		what = fmt.Sprintf("episode %d announced", e.id)
	case op == 1:
		i := 1 + w.rnd.IntN(len(sh.episodes)-1)
		s.removed = sh.episodes[i].id
		sh.episodes = slices.Delete(sh.episodes, i, i+1)
		what = fmt.Sprintf("episode %d dropped", s.removed)
	default:
		a := sh.episodes[w.rnd.IntN(len(sh.episodes))]
		for _, b := range sh.episodes {
			if b != a && b.season == a.season {
				a.number, b.number = b.number, a.number
				what = fmt.Sprintf("episodes %d and %d swap numbers", a.id, b.id)
				break
			}
		}
	}
	if err := s.st.UpdateSeries(s.ctx, w.seriesData(sh), true, w.now); err != nil {
		s.fail("setup", err.Error())
	}
	if what == "" {
		return "" // no other episode in the season to swap with: no change
	}
	return fmt.Sprintf("show %d: %s", sh.id, what)
}

// stray puts a file of the user's own where a failed import will move one
// when it is retried: Garner must not overwrite it.
func (s *sim) stray() string {
	d := s.someDownload(func(d store.Download) bool {
		return d.Import.State == store.ImportFailed && len(d.Import.Files) > 0 && d.Import.Files[0].Target != ""
	})
	if d == nil {
		return ""
	}
	target := s.w.present(d.Import.Files[0].Target)
	if _, err := os.Lstat(target); err == nil {
		return ""
	}
	rel, err := filepath.Rel(s.w.root, d.Import.Files[0].Target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	if s.w.unplugged != "" && strings.HasPrefix(rel, s.w.unplugged+"/") {
		return "" // the user cannot reach it while it is unplugged
	}
	if !s.w.addForeign(rel, fmt.Sprintf(`{"id":"stray-%d"}`, d.ID)) {
		return ""
	}
	return fmt.Sprintf("download %d: %s", d.ID, rel)
}

// call sends a request to Garner's API as the terminal interface does.
func (s *sim) call(method, path, body string) (int, []byte) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.token)
	rec := httptest.NewRecorder()
	s.api.ServeHTTP(rec, req)
	s.refused = rec.Code >= http.StatusBadRequest
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

// someEpisode is an aired episode of a show Garner has.
func (s *sim) someEpisode() *episode {
	var aired []*episode
	for _, sh := range s.w.shows {
		for _, e := range sh.episodes {
			if sh.added && !e.aired.After(s.w.now) {
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

func (sr searchResult) String() string {
	accepted := 0
	for _, c := range sr.Candidates {
		if c.Verdict == "accept" {
			accepted++
		}
	}
	return fmt.Sprintf("%s, %d candidates, %d accepted", sr.Outcome, len(sr.Candidates), accepted)
}

// pickCandidate picks a result the way a user does: mostly the first.
func (s *sim) pickCandidate(sr searchResult) (int64, string) {
	c := sr.Candidates[0]
	if s.user.IntN(3) == 0 {
		c = sr.Candidates[s.user.IntN(len(sr.Candidates))]
	}
	return c.ID, c.Verdict
}

// search searches an episode as the terminal interface does.
func (s *sim) search(e *episode) (searchResult, string) {
	var sr searchResult
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/episodes/%d/search", e.id), "")
	if code != http.StatusOK {
		return sr, fmt.Sprintf("episode %d: %s", e.id, answer(code, body))
	}
	json.Unmarshal(body, &sr)
	return sr, fmt.Sprintf("episode %d: %s", e.id, sr)
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
	id, verdict := s.pickCandidate(sr)
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/candidates/%d/download", id), "")
	return fmt.Sprintf("%s; candidate %d (%s): %s", detail, id, verdict, answer(code, body))
}

func (s *sim) downloadBest() string {
	e := s.someEpisode()
	if e == nil {
		return ""
	}
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/episodes/%d/download", e.id), "")
	return fmt.Sprintf("episode %d: %s", e.id, answer(code, body))
}

// downloadPack searches a season for packs and downloads one.
func (s *sim) downloadPack() string {
	e := s.someEpisode()
	if e == nil {
		return ""
	}
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/series/%d/seasons/%d/search", e.show.id, e.season), "")
	var sr searchResult
	if code != http.StatusOK || json.Unmarshal(body, &sr) != nil || len(sr.Candidates) == 0 {
		return fmt.Sprintf("show %d season %d: %s %s", e.show.id, e.season, answer(code, body), sr)
	}
	id, verdict := s.pickCandidate(sr)
	code, body = s.call(http.MethodPost, fmt.Sprintf("/api/v1/season-candidates/%d/download", id), "")
	return fmt.Sprintf("show %d season %d: %s; pack %d (%s): %s", e.show.id, e.season, sr, id, verdict, answer(code, body))
}

// someShow is a show Garner has.
func (s *sim) someShow() *show {
	var shows []*show
	for _, sh := range s.w.shows {
		if sh.added {
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
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/series/%d/search", sh.id), "")
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
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/downloads/%d/cancel", d.ID), "")
	return fmt.Sprintf("download %d: %s", d.ID, answer(code, body))
}

func (s *sim) retry() string {
	d := s.someDownload(func(d store.Download) bool {
		return d.Import.State == store.ImportFailed || d.Import.State == store.ImportUnverified
	})
	if d == nil {
		return ""
	}
	return s.retryDownload(*d)
}

func (s *sim) retryDownload(d store.Download) string {
	s.retrying = d.ID
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/downloads/%d/import", d.ID), "")
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
	code, body := s.call(http.MethodDelete, fmt.Sprintf("/api/v1/series/%d?files=%s", sh.id, files), "")
	if code == http.StatusOK {
		sh.added = false
	} else {
		s.deleting = 0 // refused: nothing of the show may go
	}
	return fmt.Sprintf("show %d, files=%s: %s", sh.id, files, answer(code, body))
}

// addShow adds a show the way the terminal interface does: Garner records
// it, takes over its files in the library and then turns its monitor on.
// TheTVDB is not simulated, so the show goes into the database from the
// world instead of from Garner's catalogue.
func (s *sim) addShow() string {
	var absent []*show
	for _, sh := range s.w.shows {
		if !sh.added {
			absent = append(absent, sh)
		}
	}
	if len(absent) == 0 {
		return ""
	}
	sh := absent[s.user.IntN(len(absent))]
	sh.profile = "tv" // a show added again starts over, whatever it had
	if err := s.w.seedShow(s.ctx, s.st, sh, store.MonitorOff); err != nil {
		s.fail("setup", err.Error())
		return ""
	}
	sh.added = true
	detail := fmt.Sprintf("show %d: take-over %s", sh.id, s.takeOverShow(sh))
	code, body := s.call(http.MethodPatch, fmt.Sprintf("/api/v1/series/%d", sh.id), fmt.Sprintf(`{"monitor":%q}`, sh.monitor))
	if code == http.StatusOK {
		sh.since = s.w.now
	}
	return fmt.Sprintf("%s; monitor %s: %s", detail, sh.monitor, answer(code, body))
}

func (s *sim) takeOverShow(sh *show) string {
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/series/%d/files/take-over", sh.id), "")
	if code != http.StatusOK {
		return answer(code, body)
	}
	var t struct {
		Files, Episodes, Renamable int
		Skipped                    []any
	}
	json.Unmarshal(body, &t)
	// The user handed the files Garner took over to it: they are Garner's
	// now, no longer foreign.
	for _, p := range sortedKeys(s.w.foreign) {
		if s.recorded(p) {
			delete(s.w.foreign, p)
		}
	}
	return fmt.Sprintf("%d files for %d episodes, %d skipped, %d to rename", t.Files, t.Episodes, len(t.Skipped), t.Renamable)
}

// recorded reports whether Garner records the file at p as an episode's.
func (s *sim) recorded(p string) bool {
	for _, sh := range s.w.shows {
		for _, e := range sh.episodes {
			if f, found, _ := s.st.EpisodeFile(s.ctx, e.id); found && f.Path == p {
				return true
			}
		}
	}
	return false
}

func (s *sim) takeOver() string {
	sh := s.someShow()
	if sh == nil {
		return ""
	}
	return fmt.Sprintf("show %d: %s", sh.id, s.takeOverShow(sh))
}

func (s *sim) rename() string {
	sh := s.someShow()
	if sh == nil {
		return ""
	}
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/series/%d/files/rename", sh.id), "")
	if code != http.StatusOK {
		return fmt.Sprintf("show %d: %s", sh.id, answer(code, body))
	}
	var r struct {
		Files int
		Notes []string
	}
	json.Unmarshal(body, &r)
	return fmt.Sprintf("show %d: %d files, notes %q", sh.id, r.Files, r.Notes)
}

func (s *sim) changeProfile() string {
	sh := s.someShow()
	if sh == nil {
		return ""
	}
	profile := map[string]string{"tv": "anime", "anime": "tv"}[sh.profile]
	code, body := s.call(http.MethodPatch, fmt.Sprintf("/api/v1/series/%d", sh.id), fmt.Sprintf(`{"profile":%q}`, profile))
	if code == http.StatusOK {
		sh.profile = profile
	}
	return fmt.Sprintf("show %d to %s: %s", sh.id, profile, answer(code, body))
}

// snapshot is what the rules compare before and after a step.
type snapshot struct {
	downloads map[int64]store.Download
	records   map[int64]string  // the recorded file, by episode
	library   map[string]string // where each video in the libraries is, by video
	dupes     []string
	jobs      map[string]bool // SABnzbd's job folders that exist, by job
}

// libraries are the folders whose videos the rules follow: Garner's and
// Jellyfin's libraries, wherever they are plugged in.
var libraries = []string{"library", "media-old", "unplugged"}

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
	for _, top := range libraries {
		err = filepath.WalkDir(filepath.Join(s.w.root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if v, ok := s.video(p, d); ok {
				p = s.w.plugged(p)
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
	}
	return sn, nil
}

// plugged is where a file under the unplugged folder belongs.
func (w *world) plugged(p string) string {
	if rel, ok := strings.CutPrefix(w.rel(p), "unplugged/"); ok {
		return filepath.Join(w.root, rel)
	}
	return p
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

// foreignStays is S1: Garner never moves, renames or deletes a file no one
// gave it.
func (s *sim) foreignStays(_, _ snapshot) (string, string) {
	for _, p := range sortedKeys(s.w.foreign) {
		f := s.w.foreign[p]
		info, err := os.Stat(s.w.present(p))
		if err == nil && info.Size() == f.size && info.ModTime().Equal(f.mod) {
			continue
		}
		data, err := os.ReadFile(s.w.present(p))
		if err != nil || string(data) != f.content {
			return "S1", fmt.Sprintf("%s changed or went: %v", p, err)
		}
	}
	return "", ""
}

// filesStay is S2 and S3: a video leaves the libraries only when a newer
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
		if s.deleting != 0 && s.w.episode(episodes[0]) != nil && s.w.episode(episodes[0]).show.id == s.deleting {
			continue
		}
		for _, e := range episodes {
			p, ok := after.records[e]
			if v, read := readVideo(s.w.present(p)); !ok || !read || v.ID == id {
				return "S2", fmt.Sprintf("video %s of episode %d left the library from %s, and no other file replaced it", id, e, path)
			}
		}
	}
	return "", ""
}

// recordsMatch is S4: every recorded file exists.
func (s *sim) recordsMatch(_, after snapshot) (string, string) {
	for _, e := range sortedKeys64(after.records) {
		if _, err := os.Stat(s.w.present(after.records[e])); err != nil {
			return "S4", fmt.Sprintf("episode %d records %s, which is gone", e, after.records[e])
		}
	}
	return "", ""
}

// The share's top folders; nothing comes or goes there.
var top = []string{"elsewhere", "garner", "library", "media-old", "unplugged", "usenet"}

// outsideStays is S5: nothing outside the libraries, SABnzbd's folders and
// Garner's data changes, the library roots stay, and an unplugged root
// stays away until it is plugged in again.
func (s *sim) outsideStays(before, after snapshot) (string, string) {
	if s.digest(filepath.Join(s.w.root, "elsewhere")) != s.sentinel {
		return "S5", "the folder elsewhere changed"
	}
	entries, _ := os.ReadDir(s.w.root)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, top) {
		return "S5", fmt.Sprintf("the share's top holds %v", names)
	}
	for _, root := range roots {
		_, err := os.Stat(filepath.Join(s.w.root, root))
		if exists := err == nil; exists != (root != s.w.unplugged) {
			return "S5", fmt.Sprintf("library root %s exists %v while unplugged is %q", root, exists, s.w.unplugged)
		}
	}
	// A job folder goes when someone removes the job in SABnzbd by hand,
	// when Garner is done with its download (imported or rejected), when
	// Garner cancelled the download and SABnzbd deleted its files, or with
	// its show or episode.
	done := []string{store.ImportScanning, store.ImportDone, store.ImportUnverified, store.ImportRejected}
	for _, j := range s.w.sab.jobs {
		if !before.jobs[j.id] || after.jobs[j.id] {
			continue
		}
		j.lost = true
		d, ok := downloadOfJob(after, j.id)
		switch {
		case j.vanished:
		case !ok && (!j.posting.show.added || s.removed != 0):
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
		"":                   {store.ImportPending},
		store.ImportPending:  {store.ImportMoving, store.ImportRejected, store.ImportFailed},
		store.ImportMoving:   {store.ImportScanning, store.ImportDone, store.ImportUnverified, store.ImportFailed},
		store.ImportScanning: {store.ImportDone, store.ImportUnverified},
	}
	// retryNext are the transitions only Retry makes.
	retryNext = map[string][]string{
		store.ImportFailed:     {store.ImportPending, store.ImportMoving},
		store.ImportUnverified: {store.ImportScanning},
	}
	firstStates = []string{store.Submitting, store.Queued, store.Failed}
)

// statesMove is S6: downloads and imports change state only along the
// allowed transitions, and only the downloads of a deleted show or a
// dropped episode disappear.
func (s *sim) statesMove(before, after snapshot) (string, string) {
	for _, id := range sortedKeys64(before.downloads) {
		b := before.downloads[id]
		a, ok := after.downloads[id]
		if !ok {
			e := s.w.episode(b.EpisodeID)
			if b.EpisodeID != s.removed && e != nil && e.show.added {
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

// calm makes the world healthy: it heals the faults, puts a good release
// of every wanted episode on the indexers and retries the imports that wait
// for the user. Then the clock runs, by the minute while something runs
// and by six hours while nothing does, until nothing is left to do. The
// final rules follow: L1 and L2, every download and import ended; L3, the
// monitor got every wanted episode; L4, Garner cleaned up after its
// imports.
func (s *sim) calm() {
	w := s.w
	w.sab.loseNext, w.sab.hideNext = false, false
	s.do("world", "calm", func() string { return "the world turns healthy: " + s.advance(time.Minute) })
	s.do("world", "good releases", func() string {
		var posted []string
		for _, e := range s.monitored() {
			if _, found, _ := s.st.EpisodeFile(s.ctx, e.id); found {
				continue
			}
			// An episode still to come gets its release an hour after it airs.
			p := &posting{title: fmt.Sprintf("%s.S%02dE%02d.%s-CALM", e.show.dotted(), e.season, e.number, kinds[0].tags),
				show: e.show, season: e.season, episodes: []*episode{e}, posted: w.now.Add(-time.Hour)}
			if e.aired.After(w.now) {
				p.posted = e.aired.Add(time.Hour)
			}
			w.addPosting(p, kinds[0], true)
			p.audio, p.longer, p.corrupt, p.indexers = kinds[0].audio, 1, false, []int{1, 2}
			posted = append(posted, fmt.Sprint(e.id))
		}
		return strings.Join(posted, " ")
	})
	all, _ := s.st.Downloads(s.ctx, 100000)
	slices.SortFunc(all, func(a, b store.Download) int { return int(a.ID - b.ID) })
	for _, d := range all {
		if d.Import.State == store.ImportFailed || d.Import.State == store.ImportUnverified {
			s.do("user", "retry import", func() string { return s.retryDownload(d) })
		}
	}
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
		s.do("garner", "monitor step", func() string { s.srv.StepMonitor(s.ctx); return "" })
		busy := !s.settled()
		step := 6 * time.Hour
		if busy {
			step = time.Minute
		}
		s.do("world", "clock", func() string { return s.advance(step) })
		if !busy && len(s.missing()) == 0 {
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
	}
	if missing := s.missing(); len(missing) > 0 {
		s.fail("L3", fmt.Sprintf("the monitor did not get these wanted episodes: %v", missing))
	}
	s.cleanedUp(all)
}

// wanted are the aired episodes the monitor gets.
func (s *sim) wanted() []*episode {
	return slices.DeleteFunc(s.monitored(), func(e *episode) bool { return e.aired.After(s.w.now) })
}

// monitored are the episodes the monitor gets once they aired: all of a
// show's, or those airing since its monitor was set to new.
func (s *sim) monitored() []*episode {
	var out []*episode
	for _, sh := range s.w.shows {
		if !sh.added || sh.monitor == store.MonitorOff {
			continue
		}
		for _, e := range sh.episodes {
			if sh.monitor == store.MonitorAll || e.aired.Format(time.DateOnly) >= sh.since.Format(time.DateOnly) {
				out = append(out, e)
			}
		}
	}
	return out
}

// missing are the wanted episodes without a file whose latest download
// does not wait for the user. An import that failed or that Jellyfin did
// not confirm waits for the user to act on its recovery step; a download
// the user cancelled is the user's choice, which the monitor respects.
func (s *sim) missing() []int64 {
	all, _ := s.st.Downloads(s.ctx, 100000)
	var out []int64
	for _, e := range s.wanted() {
		if _, found, _ := s.st.EpisodeFile(s.ctx, e.id); found {
			continue
		}
		var latest *store.Download
		for i := range all {
			if all[i].EpisodeID == e.id && (latest == nil || all[i].ID > latest.ID) {
				latest = &all[i]
			}
		}
		if latest != nil && (latest.State == store.Cancelled || latest.Import.Recovery != "" &&
			(latest.Import.State == store.ImportFailed || latest.Import.State == store.ImportUnverified)) {
			continue
		}
		out = append(out, e.id)
	}
	return out
}

// cleanedUp is L4: SABnzbd's folder of every download Garner imported is
// gone, unless it holds a file Garner could not assign to an episode, such
// as an extra or a file of an episode the catalogue dropped, or a subtitle
// it could not move.
func (s *sim) cleanedUp(all []store.Download) {
	for _, d := range all {
		if d.Import.State != store.ImportDone && d.Import.State != store.ImportUnverified {
			continue
		}
		var dir string
		for _, j := range s.w.sab.jobs {
			if j.id == d.JobID && !j.lost {
				dir = j.dir
			}
		}
		if _, err := os.Stat(dir); dir == "" || err != nil {
			continue
		}
		var left []string
		explained := false
		filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			if v, ok := readVideo(p); ok && (v.Extra || slices.ContainsFunc(v.Episodes, func(id int64) bool { return s.w.episode(id) == nil })) || guard.IsSubtitle(p) {
				explained = true
			}
			left = append(left, filepath.Base(p))
			return nil
		})
		if !explained {
			s.fail("L4", fmt.Sprintf("SABnzbd's folder of download %d stayed after its import, holding only %v; notes %q", d.ID, left, d.Import.Notes))
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
