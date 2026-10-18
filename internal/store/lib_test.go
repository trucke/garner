package store

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestRemoveOtherLibraries keeps the library folder of the Turso version in
// use and removes the rest: earlier versions and the layout before folders
// per version.
func TestRemoveOtherLibraries(t *testing.T) {
	libs := t.TempDir()
	for _, f := range []string{"v0.8.1/turso-go/aaaa/lib.so", "v0.7.0/turso-go/bbbb/lib.so", "turso-go/cccc/lib.so"} {
		path := filepath.Join(libs, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	removeOtherLibraries(libs, "v0.8.1", slog.New(slog.DiscardHandler))
	entries, err := os.ReadDir(libs)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if !slices.Equal(left, []string{"v0.8.1"}) {
		t.Fatalf("left %v, want only v0.8.1", left)
	}
	if _, err := os.Stat(filepath.Join(libs, "v0.8.1/turso-go/aaaa/lib.so")); err != nil {
		t.Errorf("the library in use is gone: %v", err)
	}
}
