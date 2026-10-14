package connect

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trucke/garner/internal/token"
)

// TestSaveKeepsTokenPrivate checks that the stored code, which holds the
// token, is readable only by the user and loads back unchanged.
func TestSaveKeepsTokenPrivate(t *testing.T) {
	plain, _, _ := token.New()
	want := Code{URL: "https://garner.example:7979", Token: plain, Pin: "pin"}
	path := filepath.Join(t.TempDir(), "garner", "connection")
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("connection file mode %o, want 600", mode)
	}
	got, err := Load(path)
	if err != nil || got != want {
		t.Fatalf("Load: %+v, %v; want %+v", got, err, want)
	}
}
