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
	"errors"
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
	"github.com/trucke/garner/internal/profile"
	"github.com/trucke/garner/internal/selection"
	"github.com/trucke/garner/internal/server"
	"github.com/trucke/garner/internal/store"
	"github.com/trucke/garner/internal/testprofiles"
)

var (
	seedFlag  = flag.Uint64("seed", 0, "run only this seed")
	seedsFlag = flag.Int("seeds", 0, "run this many fresh seeds instead of the fixed ones")
	stepsFlag = flag.Int("steps", defaultSteps, "random steps per seed, before the calm period")
)

const defaultSteps = 300

// Garner tells some dates in local time, such as the day a show's monitor
// started. One zone everywhere keeps a seed's story the same on every
// machine.
func init() { time.Local = time.UTC }

// fixedSeeds run with every go test. Together, every event changes
// something in them (a refused request does not count), a crash and
// another step meet each step that changes files, they cover both path
// setups and the three disk setups, and they catch the bugs built into
// Garner on purpose to test the simulation: overwriting a file Garner does
// not own, removing SABnzbd's whole folder, never giving up on Jellyfin,
// downloading again while an import waits for the user, not finishing the
// changes a crash left and creating a library folder that went. Rarer bugs
// have focused tests instead: an import's move without the lock on the
// library's files, which no seed of 240 caught (TestImportDuringRename),
// and two the burn-in of 20,000 seeds found once or twice: adopting
// another download's SABnzbd job (TestConfirmLeavesOtherJobs) and
// recording over a better file the library gained
// (TestImportKeepsBetterGainedMeanwhile).
//
// A change to the world sends every seed down another path, and
// TestSimulation then fails when an event, a crash point or a setup went
// missing. To choose again, swap in other seeds until it passes; a seed
// replayed with -v shows what it does and what it covered. Whether they
// still catch the bugs takes building them in again by hand. A bug a seed
// found gets a focused test of its own.
var fixedSeeds = []uint64{7, 26, 35, 64, 87}

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
				t.Log("covered: " + strings.Join(first.covered, "; "))
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
	// profiles holds the profile of each type, as installProfiles made it.
	profiles map[string]int64
	srv      *server.Server
	api      http.Handler
	token    string
	pick     *rand.Rand // the next event
	user     *rand.Rand // the user's choices
	// inside decides what happens at a change to a file: a crash, another
	// step first or nothing.
	inside *rand.Rand
	// events are the steps that can happen; current is the one running,
	// and nested whether a step runs inside it.
	events  []event
	current string
	nested  bool
	trace   []string
	step    int
	// deleting are the shows the user deletes with their files in this
	// step, retrying the downloads the user retries and removed the
	// episodes the catalogue drops. Sets: a step may run inside another.
	deleting, retrying, removed map[int64]bool
	// deleted are the shows deleted in this step, whatever happened to
	// them after inside it, as being added again.
	deleted map[int64]bool
	// strays counts the user's files put in the way, for their contents.
	strays int
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
	// held are the videos an episode's record held, with the episodes,
	// until the show is deleted or the catalogue drops one of them.
	held map[string][]int64
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
		pick: rand.New(rand.NewPCG(seed, 3)), user: rand.New(rand.NewPCG(seed, 4)), inside: rand.New(rand.NewPCG(seed, 5)),
		videos: map[string]cachedVideo{}, held: map[string][]int64{}, covered: map[string]bool{}}
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
	for _, e := range s.allEvents() {
		all = append(all, e)
		if e.on {
			events = append(events, e)
		}
	}
	s.events = events
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
	for _, p := range changing {
		o.all = append(o.all, "crash during "+p, "step inside "+p)
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
	if err := s.installProfiles(st); err != nil {
		st.Close()
		return err
	}
	deps := server.Deps{
		Now:       func() time.Time { return s.w.now },
		Sleep:     func(_ context.Context, d time.Duration) error { s.w.now = s.w.now.Add(d); return nil },
		Transport: transport{s.w},
		Prober:    prober{},
		Rename:    s.moveFile,
		Before:    s.beforeChange,
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

// allEvents are all that can happen next, with their weights. Faults the
// seed did not turn on never happen.
func (s *sim) allEvents() []event {
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
	s.deleting, s.retrying, s.removed, s.deleted = map[int64]bool{}, map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
	if s.last == nil {
		sn, err := s.snapshot()
		if err != nil {
			s.fail(simStopped, err.Error())
			return
		}
		s.last = &sn
	}
	before := *s.last
	s.step++
	s.refused = false
	s.current = actor + " " + name
	detail := s.runStep(f)
	s.current = ""
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
		s.fail(simStopped, err.Error())
		return
	}
	s.last = &after
	s.hold(after)
	// The rules judge Garner by the simulation's notes, so those are checked
	// first. What the user sets changes only before the calm period, which
	// checks it once at its end, with the episodes, which only the
	// simulation's own catalogue changes write.
	if s.step <= s.steps {
		if msg := s.outOfStep(false); msg != "" {
			s.fail(simOutOfStep, msg)
			return
		}
	}
	for _, rule := range []func(snapshot, snapshot) (string, string){s.foreignStays, s.filesStay, s.recordsMatch, s.outsideStays, s.statesMove} {
		if id, msg := rule(before, after); msg != "" {
			s.fail(id, msg)
			return
		}
	}
}

// Failures that are the simulation's, not Garner's.
const (
	simOutOfStep = "out of step" // the simulation's notes differ from what it told Garner
	simStopped   = "setup"       // the simulation could not go on
)

// outOfStep compares what the simulation told Garner with Garner's
// database: which shows it has, with what monitor mode, start and profile,
// and with episodes, their episodes as the catalogue has them. A
// difference is the simulation's own mistake, so the rules would judge
// Garner by notes that are wrong.
func (s *sim) outOfStep(episodes bool) string {
	list, err := s.st.ListSeries(s.ctx)
	if err != nil {
		return err.Error()
	}
	garner := map[int64]store.Series{}
	for _, sr := range list {
		garner[sr.ID] = sr
	}
	for _, sh := range s.w.shows {
		sr, added := garner[sh.id]
		if added != sh.added {
			return fmt.Sprintf("show %d: the simulation notes added %v, Garner has it %v", sh.id, sh.added, added)
		}
		if !added {
			continue
		}
		delete(garner, sh.id)
		if sr.Monitor != sh.monitor || sr.Type != sh.profile || sr.ProfileID != s.profiles[sh.profile] {
			return fmt.Sprintf("show %d: the simulation notes monitor %s and type %s, Garner has %s, %s and profile %d",
				sh.id, sh.monitor, sh.profile, sr.Monitor, sr.Type, sr.ProfileID)
		}
		if sr.Monitor == store.MonitorNew && sr.MonitorSince.Unix() != sh.since.Unix() {
			return fmt.Sprintf("show %d: the simulation notes new episodes from %s, Garner from %s", sh.id, sh.since, sr.MonitorSince)
		}
		if !episodes {
			continue
		}
		d, err := s.st.LoadSeries(s.ctx, sh.id)
		if err != nil {
			return err.Error()
		}
		noted := map[int64]string{}
		for _, e := range sh.episodes {
			noted[e.id] = fmt.Sprintf("S%02dE%02d aired %s", e.season, e.number, e.aired.Format(time.DateOnly))
		}
		for _, e := range d.Episodes {
			n := e.Numbers["official"]
			got := fmt.Sprintf("S%02dE%02d aired %s", n.Season, n.Number, e.Aired)
			if noted[e.ID] != got {
				return fmt.Sprintf("show %d: episode %d is %q in the simulation's notes, %q in Garner", sh.id, e.ID, noted[e.ID], got)
			}
			delete(noted, e.ID)
		}
		for _, id := range sortedKeys64(noted) {
			return fmt.Sprintf("show %d: episode %d is %q in the simulation's notes and not in Garner", sh.id, id, noted[id])
		}
	}
	for _, id := range sortedKeys64(garner) {
		return fmt.Sprintf("Garner has show %d, which the simulation does not know", id)
	}
	return ""
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
	what := "Garner broke " + rule
	switch rule {
	case simOutOfStep:
		what = "the simulation is out of step with Garner, its own mistake"
	case simStopped:
		what = "the simulation could not go on"
	}
	s.failure = fmt.Sprintf("seed %d, step %d: %s\n  %s\nlast steps:\n  %s\nreplay: go test ./internal/simulation -run TestSimulation -seed %d -steps %d -v",
		s.seed, s.step, what, strings.ReplaceAll(msg, s.w.root, "<share>"), strings.Join(last, "\n  "), s.seed, s.steps)
}

// restart throws Garner away and sets it up again from the database.
func (s *sim) restart() string {
	if err := s.st.Close(); err != nil {
		return "close: " + err.Error()
	}
	if err := s.boot(); err != nil {
		s.fail(simStopped, "restart: "+err.Error())
	}
	return "Garner started again"
}

// boot starts Garner again, as after a restart or a crash: its import
// loop runs a round at once, which finishes the changes to files a crash
// interrupted.
func (s *sim) boot() error {
	if err := s.start(); err != nil {
		return err
	}
	nested := s.nested
	s.nested = true // nothing crashes or runs in between while starting
	s.srv.WorkImports(s.ctx)
	s.nested = nested
	return nil
}

// changing are the steps that change files: those a crash or another step
// must meet in the middle.
var changing = []string{"garner import step", "user rename", "user delete show"}

// crash ends Garner in the middle of a step: before a change to a file,
// or right after a move.
type crash struct{ when, path string }

// beforeChange runs before each change to a file the guard allows. With
// the faults on, Garner crashes there now and then, or another step runs
// first, as another goroutine or request may.
func (s *sim) beforeChange(op, path string) {
	if s.nested || s.current == "" {
		return
	}
	f := s.w.faults
	switch n := s.inside.IntN(100); {
	case f.crashes && n < 4:
		panic(crash{"before " + op, path})
	case f.between && n >= 4 && n < 14:
		s.between(op, path)
	}
}

// moveFile moves a file for Garner as the world does; with crashes on,
// Garner now and then ends right after the move, before it recorded it.
func (s *sim) moveFile(oldpath, newpath string) error {
	err := s.w.rename(oldpath, newpath)
	if err == nil && s.w.faults.crashes && !s.nested && s.current != "" && s.inside.IntN(100) < 5 {
		panic(crash{"after rename to", newpath})
	}
	return err
}

// between runs another step before a change to a file. Garner's loops each
// run in one goroutine, so a loop's step never runs inside its own; a
// restart is a crash, which beforeChange makes.
func (s *sim) between(op, path string) {
	var steps []event
	total := 0
	for _, e := range s.events {
		if e.actor == "garner" && e.actor+" "+e.name == s.current || e.actor == "process" {
			continue
		}
		steps = append(steps, e)
		total += e.weight
	}
	n := s.inside.IntN(total)
	for _, e := range steps {
		if n -= e.weight; n >= 0 {
			continue
		}
		s.nested = true
		outer := s.refused
		s.refused = false
		detail := e.run()
		refused := s.refused
		s.refused, s.nested = outer, false
		s.note("  "+e.actor+" "+e.name, fmt.Sprintf("inside %s, before %s %s: %s", s.current, op, s.w.rel(path), detail))
		if (e.actor == "garner" || detail != "" && !refused) && slices.Contains(changing, s.current) {
			s.covered["step inside "+s.current] = true
		}
		return
	}
}

// runStep runs a step. A crash in it ends Garner, which starts again from
// its database, as after a power cut.
func (s *sim) runStep(f func() string) (detail string) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		c, ok := r.(crash)
		if !ok {
			panic(r)
		}
		if slices.Contains(changing, s.current) {
			s.covered["crash during "+s.current] = true
		}
		s.nested = false
		detail = fmt.Sprintf("crashed %s %s", c.when, s.w.rel(c.path))
		if err := s.st.Close(); err != nil {
			s.fail(simStopped, "close after the crash: "+err.Error())
			return
		}
		if err := s.boot(); err != nil {
			s.fail(simStopped, "start after the crash: "+err.Error())
			return
		}
		// The step's answer went with the crash; the database tells what
		// it did, such as whether a deleted show is gone.
		for _, sh := range s.w.shows {
			added, err := s.st.HasSeries(s.ctx, sh.id)
			if err != nil {
				s.fail(simStopped, err.Error())
				return
			}
			sh.added = added
		}
	}()
	return f()
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
		s.fail(simStopped, err.Error())
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
	// A step this one runs inside may have deleted the show already.
	if added, err := s.st.HasSeries(s.ctx, sh.id); err != nil || !added {
		if err != nil {
			s.fail(simStopped, err.Error())
		}
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
		dropped := sh.episodes[i].id
		s.removed[dropped] = true
		sh.episodes = slices.Delete(sh.episodes, i, i+1)
		what = fmt.Sprintf("episode %d dropped", dropped)
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
		s.fail(simStopped, err.Error())
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
	s.strays++
	if !s.w.addForeign(rel, fmt.Sprintf(`{"id":"stray-%d"}`, s.strays)) {
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
	s.retrying[d.ID] = true
	code, body := s.call(http.MethodPost, fmt.Sprintf("/api/v1/downloads/%d/import", d.ID), "")
	return fmt.Sprintf("download %d (%s): %s", d.ID, d.Import.State, answer(code, body))
}

func (s *sim) deleteShow() string {
	sh := s.someShow()
	if sh == nil {
		return ""
	}
	files := []string{"keep", "delete"}[s.user.IntN(2)]
	outer := s.deleting[sh.id] // a step this one runs inside deletes it
	if files == "delete" {
		s.deleting[sh.id] = true
	}
	code, body := s.call(http.MethodDelete, fmt.Sprintf("/api/v1/series/%d?files=%s", sh.id, files), "")
	if code == http.StatusOK {
		sh.added, s.deleted[sh.id] = false, true
	} else if !outer {
		delete(s.deleting, sh.id) // refused: nothing of the show may go
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
		s.fail(simStopped, err.Error())
		return ""
	}
	// Seeded with the monitor off; the notes take the mode the user wants
	// once Garner took it.
	want := sh.monitor
	sh.added, sh.monitor = true, store.MonitorOff
	detail := fmt.Sprintf("show %d: take-over %s", sh.id, s.takeOverShow(sh))
	code, body := s.call(http.MethodPatch, fmt.Sprintf("/api/v1/series/%d", sh.id), fmt.Sprintf(`{"monitor":%q}`, want))
	if code == http.StatusOK {
		sh.monitor, sh.since = want, s.w.now
	}
	return fmt.Sprintf("%s; monitor %s: %s", detail, want, answer(code, body))
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
	// Type and profile change together, as the one profile of each type
	// did before profiles moved into the database.
	profile := map[string]string{"tv": "anime", "anime": "tv"}[sh.profile]
	code, body := s.call(http.MethodPatch, fmt.Sprintf("/api/v1/series/%d", sh.id),
		fmt.Sprintf(`{"type":%q,"profile_id":%d}`, profile, s.profiles[profile]))
	if code == http.StatusOK {
		sh.profile = profile
	}
	return fmt.Sprintf("show %d to %s: %s", sh.id, profile, answer(code, body))
}

// installProfiles gives Garner the profiles of an installation from before
// profiles moved into the database, once: German for TV shows and German
// with English for anime, as the defaults. The simulation notes a show's
// type; its profile follows the type.
func (s *sim) installProfiles(st *store.Store) error {
	s.profiles = map[string]int64{}
	for _, p := range []struct {
		profile profile.Profile
		typ     string
	}{{testprofiles.German, store.TypeTV}, {testprofiles.GermanEnglish, store.TypeAnime}} {
		stored, err := st.ProfileByName(s.ctx, p.profile.Name)
		if errors.Is(err, store.ErrNoProfile) {
			stored.ID, err = st.CreateProfile(s.ctx, p.profile.Name, profile.Encode(p.profile))
			if err == nil {
				err = st.SetDefaultProfile(s.ctx, p.typ, stored.ID)
			}
		}
		if err != nil {
			return err
		}
		s.profiles[p.typ] = stored.ID
	}
	return nil
}

// snapshot is what the rules compare before and after a step.
type snapshot struct {
	downloads map[int64]store.Download
	records   map[int64]string  // the recorded file, by episode
	tiers     map[int64]string  // the recorded file's tier, by episode
	library   map[string]string // where each video in the libraries is, by video
	dupes     []string
	// planned are the files Garner planned to remove, as the rest of an
	// upgrade that replaced them.
	planned map[string]bool
	jobs    map[string]bool // SABnzbd's job folders that exist, by job
}

// libraries are the folders whose videos the rules follow: Garner's and
// Jellyfin's libraries, wherever they are plugged in.
var libraries = []string{"library", "media-old", "unplugged"}

func (s *sim) snapshot() (snapshot, error) {
	sn := snapshot{downloads: map[int64]store.Download{}, records: map[int64]string{}, tiers: map[int64]string{}, library: map[string]string{}, jobs: map[string]bool{},
		planned: map[string]bool{}}
	changes, err := s.st.FileChanges(s.ctx)
	if err != nil {
		return sn, err
	}
	for _, c := range changes {
		if c.NewPath == "" {
			sn.planned[c.Path] = true
		}
	}
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
				sn.records[e.id], sn.tiers[e.id] = f.Path, f.Tier
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
		if len(episodes) == 0 && (before.planned[path] || inFlight(before, path)) {
			continue // the rest of an upgrade that replaced it, or an import's own file
		}
		if len(episodes) == 0 {
			return "S2", fmt.Sprintf("video %s at %s left the library, and no episode recorded it", id, path)
		}
		if ep := s.w.episode(episodes[0]); ep != nil && s.deleting[ep.show.id] {
			continue
		}
		for _, e := range episodes {
			p, ok := after.records[e]
			if v, read := readVideo(s.w.present(p)); !ok || !read || v.ID == id {
				return "S2", fmt.Sprintf("video %s of episode %d left the library from %s, and no other file replaced it", id, e, path)
			}
			if msg := s.worse(before, after, e); msg != "" {
				return "S2", msg
			}
		}
	}
	return "", ""
}

