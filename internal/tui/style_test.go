package tui

import "testing"

// TestCleanUntrustedText checks that text from indexers and the server
// cannot drive the terminal or reorder what is shown.
func TestCleanUntrustedText(t *testing.T) {
	for in, want := range map[string]string{
		"Show.S01E01\x1b]52;c;ZXZpbA==\x07-EXAMPLE": "Show.S01E01]52;c;ZXZpbA==-EXAMPLE",
		"Show.S01E01\x1b[2J-EXAMPLE":                "Show.S01E01[2J-EXAMPLE",
		"Show.S01E01\u009b31m-EXAMPLE":              "Show.S01E0131m-EXAMPLE",
		"Show.\u202eLPMAXE-10E10S":                  "Show.LPMAXE-10E10S",
		"line one\nline two\ttab":                   "line one line two tab",
		"Laternenhafen – Nach dem Ende der Nacht":   "Laternenhafen – Nach dem Ende der Nacht",
	} {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
}
