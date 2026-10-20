package selection

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/match"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/store"
)

// TestRealSelectCases checks verdicts on real release names reviewed in
// GRR-1. They live in the private garner-research repository with
// catalogue snapshots (catalog/<series>.json next to the cases file). Run
// with GARNER_SELECT_CASES=path/to/garner-research/tests/select-cases.json.
func TestRealSelectCases(t *testing.T) {
	path := os.Getenv("GARNER_SELECT_CASES")
	if path == "" {
		t.Skip("GARNER_SELECT_CASES not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Series    int64                      `json:"series"`
		Profile   string                     `json:"profile"`
		Rules     map[release.Service]string `json:"rules"`
		Name      string                     `json:"name"`
		Want      string                     `json:"want"`
		Languages []string                   `json:"languages"`
		Published string                     `json:"published"`
		Verdict   Verdict                    `json:"verdict"`
		Tier      string                     `json:"tier"`
		Reason    string                     `json:"reason"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	catalog := map[int64]store.SeriesData{}
	for _, tc := range cases {
		d, ok := catalog[tc.Series]
		if !ok {
			raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), "catalog", fmt.Sprintf("%d.json", tc.Series)))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatal(err)
			}
			catalog[tc.Series] = d
		}
		t.Run(tc.Name, func(t *testing.T) {
			profile := Profiles{}.Named(tc.Profile)
			show := match.NewShow(d, match.Options{Anime: profile.Name == AnimeName, ServiceOrders: tc.Rules})
			var published time.Time
			if tc.Published != "" {
				published, _ = time.Parse(time.DateOnly, tc.Published)
			}
			original, _ := release.LanguageNamed(d.Series.OriginalLanguage)
			ctx := Context{Profile: profile, Show: show, Want: episodeAt(t, d, tc.Want), Original: original}
			claims := release.Parse(tc.Name)
			e := Evaluate(ctx, Release{Claims: claims, Match: show.Match(claims, published), IndexerLanguages: IndexerLanguages(tc.Languages)})
			reasonOK := slices.ContainsFunc(e.Reasons, func(r string) bool { return strings.Contains(r, tc.Reason) })
			if e.Verdict != tc.Verdict || tc.Tier != "" && e.Tier != tc.Tier || !reasonOK {
				t.Errorf("%s\n got: %s %q %q\nwant: %s %q with %q", tc.Name, e.Verdict, e.Tier, e.Reasons, tc.Verdict, tc.Tier, tc.Reason)
			}
		})
	}
}

// episodeAt finds the episode at an aired-order position such as S01E01.
func episodeAt(t *testing.T, d store.SeriesData, label string) int64 {
	t.Helper()
	var p store.Position
	if _, err := fmt.Sscanf(label, "S%02dE%d", &p.Season, &p.Number); err != nil {
		t.Fatalf("bad position %q", label)
	}
	for _, e := range d.Episodes {
		if e.Numbers["official"] == p {
			return e.ID
		}
	}
	t.Fatalf("no episode %s", label)
	return 0
}