// inFlight reports whether path is where an import that has not ended
// moves a file: until it records the file, it is the import's to keep or
// remove.
func inFlight(sn snapshot, path string) bool {
	for _, id := range sortedKeys64(sn.downloads) {
		d := sn.downloads[id]
		if d.Import.State != store.ImportMoving && d.Import.State != store.ImportFailed {
			continue
		}
		if slices.ContainsFunc(d.Import.Files, func(f store.ImportFile) bool { return f.Target == path }) {
			return true
		}
	}
	return false
}

// worse says how episode e's file was replaced by one of a worse tier,
// unless the user chose a release over that file, knowing it, and its
// import moved its file in this step: a download of the episode alone
// that started while the file, or one as good, was there. Other imports
// in the step may have replaced that release's file again, with a better
// one. A season pack is for the season's missing episodes, never chosen
// over a file.
func (s *sim) worse(before, after snapshot, e int64) string {
	ep := s.w.episode(e)
	if ep == nil {
		return ""
	}
	profile := selection.FromProfile(testprofiles.German, "")
	if ep.show.profile == store.TypeAnime {
		profile = selection.FromProfile(testprofiles.GermanEnglish, "")
	}
	rank := func(tier string) int { return selection.TierRank(profile, tier) }
	old, now := before.tiers[e], after.tiers[e]
	if rank(now) <= rank(old) {
		return ""
	}
	moved := []string{store.ImportScanning, store.ImportDone, store.ImportUnverified}
	for _, id := range sortedKeys64(after.downloads) {
		d := after.downloads[id]
		if d.EpisodeID != e || d.SearchSeason > 0 || d.Replaces == "" || rank(old) < rank(d.Replaces) {
			continue
		}
		b, ok := before.downloads[id]
		if slices.Contains(moved, d.Import.State) && !(ok && sameDownload(b, d) && slices.Contains(moved, b.Import.State)) {
			return ""
		}
	}
	return fmt.Sprintf("episode %d's file %s (%s) was replaced by a worse one, %s (%s)", e, before.records[e], cmp.Or(old, "tier unknown"), after.records[e], cmp.Or(now, "tier unknown"))
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
		case !ok && (!j.posting.show.added || s.deleted[j.posting.show.id] || len(s.removed) > 0):
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
)

