package importer

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/importer/guard"
	"github.com/trucke/garner/internal/inspect"
	"github.com/trucke/garner/internal/jellyfin"
	"github.com/trucke/garner/internal/selection"
	"github.com/trucke/garner/internal/store"
)

type fakeProber struct{ probe inspect.Probe }

func (f fakeProber) Probe(context.Context, string) (inspect.Probe, error) { return f.probe, nil }

var german = inspect.Probe{Duration: 22 * time.Minute, Video: []inspect.Video{{Width: 1920, Height: 1080}},
	Audio: []inspect.Track{{Language: "ger"}, {Language: "eng"}}}

// fakeJellyfin lists the files it was told about, the way Jellyfin with
// the TVDB plugin identifies them.
type fakeJellyfin struct {
	mu      sync.Mutex
	created []string
	// ignoreHints acts like Jellyfin with an empty library: it ignores
	// Created until a full scan.
	ignoreHints bool
	pending     []string
	scans       int
	// unidentified lookups list episodes without TVDB IDs, as Jellyfin
	// does before its metadata provider answered.
	unidentified int
}

func (f *fakeJellyfin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/Library/Media/Updated":
		var body struct{ Updates []struct{ Path string } }
		json.NewDecoder(r.Body).Decode(&body)
		for _, u := range body.Updates {
			if f.ignoreHints {
				f.pending = append(f.pending, u.Path)
			} else {
				f.created = append(f.created, u.Path)
			}
		}
	case "/Library/VirtualFolders":
		json.NewEncoder(w).Encode([]map[string]any{{"Name": "TV Shows", "Locations": []string{"/data/library/tv"}}})
	case "/Library/Refresh":
		f.scans++
		f.created, f.pending = append(f.created, f.pending...), nil
	case "/Items":
		var items []map[string]any
		for _, p := range f.created {
			items = append(items, map[string]any{"Id": "s1", "Path": filepath.Dir(filepath.Dir(p)), "ProviderIds": map[string]string{"Tvdb": "42"}})
		}
		json.NewEncoder(w).Encode(map[string]any{"Items": items})
	case "/Shows/s1/Episodes":
		var items []map[string]any
		for _, p := range f.created {
			item := map[string]any{"Path": p, "Name": "Pilot", "ParentIndexNumber": 1, "IndexNumber": 1, "ProviderIds": map[string]string{"Tvdb": "101"}}
			if f.unidentified > 0 {
				item = map[string]any{"Path": p, "Name": filepath.Base(p), "ProviderIds": map[string]string{}}
			}
			items = append(items, item)
		}
		if f.unidentified > 0 {
			f.unidentified--
		}
		json.NewEncoder(w).Encode(map[string]any{"Items": items})
	}
}

type env struct {
	st       *store.Store
	svc      *Service
	jf       *fakeJellyfin
	share    string // stands in for the mounted share
	rejected []string
}

