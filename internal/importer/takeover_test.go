package importer

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestTakeOver takes over a show's files from an older library folder that
// Jellyfin holds the show in, keeps the better of two files for one
// episode, leaves what it cannot assign, and then renames the files into
// Garner's folder with their subtitles.
func TestTakeOver(t *testing.T) {
	ctx := context.Background()
	e, _ := setup(t, german)
	old := filepath.Join(e.share, "library/tv/Harbor Precinct Night Shift (2013)")
	season := filepath.Join(old, "Season 1")
	os.MkdirAll(season, 0o755)
	files := map[string]int{
		"Harbor Precinct Night Shift - S01E01 - Pilot.mkv": 1000,
		"S01E02 - What Why.mkv":                            900, // no show title, as is common in a show's folder
		"S01E02 - What Why.en.srt":                         10,
		"Harbor Precinct Night Shift - S01E02.mkv":         500, // smaller than the other file of S01E02
		"Behind the scenes.mkv":                            300,
	}
	for name, size := range files {
		os.WriteFile(filepath.Join(season, name), make([]byte, size), 0o644)
	}
	os.WriteFile(filepath.Join(old, "tvshow.nfo"), []byte("x"), 0o644)
	// Jellyfin knows the show in the old folder.
	e.jf.created = []string{"/data/library/tv/Harbor Precinct Night Shift (2013)/Season 1/x.mkv"}

	got, err := e.svc.TakeOver(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.Files != 2 || got.Episodes != 2 || got.Renamable != 2 || len(got.Folders) != 1 || got.Folders[0] != old {
		t.Fatalf("take-over = %+v", got)
	}
	reasons := map[string]string{}
	for _, s := range got.Skipped {
		reasons[filepath.Base(s.Path)] = s.Reason
	}
	if reasons["Harbor Precinct Night Shift - S01E02.mkv"] != "a better file holds S01E02" || reasons["Behind the scenes.mkv"] == "" || len(reasons) != 2 {
		t.Errorf("skipped %q", reasons)
	}
	for id, name := range map[int64]string{101: "Harbor Precinct Night Shift - S01E01 - Pilot.mkv", 102: "S01E02 - What Why.mkv"} {
		f, found, _ := e.st.EpisodeFile(ctx, id)
		if !found || f.Path != filepath.Join(season, name) || f.Tier != "German 1080p" {
			t.Errorf("episode %d: %+v, %v", id, f, found)
		}
	}
	// Looking again takes nothing twice.
	if again, err := e.svc.TakeOver(ctx, 42); err != nil || again.Files != 0 || again.Renamable != 2 {
		t.Errorf("again: %+v, %v", again, err)
	}

	renamed, err := e.svc.Rename(ctx, 42)
	if err != nil || renamed.Files != 2 {
		t.Fatalf("rename = %+v, %v", renamed, err)
	}
	own := filepath.Join(e.share, "library/tv/Harbor.Precinct.Night.Shift.2013.[tvdbid-42]/Season.01")
	for _, id := range []int64{101, 102} {
		f, _, _ := e.st.EpisodeFile(ctx, id)
		if filepath.Dir(f.Path) != own || !strings.HasPrefix(filepath.Base(f.Path), "Harbor.Precinct.Night.Shift.2013.S01E0") {
			t.Errorf("episode %d is at %s", id, f.Path)
		} else if _, err := os.Stat(f.Path); err != nil {
			t.Errorf("episode %d: %v", id, err)
		}
	}
	f, _, _ := e.st.EpisodeFile(ctx, 102)
	if _, err := os.Stat(strings.TrimSuffix(f.Path, ".mkv") + ".en.srt"); err != nil {
		t.Errorf("the subtitle did not follow its video: %v", err)
	}
	// The other files stay in the old folder, and the notes say so.
	if _, err := os.Stat(filepath.Join(season, "Behind the scenes.mkv")); err != nil {
		t.Errorf("a file Garner does not manage moved: %v", err)
	}
	if !slices.ContainsFunc(renamed.Notes, func(n string) bool { return strings.HasPrefix(n, old+" stays with 3 other files") }) {
		t.Errorf("notes %q", renamed.Notes)
	}
	if again, _ := e.svc.TakeOver(ctx, 42); again.Renamable != 0 {
		t.Errorf("after renaming, %d files are still renamable", again.Renamable)
	}
}

// TestTakeOverOwnNames takes back files Garner named, as after a show was
// deleted with its files kept: the number after the show's name decides,
// whatever numbers the episode title holds.
func TestTakeOverOwnNames(t *testing.T) {
	for name, want := range map[string][]int64{
		"Harbor.Precinct.Night.Shift.2013.S01E01.1.23.45.WEB-1080p.DE-GRP.mkv": {101},
		"Harbor.Precinct.Night.Shift.2013.S01E01-E02.Pilot.WEB-1080p.DE.mkv":   {101, 102},
	} {
		ctx := context.Background()
		e, _ := setup(t, german)
		own := filepath.Join(e.share, "library/tv/Harbor.Precinct.Night.Shift.2013.[tvdbid-42]/Season.01")
		os.MkdirAll(own, 0o755)
		os.WriteFile(filepath.Join(own, name), make([]byte, 100), 0o644)
		got, err := e.svc.TakeOver(ctx, 42)
		if err != nil || got.Files != 1 || got.Episodes != len(want) || got.Renamable != 0 {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
		for _, id := range want {
			if f, found, _ := e.st.EpisodeFile(ctx, id); !found || filepath.Base(f.Path) != name {
				t.Errorf("%s: episode %d has %+v", name, id, f)
			}
		}
	}
}