// statesMove is S6: downloads and imports change state only along the
// allowed transitions, and only the downloads of a deleted show or a
// dropped episode disappear.
func (s *sim) statesMove(before, after snapshot) (string, string) {
	for _, id := range sortedKeys64(before.downloads) {
		b := before.downloads[id]
		a, ok := after.downloads[id]
		ok = ok && sameDownload(a, b)
		if !ok {
			e := s.w.episode(b.EpisodeID)
			if !s.removed[b.EpisodeID] && e != nil && e.show.added && !s.deleted[e.show.id] {
				return "S6", fmt.Sprintf("download %d disappeared, and its show is still there", id)
			}
			continue
		}
		if !reachable(downloadNext, b.State, a.State) {
			return "S6", fmt.Sprintf("download %d went from %s to %s", id, b.State, a.State)
		}
		graph := importNext
		if s.retrying[id] {
			graph = merge(importNext, retryNext)
		}
		if !reachable(graph, b.Import.State, a.Import.State) {
			return "S6", fmt.Sprintf("the import of download %d went from %q to %q", id, b.Import.State, a.Import.State)
		}
	}
	// Every download starts as submitting; steps inside this one may have
	// moved it on already.
	for _, id := range sortedKeys64(after.downloads) {
		a := after.downloads[id]
		if b, ok := before.downloads[id]; (!ok || !sameDownload(a, b)) && !reachable(downloadNext, store.Submitting, a.State) {
			return "S6", fmt.Sprintf("download %d started as %s", id, a.State)
		}
	}
	return "", ""
}

