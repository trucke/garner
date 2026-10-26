package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckProfile checks profiles as their authors do before sharing
// them: a valid one lists its preferences, a broken one every problem.
func TestCheckProfile(t *testing.T) {
	valid := "garner_profile = 1\nname = \"Harbor fans\"\naudio = [\"de\", \"en\"]\nresolution = [\"1080p\", \"720p\"]\n"
	path := filepath.Join(t.TempDir(), "harbor.toml")
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, stdin string
		code              int
		want              []string
	}{
		{"file", path, "", 0, []string{"✓ Harbor fans\n  Preferences     German 1080p → German 720p → English 1080p → English 720p\n",
			"  Dual audio      prefer\n  Unnamed audio   reject\n  Upgrades        on\n"}},
		{"standard input", "-", valid, 0, []string{"✓ Harbor fans", "German 1080p → German 720p"}},
		{"warning", "-", strings.Replace(valid, `["de", "en"]`, `["de"]`, 1) + "priority = \"resolution\"\n", 0,
			[]string{"✓ Harbor fans", "! priority has no effect"}},
		{"problems", "-", "garner_profile = 1\naudio = [\"German\"]\n", 1,
			[]string{"✗ name is missing", `✗ audio: write "de" instead of "German"`, "✗ resolution is missing"}},
		{"too large", "-", valid + "# " + strings.Repeat("x", 70<<10), 1, []string{"✗ the profile has"}},
		{"missing file", filepath.Join(t.TempDir(), "none.toml"), "", 1, []string{"✗ open "}},
	} {
		var out bytes.Buffer
		code := checkProfile(tc.path, strings.NewReader(tc.stdin), &out)
		if code != tc.code {
			t.Errorf("%s: exit code %d, want %d\n%s", tc.name, code, tc.code, out.String())
		}
		for _, w := range tc.want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%s: output lacks %q:\n%s", tc.name, w, out.String())
			}
		}
	}
}
