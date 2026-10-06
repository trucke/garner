package match

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/store"
)

// TestRealMatchCases checks matching against real release names with
// reviewed expectations. They live in the private garner-research
// repository with snapshots of the catalogue (catalog/<series>.json next to
// the cases file), because indexer rules forbid republishing the names. Run
// with GARNER_MATCH_CASES=path/to/garner-research/tests/match-cases.json.
func TestRealMatchCases(t *testing.T) {
	path := os.Getenv("GARNER_MATCH_CASES")
	if path == "" {
		t.Skip("GARNER_MATCH_CASES not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Series    int64                      `json:"series"`
		Anime     bool                       `json:"anime"`
		Rules     map[release.Service]string `json:"rules"`
		Name      string                     `json:"name"`
		Published string                     `json:"published"`
		Want      string                     `json:"want"`
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
			var published time.Time
			if tc.Published != "" {
				if published, err = time.Parse(time.DateOnly, tc.Published); err != nil {
					t.Fatal(err)
				}
			}
			show := NewShow(d, Options{Anime: tc.Anime, ServiceOrders: tc.Rules})
			m := show.Match(release.Parse(tc.Name), published)
			if got := summarize(show, m); got != tc.Want || !strings.Contains(m.Reason, tc.Reason) {
				t.Errorf("%s\n got: %s (%s)\nwant: %s (%s)\nnotes: %q", tc.Name, got, m.Reason, tc.Want, tc.Reason, m.Notes)
			}
		})
	}
}

// summarize renders a match compactly: "S01E03", "S01E158-S01E170 (13)",
// "season S06E01-S06E10 (10)", "unknown" or "extra", plus " uncertain".
func summarize(s *Show, m Match) string {
	switch m.Kind {
	case Unknown, Extra:
		return m.Kind.String()
	}
	out := s.Label(m.Episodes[0])
	if n := len(m.Episodes); n > 1 {
		out = fmt.Sprintf("%s-%s (%d)", out, s.Label(m.Episodes[n-1]), n)
	}
	if m.Kind == Season {
		out = "season " + out
	}
	if m.Uncertain {
		out += " uncertain"
	}
	return out
}
