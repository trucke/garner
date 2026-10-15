package importer

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDeleteFiles checks that deleting a show removes its videos, their
// subtitles and the folders left empty, but nothing Garner did not put
// there and never the library root.
func TestDeleteFiles(t *testing.T) {
	root := t.TempDir()
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

	removed, problems := DeleteFiles(42, []string{a, b, filepath.Join(show, "Season.01/already-gone.mkv")})
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
	os.Remove(notes)
	DeleteFiles(42, []string{b})
	if _, err := os.Stat(show); !os.IsNotExist(err) {
		t.Error("the empty show folder stayed")
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("the library root was removed: %v", err)
	}
}