// sameDownload tells a download from a later one that got its number: the
// database reuses the highest number after a show's downloads went.
func sameDownload(a, b store.Download) bool {
	return a.Created.Equal(b.Created) && a.EpisodeID == b.EpisodeID && a.Title == b.Title
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
	w.faults.crashes, w.faults.between = false, false
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
						s.fail(simStopped, err.Error())
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
	if msg := s.outOfStep(true); msg != "" {
		s.fail(simOutOfStep, msg)
		return
	}
	all, err := s.st.Downloads(s.ctx, 100000)
	if err != nil {
		s.fail(simStopped, err.Error())
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
	if s.failure == "" {
		s.replacedGone(*s.last)
	}
}

// hold notes the videos episodes' records hold. It lets go of those of a
// deleted show, whose staying is the user's choice, and of an episode the
// catalogue dropped, whose record went with it.
func (s *sim) hold(sn snapshot) {
	byPath := make(map[string]string, len(sn.library))
	for id, p := range sn.library {
		byPath[p] = id
	}
	for _, e := range sortedKeys64(sn.records) {
		if id, ok := byPath[sn.records[e]]; ok && !slices.Contains(s.held[id], e) {
			s.held[id] = append(s.held[id], e)
		}
	}
	for _, id := range sortedKeys(s.held) {
		if slices.ContainsFunc(s.held[id], func(e int64) bool { ep := s.w.episode(e); return ep == nil || !ep.show.added || s.deleted[ep.show.id] }) {
			delete(s.held, id)
		}
	}
}

// replacedGone is L5: a video that was an episode's file and is no longer
// any episode's is gone from the library, as when an upgrade replaced it.
func (s *sim) replacedGone(sn snapshot) {
	recorded := map[string]bool{}
	for _, p := range sn.records {
		recorded[p] = true
	}
	for _, id := range sortedKeys(sn.library) {
		if p := sn.library[id]; s.held[id] != nil && !recorded[p] {
			s.fail("L5", fmt.Sprintf("%s was an episode's file and stayed in the library after no episode recorded it any more", p))
			return
		}
	}
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
		s.note("end download", fmt.Sprintf("%d episode %d %s %s %s/%s %s", d.ID, d.EpisodeID, d.JobID, d.Title, d.State, d.Import.State, d.Import.Detail))
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
