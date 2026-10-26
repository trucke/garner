package selection

import (
	"slices"
	"testing"

	"github.com/trucke/garner/internal/profile"
	"github.com/trucke/garner/internal/release"
)

// TestProfileLanguages keeps the languages a profile may name equal to the
// ones the release parser knows: a profile language the parser does not
// read would never match a release.
func TestProfileLanguages(t *testing.T) {
	var known []string
	for _, l := range release.Languages() {
		known = append(known, string(l))
		if profile.LanguageName(string(l)) != l.Name() {
			t.Errorf("%s: profile names it %q, the parser %q", l, profile.LanguageName(string(l)), l.Name())
		}
	}
	if !slices.Equal(profile.Languages(), known) {
		t.Errorf("profile languages %v, the parser knows %v", profile.Languages(), known)
	}
}
