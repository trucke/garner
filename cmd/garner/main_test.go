package main

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestNoServerCode keeps the server out of the terminal interface: of
// Garner's own packages it may use only those a client needs. A type the
// TUI needs from the server belongs in package api.
func TestNoServerCode(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	allowed := []string{"api", "buildinfo", "client", "connect", "profile", "tlsident", "token", "tui"}
	for _, pkg := range strings.Fields(string(out)) {
		if name, ok := strings.CutPrefix(pkg, "github.com/trucke/garner/internal/"); ok && !slices.Contains(allowed, name) {
			t.Errorf("garner depends on internal/%s, which is server code", name)
		}
	}
}
