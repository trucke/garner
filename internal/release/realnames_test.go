package release

import (
	"encoding/json"
	"os"
	"testing"
)

// TestRealReleaseCases checks the parser against real release names with
// reviewed expectations. They live in the private garner-research repository
// because indexer rules forbid republishing them. Run with
// GARNER_RELEASE_CASES=path/to/garner-research/tests/release-cases.json.
func TestRealReleaseCases(t *testing.T) {
	path := os.Getenv("GARNER_RELEASE_CASES")
	if path == "" {
		t.Skip("GARNER_RELEASE_CASES not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var real []struct {
		Set  string  `json:"set"`
		Name string  `json:"name"`
		Want summary `json:"want"`
	}
	if err := json.Unmarshal(data, &real); err != nil {
		t.Fatal(err)
	}
	if len(real) == 0 {
		t.Fatal("no cases")
	}
	for _, tc := range real {
		t.Run(tc.Name, func(t *testing.T) {
			if got := summarize(Parse(tc.Name)); got != tc.Want {
				t.Errorf("%s: Parse(%q)\n got: %+v\nwant: %+v", tc.Set, tc.Name, got, tc.Want)
			}
		})
	}
}
