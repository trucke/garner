package importer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trucke/garner/internal/store"
)

// TestDeleteFiles checks that deleting a show removes its videos, their
// subtitles and the folders left empty, but nothing Garner did not put
// there and never the library root.
func TestDeleteFiles(t *testing.T) {
	ctx := context.Background()
	e, _ := setup(t, german)
	root := filepath.Join(e.share, "library", "tv")
	show := filepath.Join(root, "Harbor.Precinct.2013.[tvdbid-42]")
	write := func(rel string) string {
		t.Helper()
		p := filepath.Join(show, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := write("Season.01/Harbor.Precinct.2013.S01E01.Pilot.WEBDL-1080p.DE-EXAMPLE.mkv")
	write("Season.01/Harbor.Precinct.2013.S01E01.Pilot.WEBDL-1080p.DE-EXAMPLE.de.srt")
	b := write("Season.02/Harbor.Precinct.2013.S02E01.Return.WEBDL-1080p.DE-EXAMPLE.mkv")
	notes := write("Season.02/notes.txt") // someone else's file
	outside := filepath.Join(root, "Other.Show.mkv")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for episode, path := range map[int64]string{101: a, 102: b} {
		if err := e.st.SetEpisodeFile(ctx, store.EpisodeFile{EpisodeID: episode, Path: path, Size: 1}, 0, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.ClaimFiles(ctx, []string{a, outside}); err == nil {
		t.Fatal("claimed a file the database does not record")
	}
	claimed, err := e.svc.ClaimFiles(ctx, []string{a, b})
	if err != nil {
		t.Fatal(err)
	}

	removed, problems := e.svc.DeleteFiles(ctx, 42, claimed)
	if len(removed) != 2 || len(problems) != 0 {
		t.Fatalf("removed %v, problems %v; want 2 files and none", removed, problems)
	}
	for _, gone := range []string{filepath.Join(show, "Season.01"), b} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s still exists", gone)
		}
	}
	for _, kept := range []string{notes, show, outside, root} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}

	// Once the other file is gone too, the empty folders go, the root stays.
	// A file that is gone already is no problem.
	os.Remove(notes)
	if removed, problems := e.svc.DeleteFiles(ctx, 42, claimed[1:]); len(removed) != 0 || len(problems) != 0 {
		t.Errorf("deleting a file that is gone: removed %v, problems %v", removed, problems)
	}
	if _, err := os.Stat(show); !os.IsNotExist(err) {
		t.Error("the empty show folder stayed")
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("the library root was removed: %v", err)
	}
}
