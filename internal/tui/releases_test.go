package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/api"
)

// TestReleasesTried shows which of two postings of the same release Garner
// tried and what came of it, and tells them apart by their upload time
// (GRR-43). The Tried column appears only once Garner tried one.
func TestReleasesTried(t *testing.T) {
	const title = "Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP"
	first := time.Date(2021, 9, 10, 4, 26, 0, 0, time.Local)
	second := time.Date(2021, 9, 8, 12, 28, 0, 0, time.Local)
	failed := &api.Download{State: api.Failed, Failure: "Aborted, cannot be completed", FailureSummary: "102 articles missing",
		Updated: time.Now()}
	s := &releasesScreen{sess: &session{}, back: &showScreen{name: "Harbor Precinct"}, search: &api.Search{Candidates: []api.Candidate{
		{ID: 1, Rank: 1, Indexer: "One", Title: title, Size: 725e6, Published: &first, Verdict: "accept", Tier: "German 1080p",
			Audio: []string{"de"}, Reasons: []string{"German audio"}, Attempt: failed},
		{ID: 2, Rank: 2, Indexer: "One", Title: title, Size: 698e6, Published: &second, Verdict: "accept", Tier: "German 1080p",
			Audio: []string{"de"}, Reasons: []string{"German audio"}},
	}}, selected: 1}

	lines := strings.Split(ansi.Strip(s.view(150, 30)), "\n")
	var head, tried, untried string
	for _, l := range lines {
		switch {
		case strings.Contains(l, "Release") && strings.Contains(l, "Preference"):
			head = l
		case strings.Contains(l, "725 MB"):
			tried = l
		case strings.Contains(l, "698 MB"):
			untried = l
		}
	}
	if !strings.Contains(head, "Tried") || !strings.Contains(tried, "failed") || strings.Contains(untried, "failed") {
		t.Errorf("rows do not tell the tried posting apart:\n%s\n%s\n%s", head, tried, untried)
	}
	why := strings.Join(lines, "\n")
	for _, want := range []string{"Uploaded  10 Sep 2021, 04:26", "Failed", "102 articles missing", "does not pick this upload again"} {
		if !strings.Contains(why, want) {
			t.Errorf("the selected posting's details lack %q:\n%s", want, why)
		}
	}

	s.selected = 2
	if why := ansi.Strip(s.view(150, 30)); !strings.Contains(why, "Uploaded  8 Sep 2021, 12:28") || strings.Contains(why, "Failed") {
		t.Errorf("the untried posting's details:\n%s", why)
	}
	s.search.Candidates[0].Attempt = nil
	if view := ansi.Strip(s.view(150, 30)); strings.Contains(view, "Tried") {
		t.Errorf("a Tried column without tried releases:\n%s", view)
	}
}