// setup builds a share with a completed download of "Harbor Precinct:
// Night Shift" S01E01 and a library folder for regular TV.
func setup(t *testing.T, probe inspect.Probe) (*env, store.Download) {
	t.Helper()
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	st, err := store.Open(ctx, t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SaveSeries(ctx, store.SeriesData{
		Series: store.Series{ID: 42, Slug: "x", Name: "Harbor Precinct: Night Shift", OriginalLanguage: "eng", OriginalCountry: "usa",
			Status: "Ended", Year: 2013, DefaultOrder: "official", TVDBUpdated: "2026-01-01", Profile: "tv"},
		Episodes: []store.Episode{
			{ID: 101, Runtime: 22, NameEng: "Pilot", Numbers: map[string]store.Position{"official": {Season: 1, Number: 1}}},
			{ID: 102, Runtime: 22, NameEng: "What? Why?", Numbers: map[string]store.Position{"official": {Season: 1, Number: 2}}},
		},
	}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	share := t.TempDir()
	job := filepath.Join(share, "usenet", "complete", "tv", "Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP")
	for _, dir := range []string{job, filepath.Join(share, "library", "tv")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(job, "x7f3a.mkv"), make([]byte, 1000), 0o644)
	os.WriteFile(filepath.Join(job, "x7f3a.sample.mkv"), make([]byte, 50), 0o644)
	os.WriteFile(filepath.Join(job, "release.nfo"), []byte("info"), 0o644)

	d, err := st.StartDownload(ctx, store.Download{EpisodeID: 101, Title: "Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP",
		Indexer: "One", Link: "x", Verdict: "accept", Tier: "German 1080p"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d.State, d.Storage = store.Completed, "/data/usenet/complete/tv/Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP"
	if err := st.UpdateDownload(ctx, d, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkImportPending(ctx, d.ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	jf := &fakeJellyfin{}
	srv := httptest.NewServer(jf)
	t.Cleanup(srv.Close)
	e := &env{st: st, jf: jf, share: share}
	e.svc = New(st, fakeProber{probe}, jellyfin.New(srv.URL, "key", srv.Client()), Options{
		SABnzbdPaths:  config.PathMap{"/data/usenet": filepath.Join(share, "usenet")},
		JellyfinPaths: config.PathMap{"/data/library": filepath.Join(share, "library")},
		Library:       config.Library{TV: filepath.Join(share, "library", "tv"), Anime: filepath.Join(share, "library", "anime")},
	}, log)
	e.svc.OnRejected = func(_ context.Context, _ store.Download, reason string) { e.rejected = append(e.rejected, reason) }
	return e, d
}

const jobDir = "usenet/complete/tv/Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP"

const target = "library/tv/Harbor.Precinct.Night.Shift.2013.[tvdbid-42]/Season.01/Harbor.Precinct.Night.Shift.2013.S01E01.Pilot.WEB-1080p.DE.EN-GRP.mkv"

func TestImport(t *testing.T) {
	ctx := context.Background()
	e, d := setup(t, german)
	e.svc.Work(ctx)

	got, _ := e.st.GetDownload(ctx, d.ID)
	if got.Import.State != store.ImportDone || !strings.Contains(got.Import.Detail, `S01E01 "Pilot", TVDB episode 101`) {
		t.Fatalf("import = %+v", got.Import)
	}
	if _, err := os.Stat(filepath.Join(e.share, target)); err != nil {
		t.Errorf("not in the library: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.share, "usenet/complete/tv/Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP/x7f3a.mkv")); !os.IsNotExist(err) {
		t.Error("the download still holds the file; it should have been moved")
	}
	if len(got.Import.Files) != 1 {
		t.Errorf("imported %d files; the sample must stay behind", len(got.Import.Files))
	}
	if want := "/data/" + target; len(e.jf.created) != 1 || e.jf.created[0] != want {
		t.Errorf("Jellyfin was told %q, want %q", e.jf.created, want)
	}
	if f, found, _ := e.st.EpisodeFile(ctx, 101); !found || f.Path != filepath.Join(e.share, target) {
		t.Errorf("episode file = %+v, %v", f, found)
	}
}

// TestImportSamePaths imports with Garner, SABnzbd and Jellyfin seeing the
// files under the same paths, as on one machine: garner.toml has no paths
// entries.
func TestImportSamePaths(t *testing.T) {
	ctx := context.Background()
	e, d := setup(t, german)
	e.svc = New(e.st, fakeProber{german}, e.svc.jellyfin, Options{Library: e.svc.opts.Library}, slog.New(slog.DiscardHandler))
	d.Storage = filepath.Join(e.share, jobDir)
	if err := e.st.UpdateDownload(ctx, d, time.Now()); err != nil {
		t.Fatal(err)
	}
	e.svc.Work(ctx)

	if got, _ := e.st.GetDownload(ctx, d.ID); got.Import.State != store.ImportDone {
		t.Fatalf("import = %+v", got.Import)
	}
	if want := filepath.Join(e.share, target); len(e.jf.created) != 1 || e.jf.created[0] != want {
		t.Errorf("Jellyfin was told %q, want %q", e.jf.created, want)
	}
	if _, err := os.Stat(filepath.Join(e.share, jobDir)); !os.IsNotExist(err) {
		t.Errorf("SABnzbd's folder stayed: %v", err)
	}
}

// TestImportMovedAlready moves again after a restart that came after
// SABnzbd's folder was removed: the files count as moved, and no note
// claims the folder was left in place.
func TestImportMovedAlready(t *testing.T) {
	ctx := context.Background()
	e, d := setup(t, german)
	e.svc.Work(ctx)
	got, _ := e.st.GetDownload(ctx, d.ID)
	if got.Import.State != store.ImportDone {
		t.Fatalf("import = %+v", got.Import)
	}
	if err := e.st.UpdateImport(ctx, d.ID, store.Import{State: store.ImportMoving, Files: got.Import.Files}, time.Now()); err != nil {
		t.Fatal(err)
	}
	e.svc.Work(ctx)
	if got, _ := e.st.GetDownload(ctx, d.ID); got.Import.State != store.ImportDone || len(got.Import.Notes) != 0 {
		t.Errorf("after moving again: %+v", got.Import)
	}
}

// TestImportEmptyLibrary covers Jellyfin ignoring the hint for a library it
// has never seen files in: Garner asks for a full scan once.
func TestImportEmptyLibrary(t *testing.T) {
	ctx := context.Background()
	e, d := setup(t, german)
	e.jf.ignoreHints = true
	start := time.Now()
	e.svc.Now = func() time.Time { return start }
	e.svc.Work(ctx)
	if got, _ := e.st.GetDownload(ctx, d.ID); got.Import.State != store.ImportScanning || e.jf.scans != 0 {
		t.Fatalf("right after the move: %+v, %d scans", got.Import, e.jf.scans)
	}
	e.svc.Now = func() time.Time { return start.Add(2 * time.Minute) }
	e.svc.Work(ctx)
	e.svc.Work(ctx)
	got, _ := e.st.GetDownload(ctx, d.ID)
	if got.Import.State != store.ImportDone || e.jf.scans != 1 {
		t.Errorf("after the fallback: %+v, %d scans", got.Import, e.jf.scans)
	}
}

// TestImportWithoutJellyfin covers an import that waits for Jellyfin when
// Garner restarts without Jellyfin settings.
func TestImportWithoutJellyfin(t *testing.T) {
	ctx := context.Background()
	e, d := setup(t, german)
	e.jf.ignoreHints = true
	e.svc.Work(ctx)
	e.svc.jellyfin = nil
	e.svc.Work(ctx)
	if got, _ := e.st.GetDownload(ctx, d.ID); got.Import.State != store.ImportDone || !strings.Contains(got.Import.Detail, "not configured") {
		t.Errorf("import = %+v", got.Import)
	}
}

// TestImportWaitsForIdentification covers Jellyfin listing a file before it
// has identified it: that is not a mismatch.
func TestImportWaitsForIdentification(t *testing.T) {
	ctx := context.Background()
	e, d := setup(t, german)
	e.jf.unidentified = 1
	e.svc.Work(ctx)
	if got, _ := e.st.GetDownload(ctx, d.ID); got.Import.State != store.ImportScanning {
		t.Fatalf("while Jellyfin identifies: %+v", got.Import)
	}
	e.svc.Work(ctx)
	if got, _ := e.st.GetDownload(ctx, d.ID); got.Import.State != store.ImportDone {
		t.Errorf("after identification: %+v", got.Import)
	}
}

func TestImportRejectsWrongAudio(t *testing.T) {
	ctx := context.Background()
	japanese := german
	japanese.Audio = []inspect.Track{{Language: "jpn"}}
	e, d := setup(t, japanese)
	e.svc.Work(ctx)
	got, _ := e.st.GetDownload(ctx, d.ID)
	if got.Import.State != store.ImportRejected || len(e.rejected) != 1 || !strings.Contains(e.rejected[0], "no German track") {
		t.Errorf("import = %+v, rejected %q", got.Import, e.rejected)
	}
	if _, err := os.Stat(filepath.Join(e.share, target)); !os.IsNotExist(err) {
		t.Error("a rejected file reached the library")
	}
	if _, err := os.Stat(filepath.Join(e.share, jobDir)); !os.IsNotExist(err) {
		t.Error("the rejected release's files are still in SABnzbd's folder")
	}
}

// TestImportFailsAndRetries covers a missing library folder, such as an
// unmounted share: the import stops with a recovery step and never creates
// the folder; after the fix, a retry finishes it.
func TestImportFailsAndRetries(t *testing.T) {
	ctx := context.Background()
	e, d := setup(t, german)
	lib := filepath.Join(e.share, "library", "tv")
	os.Remove(lib)
	e.svc.Work(ctx)
	got, _ := e.st.GetDownload(ctx, d.ID)
	if got.Import.State != store.ImportFailed || !strings.Contains(got.Import.Detail, "is missing") || got.Import.Recovery == "" {
		t.Fatalf("import = %+v", got.Import)
	}
	if _, err := os.Stat(lib); !os.IsNotExist(err) {
		t.Error("Garner created the library folder")
	}
	if _, err := os.Stat(filepath.Join(e.share, jobDir, "x7f3a.mkv")); err != nil {
		t.Error("a failed import lost the downloaded file")
	}

	os.Mkdir(lib, 0o755)
	if _, err := e.svc.Retry(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	e.svc.Work(ctx)
	if got, _ := e.st.GetDownload(ctx, d.ID); got.Import.State != store.ImportDone {
		t.Errorf("after retry: %+v", got.Import)
	}
}

func TestImportKeepsForeignFiles(t *testing.T) {
	ctx := context.Background()
	e, d := setup(t, german)
	existing := filepath.Join(e.share, target)
	os.MkdirAll(filepath.Dir(existing), 0o755)
	os.WriteFile(existing, []byte("someone else's file"), 0o644)
	e.svc.Work(ctx)
	got, _ := e.st.GetDownload(ctx, d.ID)
	if got.Import.State != store.ImportFailed || !strings.Contains(got.Import.Detail, "not imported by Garner") {
		t.Errorf("import = %+v", got.Import)
	}
	if b, _ := os.ReadFile(existing); string(b) != "someone else's file" {
		t.Error("the existing file was overwritten")
	}
}

// TestImportReplacesOldVersion keeps one version per episode: a new import
// removes the file Garner imported before.
func TestImportReplacesOldVersion(t *testing.T) {
	ctx := context.Background()
	e, d := setup(t, german)
	old := filepath.Join(e.share, "library/tv/old/Harbor Precinct - S01E01.avi")
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.WriteFile(old, []byte("old"), 0o644)
	oldSub := strings.TrimSuffix(old, ".avi") + ".de.srt"
	os.WriteFile(oldSub, []byte("sub"), 0o644)
	e.st.SetEpisodeFile(ctx, store.EpisodeFile{EpisodeID: 101, Path: old, Size: 3}, 0, time.Now())
	e.svc.Work(ctx)
	if got, _ := e.st.GetDownload(ctx, d.ID); got.Import.State != store.ImportDone {
		t.Fatalf("import = %+v", got.Import)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("the previous version is still there")
	}
	if _, err := os.Stat(oldSub); !os.IsNotExist(err) {
		t.Error("the previous version's subtitle is still there")
	}
}

// TestImportTidies moves a release's subtitle files next to the video, with
// the language and forced mark Jellyfin reads, and removes the rest of
// SABnzbd's folder.
func TestImportTidies(t *testing.T) {
	ctx := context.Background()
	e, d := setup(t, german)
	job := filepath.Join(e.share, jobDir)
	os.WriteFile(filepath.Join(job, "x7f3a.ger.srt"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(job, "x7f3a.eng.forced.srt"), []byte("2"), 0o644)
	os.MkdirAll(filepath.Join(job, "Subs"), 0o755)
	os.WriteFile(filepath.Join(job, "Subs", "2_English.srt"), []byte("3"), 0o644)
	e.svc.Work(ctx)

	got, _ := e.st.GetDownload(ctx, d.ID)
	if got.Import.State != store.ImportDone || len(got.Import.Notes) != 1 || got.Import.Notes[0] != "moved 3 subtitle files next to the video" {
		t.Fatalf("import = %+v", got.Import)
	}
	video := strings.TrimSuffix(filepath.Join(e.share, target), ".mkv")
	for _, name := range []string{video + ".de.srt", video + ".en.forced.srt", video + ".en.srt"} {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("missing %s", filepath.Base(name))
		}
	}
	if _, err := os.Stat(job); !os.IsNotExist(err) {
		t.Error("SABnzbd's folder is still there")
	}
}

// TestImportKeepsUnassignedVideos leaves SABnzbd's folder in place when it
// holds a video no episode could be matched to.
func TestImportKeepsUnassignedVideos(t *testing.T) {
	ctx := context.Background()
	e, d := setup(t, german)
	job := filepath.Join(e.share, jobDir)
	os.Remove(filepath.Join(job, "x7f3a.mkv"))
	os.WriteFile(filepath.Join(job, "Harbor.Precinct.Night.Shift.S01E01.mkv"), make([]byte, 1000), 0o644)
	os.WriteFile(filepath.Join(job, "Harbor.Precinct.Night.Shift.Making.Of.mkv"), make([]byte, 900), 0o644)
	e.svc.Work(ctx)
	got, _ := e.st.GetDownload(ctx, d.ID)
	if got.Import.State != store.ImportDone || len(got.Import.Notes) != 1 || !strings.Contains(got.Import.Notes[0], "holds 1 video file that could not be assigned") {
		t.Fatalf("import = %+v", got.Import)
	}
	if _, err := os.Stat(filepath.Join(job, "Harbor.Precinct.Night.Shift.Making.Of.mkv")); err != nil {
		t.Error("the unassigned video was removed")
	}
}

// TestSubtitleOwner assigns subtitles by the video's full name, with the
// longest match winning, and keeps SABnzbd's folder when subtitles remain.
func TestSubtitleOwner(t *testing.T) {
	dir := t.TempDir()
	lib := t.TempDir()
	files := []store.ImportFile{
		{Source: filepath.Join(dir, "Episode1.mkv"), Target: filepath.Join(lib, "Show.S01E01.mkv")},
		{Source: filepath.Join(dir, "Episode10.mkv"), Target: filepath.Join(lib, "Show.S01E10.mkv")},
	}
	for _, name := range []string{"Episode10.en.srt", "Episode1.de.srt", "Unrelated.srt"} {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
	}
	g := guard.New(guard.Options{Library: config.Library{TV: lib}})
	job, err := g.Job(dir, filepath.Base(dir))
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{guard: g}
	s.moveSubtitles(context.Background(), dir, files, false, func(_ store.ImportFile, p string) (guard.File, error) { return g.InJob(job, p) })
	for _, want := range []string{"Show.S01E10.en.srt", "Show.S01E01.de.srt"} {
		if _, err := os.Stat(filepath.Join(lib, want)); err != nil {
			t.Errorf("missing %s", want)
		}
	}
	if n := subtitleFiles(dir); n != 1 {
		t.Errorf("%d subtitle files left; want the unrelated one", n)
	}
}

func TestVideoFilesSingleSample(t *testing.T) {
	f := filepath.Join(t.TempDir(), "show.sample.mkv")
	os.WriteFile(f, []byte("x"), 0o644)
	if got, err := videoFiles(f); err != nil || len(got) != 0 {
		t.Errorf("videoFiles(sample file) = %v, %v", got, err)
	}
}

func TestDotted(t *testing.T) {
	for in, want := range map[string]string{
		"Glass Orchard: After Winter's End":     "Glass.Orchard.After.Winters.End",
		"Harbor Twenty-Two":                     "Harbor.Twenty-Two",
		"Rogue!! -Iron Sky, Long Night-":        "Rogue.Iron.Sky.Long.Night",
		"Rogue!! Der Herr der Zerstörung":       "Rogue.Der.Herr.der.Zerstoerung",
		"Kōri no Mīnato":                        "Kori.no.Minato",
		"Lanterns & Tides":                      "Lanterns.and.Tides",
		"What? Why?":                            "What.Why",
		"灯籠の港":                                  "",
		"Epilogue / The Restless Harbor Family": "Epilogue.The.Restless.Harbor.Family",
	} {
		if got := dotted(in); got != want {
			t.Errorf("dotted(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTargetLength keeps names within maxName even with a long show name
// and a long release group, both of which come from outside.
func TestTargetLength(t *testing.T) {
	long := strings.Repeat("Very Long Title ", 20)
	c := showContext{
		profile:  selection.TV,
		data:     store.SeriesData{Series: store.Series{ID: 42, Name: long, Year: 2013}},
		episodes: map[int64]store.Episode{1: {ID: 1, NameEng: long, Numbers: map[string]store.Position{"official": {Season: 1, Number: 1}}}},
	}
	tags := releaseTags("Show.S01E01.German.1080p.WEB-"+strings.Repeat("Group", 30), "", german)
	got, err := c.target(config.Library{TV: "/lib/tv"}, []int64{1}, tags, ".mkv")
	if err != nil {
		t.Fatal(err)
	}
	if file, folder := filepath.Base(got), filepath.Base(filepath.Dir(filepath.Dir(got))); len(file) > maxName || len(folder) > maxName ||
		!strings.Contains(file, ".S01E01.") || !strings.HasSuffix(file, ".mkv") || !strings.HasSuffix(folder, ".[tvdbid-42]") {
		t.Errorf("target = %s (%d bytes)", got, len(file))
	}
}

func TestReleaseTags(t *testing.T) {
	p := inspect.Probe{Video: []inspect.Video{{Width: 1920, Height: 800}}, Audio: []inspect.Track{{Language: "ger"}, {Language: "jpn"}}}
	for title, want := range map[string]string{
		"Show.S01E01.German.DL.1080p.NF.WEB-DL.H264-GRP":     "NF.WEBDL-1080p.DE.JA-GRP",
		"Show.S01E01.REPACK.German.DL.1080p.BluRay.x264-GRP": "Bluray-1080p.Repack.DE.JA-GRP",
		"[Grp] Show - 01 v2 [1080p]":                         "1080p.v2.DE.JA-Grp",
	} {
		if got := releaseTags(title, "", p); got != want {
			t.Errorf("releaseTags(%q) = %q, want %q", title, got, want)
		}
	}
	untagged := inspect.Probe{Video: []inspect.Video{{Width: 1280, Height: 720}}, Audio: []inspect.Track{{Language: "und"}}}
	if got := releaseTags("Show.S01E01.720p.WEB-GRP", "German 720p", untagged); got != "WEB-720p.DE-GRP" {
		t.Errorf("untagged audio: %q", got)
	}
}

// TestImportPackKeepsBetter imports a pack's other episodes only where the
// pack is better than the file in the library: one version per episode.
func TestImportPackKeepsBetter(t *testing.T) {
	for _, tc := range []struct {
		existing string // tier of S01E02's file
		replaced bool
	}{
		{"German 1080p", false}, // as good as the pack: kept
		{"German 720p", true},   // worse: replaced
	} {
		ctx := context.Background()
		e, d := setup(t, german)
		job := filepath.Join(e.share, jobDir)
		os.Remove(filepath.Join(job, "x7f3a.mkv"))
		for _, name := range []string{"Harbor.Precinct.Night.Shift.S01E01.mkv", "Harbor.Precinct.Night.Shift.S01E02.mkv"} {
			os.WriteFile(filepath.Join(job, name), make([]byte, 1000), 0o644)
		}
		earlier, err := e.st.StartDownload(ctx, store.Download{EpisodeID: 102, Title: "Harbor.Precinct.S01E02-OLD", Link: "x",
			Verdict: "accept", Tier: tc.existing}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		old := filepath.Join(e.share, "library/tv/old/S01E02.mkv")
		os.MkdirAll(filepath.Dir(old), 0o755)
		os.WriteFile(old, []byte("old"), 0o644)
		e.st.SetEpisodeFile(ctx, store.EpisodeFile{EpisodeID: 102, Path: old, Size: 3, Tier: tc.existing}, earlier.ID, time.Now())

		e.svc.Work(ctx)
		got, _ := e.st.GetDownload(ctx, d.ID)
		f, _, _ := e.st.EpisodeFile(ctx, 102)
		kept := slices.ContainsFunc(got.Import.Notes, func(n string) bool { return strings.HasPrefix(n, "kept the existing file of S01E02") })
		if replaced := f.Path != old; replaced != tc.replaced || kept == tc.replaced {
			t.Errorf("existing %s: S01E02's file is %s, notes %q", tc.existing, f.Path, got.Import.Notes)
		}
		if _, err := os.Stat(old); (err == nil) == tc.replaced {
			t.Errorf("existing %s: the old file exists: %v", tc.existing, err == nil)
		}
		if _, err := os.Stat(job); !os.IsNotExist(err) {
			t.Errorf("existing %s: SABnzbd's folder is still there", tc.existing)
		}
	}
}

// TestImportPackRetryRemovesKeptFiles retries a pack's import whose move
// failed on a missing library folder: the pack's file of an episode whose
// library file was kept goes with the rest of SABnzbd's folder, as without
// the failure, and no note claims it could not be assigned (GRR-40).
func TestImportPackRetryRemovesKeptFiles(t *testing.T) {
	ctx := context.Background()
	e, d := setup(t, german)
	job := filepath.Join(e.share, jobDir)
	os.Remove(filepath.Join(job, "x7f3a.mkv"))
	for _, name := range []string{"Harbor.Precinct.Night.Shift.S01E01.mkv", "Harbor.Precinct.Night.Shift.S01E02.mkv"} {
		os.WriteFile(filepath.Join(job, name), make([]byte, 1000), 0o644)
	}
	earlier, err := e.st.StartDownload(ctx, store.Download{EpisodeID: 102, Title: "Harbor.Precinct.S01E02-OLD", Link: "x",
		Verdict: "accept", Tier: "German 1080p"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(e.share, "library/tv/old/S01E02.mkv")
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.WriteFile(old, []byte("old"), 0o644)
	e.st.SetEpisodeFile(ctx, store.EpisodeFile{EpisodeID: 102, Path: old, Size: 3, Tier: "German 1080p"}, earlier.ID, time.Now())

	// The share is unmounted while the import runs.
	library := filepath.Join(e.share, "library/tv")
	if err := os.Rename(library, library+".away"); err != nil {
		t.Fatal(err)
	}
	e.svc.Work(ctx)
	if got, _ := e.st.GetDownload(ctx, d.ID); got.Import.State != store.ImportFailed {
		t.Fatalf("with the library away: %+v", got.Import)
	}
	if err := os.Rename(library+".away", library); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Retry(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	e.svc.Work(ctx)

	got, _ := e.st.GetDownload(ctx, d.ID)
	if got.Import.State != store.ImportDone {
		t.Fatalf("after the retry: %+v", got.Import)
	}
	if f, _, _ := e.st.EpisodeFile(ctx, 102); f.Path != old {
		t.Errorf("S01E02's file is %s; the library's own was as good and should stay", f.Path)
	}
	if _, err := os.Stat(job); !os.IsNotExist(err) {
		t.Errorf("SABnzbd's folder is still there; notes %q", got.Import.Notes)
	}
}

// TestUpgradeCrash ends Garner after an upgrade recorded its file and
// before it removed the version it replaces. After a restart, the import
// must still remove that version (GRR-37).
func TestUpgradeCrash(t *testing.T) {
	ctx := context.Background()
	e, d := setup(t, german)
	earlier, err := e.st.StartDownload(ctx, store.Download{EpisodeID: 101, Title: "Harbor.Precinct.S01E01-OLD", Link: "x",
		Verdict: "accept", Tier: "German 720p"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(e.share, "library/tv/old/S01E01.mkv")
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.WriteFile(old, []byte("old"), 0o644)
	e.st.SetEpisodeFile(ctx, store.EpisodeFile{EpisodeID: 101, Path: old, Size: 3, Tier: "German 720p"}, earlier.ID, time.Now())

	type crash struct{}
	opts := e.svc.opts
	opts.Before = func(op, path string) {
		if op == "remove" && path == old {
			panic(crash{})
		}
	}
	crashing := New(e.st, e.svc.probe, e.svc.jellyfin, opts, slog.New(slog.DiscardHandler))
	func() {
		defer func() {
			if _, ok := recover().(crash); !ok {
				t.Fatal("the import did not reach the old version")
			}
		}()
		crashing.Work(ctx)
	}()

	// Garner starts again from its database.
	restarted := New(e.st, e.svc.probe, e.svc.jellyfin, e.svc.opts, slog.New(slog.DiscardHandler))
	restarted.Work(ctx)
	if got, _ := e.st.GetDownload(ctx, d.ID); got.Import.State == store.ImportMoving || got.Import.State == store.ImportFailed {
		t.Fatalf("after the restart: %+v", got.Import)
	}
	if _, err := os.Stat(old); err == nil {
		t.Errorf("the version the upgrade replaced is still there")
	}
}

// TestUpgradeRemovesLater replaces a version whose folder refuses writes,
// or is away as when its share is not mounted: the version stays planned
// for removal and goes once the folder is back to normal (GRR-37).
func TestUpgradeRemovesLater(t *testing.T) {
	for _, tc := range []struct {
		name        string
		block, free func(dir string) error
	}{
		{"refuses writes", func(dir string) error { return os.Chmod(dir, 0o555) }, func(dir string) error { return os.Chmod(dir, 0o755) }},
		{"away", func(dir string) error { return os.Rename(dir, dir+".away") }, func(dir string) error { return os.Rename(dir+".away", dir) }},
	} {
		ctx := context.Background()
		e, d := setup(t, german)
		earlier, err := e.st.StartDownload(ctx, store.Download{EpisodeID: 101, Title: "Harbor.Precinct.S01E01-OLD", Link: "x",
			Verdict: "accept", Tier: "German 720p"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(e.share, "library/tv/old")
		old := filepath.Join(dir, "S01E01.mkv")
		os.MkdirAll(dir, 0o755)
		os.WriteFile(old, []byte("old"), 0o644)
		e.st.SetEpisodeFile(ctx, store.EpisodeFile{EpisodeID: 101, Path: old, Size: 3, Tier: "German 720p"}, earlier.ID, time.Now())

		if err := tc.block(dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		e.svc.Work(ctx)
		if got, _ := e.st.GetDownload(ctx, d.ID); got.Import.State == store.ImportMoving || got.Import.State == store.ImportFailed {
			t.Fatalf("%s: %+v", tc.name, got.Import)
		}
		if tc.name == "away" {
			if _, err := os.Stat(filepath.Join(dir+".away", "S01E01.mkv")); err != nil {
				t.Errorf("%s: the old version went while its folder was away: %v", tc.name, err)
			}
			e.svc.Work(ctx) // still away: it waits
		}
		if err := tc.free(dir); err != nil {
			t.Fatal(err)
		}
		e.svc.Work(ctx)
		if _, err := os.Stat(old); err == nil {
			t.Errorf("%s: the version the upgrade replaced is still there", tc.name)
		}
		if changes, _ := e.st.FileChanges(ctx); len(changes) != 0 {
			t.Errorf("%s: plans left: %+v", tc.name, changes)
		}
	}
}
