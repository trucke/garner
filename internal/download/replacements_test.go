package download

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/sabnzbd"
	"github.com/trucke/garner/internal/store"
)

func setupSeasonReplacement(t *testing.T) (*store.Store, *Service, *fakeSAB) {
	t.Helper()
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	st, err := store.Open(ctx, t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SaveSeries(ctx, store.SeriesData{
		Series:   store.Series{ID: 42, Slug: "harbor-precinct", Name: "Harbor Precinct", DefaultOrder: "official"},
		Episodes: []store.Episode{{ID: 101}, {ID: 102}, {ID: 103}},
	}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	fake := &fakeSAB{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return st, New(st, sabnzbd.New(srv.URL, "key", srv.Client()), "tv", log), fake
}

// A season choice must not depend on whether its anchor episode happened
// to have been searched too, or quietly turn into a single-episode retry.
// Only another upload of the chosen release replaces it (GRR-64).
func TestReplaceSeasonCandidate(t *testing.T) {
	for _, tc := range []struct {
		name          string
		fromSeason    bool
		episodeSearch bool
	}{
		{"season search only", true, false},
		{"season search with an episode search", true, true},
		{"episode search with a season search", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, svc, fake := setupSeasonReplacement(t)
			searched := time.Now().Add(-time.Hour)
			const prefix = "Harbor.Precinct.S01.German.DL.1080p.WEB-"
			const episodeTitle = "Harbor.Precinct.S01E01.German.DL.1080p.WEB-EPISODE"
			first := store.Candidate{Title: prefix + "A", Posting: "a1", Verdict: "accept", Link: "https://indexer.invalid/pack-a", Episodes: []int64{101, 102}}
			if tc.episodeSearch {
				// The first release is identical in both caches. Its title or
				// link alone cannot say which results the user chose it from.
				if err := st.SaveSearch(ctx, store.Search{EpisodeID: 101, Searched: searched, Candidates: []store.Candidate{
					first, {Title: episodeTitle, Posting: "e1", Verdict: "accept", Link: "https://indexer.invalid/episode"},
					{Title: prefix + "A", Posting: "a-episode", Verdict: "accept", Link: "https://indexer.invalid/pack-a-episode"},
				}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.SaveSeasonSearch(ctx, 42, store.Search{Season: 1, Searched: searched, Candidates: []store.Candidate{
				first,
				{Title: prefix + "REJECTED", Posting: "r1", Verdict: "reject", Link: "https://indexer.invalid/rejected"},
				{Title: "Harbor.Precinct.S01E02-E03.German.DL.1080p.WEB-OTHER", Posting: "o1", Verdict: "accept", Link: "https://indexer.invalid/other", Episodes: []int64{102, 103}},
				{Title: prefix + "B", Posting: "b1", Verdict: "accept", Link: "https://indexer.invalid/pack-b", Episodes: []int64{101, 102}},
				{Title: prefix + "A", Posting: "a-season", Verdict: "accept", Link: "https://indexer.invalid/pack-a-season", Episodes: []int64{101, 102}},
				{Title: prefix + "FALLBACK", Posting: "f1", Verdict: "fallback", Link: "https://indexer.invalid/fallback"},
			}}, []int64{101, 101, 102, 101, 101, 101}); err != nil {
				t.Fatal(err)
			}
			var d store.Download
			var err error
			want, source := "a-season", 1
			if tc.fromSeason {
				search, loadErr := st.LoadSeasonSearch(ctx, 42, 1)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				d, err = svc.SeasonCandidate(ctx, search.Candidates[0].ID)
			} else {
				search, loadErr := st.LoadSearch(ctx, 101)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				d, err = svc.Candidate(ctx, search.Candidates[0].ID)
				want, source = "a-episode", 0
			}
			if err != nil {
				t.Fatal(err)
			}
			fake.finish(d.JobID, "Failed", "Aborted, cannot be completed", "")
			svc.Poll(ctx)
			attempts, err := st.EpisodeDownloads(ctx, 101)
			if err != nil || len(attempts) != 2 || attempts[0].Posting != want {
				t.Fatalf("submitted %q; want the other upload %q of the chosen release, from the original search scope", fake.submitted(), want)
			}
			if !attempts[0].Manual || attempts[0].State != store.Queued || attempts[0].SearchSeason != source {
				t.Fatal("the replacement lost the user's choice, its stored search scope or its queued state")
			}
			fake.finish(attempts[0].JobID, "Failed", "Aborted, cannot be completed", "")
			svc.Poll(ctx)
			if got := fake.submitted(); len(got) != 2 {
				t.Errorf("submitted %q after both uploads of the choice failed; no other release may replace it", got)
			}
		})
	}
}

// An unrelated episode search must not reset a season attempt's budget,
// even if it finds exactly the same packs while that attempt is running.
// Failures from both scopes share the budget: switching scopes must not
// buy more automatic attempts after repeated failures.
func TestSeasonReplacementLimit(t *testing.T) {
	ctx := context.Background()
	st, svc, fake := setupSeasonReplacement(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	var candidates []store.Candidate
	var owners []int64
	for i := range maxReplacements + 2 {
		// Uploads of one release: a chosen pack is replaced by those only.
		candidates = append(candidates, store.Candidate{
			Title: "Harbor.Precinct.S01.German.DL.1080p.WEB-PACK", Posting: fmt.Sprintf("p%d", i),
			Verdict: "accept", Link: fmt.Sprintf("https://indexer.invalid/pack-%d", i), Episodes: []int64{101, 102},
		})
		owners = append(owners, 101)
	}
	if err := st.SaveSeasonSearch(ctx, 42, store.Search{Season: 1, Searched: now.Add(-time.Hour), Candidates: candidates}, owners); err != nil {
		t.Fatal(err)
	}
	search, err := st.LoadSeasonSearch(ctx, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := st.StartDownload(ctx, store.Download{EpisodeID: 101, Manual: true, Verdict: "accept",
		Title: "Harbor.Precinct.S01E01.German.DL.1080p.WEB-PRIOR", Link: "https://indexer.invalid/prior"}, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	prior.State, prior.Failure = store.Failed, "missing articles"
	if err := st.UpdateDownload(ctx, prior, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SeasonCandidate(ctx, search.Candidates[0].ID); err != nil {
		t.Fatal(err)
	}
	for i := range maxReplacements {
		now = now.Add(time.Minute)
		if err := st.SaveSearch(ctx, store.Search{EpisodeID: 101, Searched: now, Candidates: candidates}); err != nil {
			t.Fatal(err)
		}
		fake.finish(fmt.Sprintf("job-%d", i+1), "Failed", "Aborted, cannot be completed", "")
		svc.Poll(ctx)
	}
	if got := fake.submitted(); len(got) != maxReplacements {
		t.Fatalf("submitted %d season releases, want the shared failure budget to stop at %d with one earlier episode failure", len(got), maxReplacements)
	}
	active, err := st.ActiveDownloads(ctx)
	if err != nil || len(active) != 0 {
		t.Fatal("the season retry limit left another download running")
	}
	if attempts, _ := st.EpisodeDownloads(ctx, 101); !strings.HasSuffix(attempts[0].Stopped, "failed in a row, so Garner stopped trying others.") {
		t.Errorf("stopped = %q", attempts[0].Stopped)
	}
}

// A release chosen by hand stays the user's choice: when it fails, Garner
// tries only other uploads of it, then stops, rather than download a
// release the user did not choose (GRR-64).
func TestReplaceKeepsTheChoice(t *testing.T) {
	ctx := context.Background()
	st, svc, fake := setupSeasonReplacement(t)
	const chosen = "Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP"
	if err := st.SaveSearch(ctx, store.Search{EpisodeID: 101, Searched: time.Now().Add(-time.Hour), Candidates: []store.Candidate{
		{Title: "Harbor.Precinct.S01E01.German.DL.1080p.BluRay-TOP", Posting: "top", Verdict: "accept", Tier: "German 1080p", Link: "https://indexer.invalid/top"},
		{Title: chosen, Posting: "first", Verdict: "accept", Tier: "German 1080p", Link: "https://indexer.invalid/first"},
		{Title: chosen, Posting: "second", Verdict: "accept", Tier: "German 1080p", Link: "https://indexer.invalid/second"},
		{Title: "Harbor.Precinct.S01E01.German.DL.720p.WEB-GRP", Posting: "720p", Verdict: "accept", Tier: "German 720p", Link: "https://indexer.invalid/720p"},
	}}); err != nil {
		t.Fatal(err)
	}
	search, err := st.LoadSearch(ctx, 101)
	if err != nil {
		t.Fatal(err)
	}
	d, err := svc.Candidate(ctx, search.Candidates[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	fake.finish(d.JobID, "Failed", "Aborted, cannot be completed", "")
	svc.Poll(ctx)
	attempts, err := st.EpisodeDownloads(ctx, 101)
	if err != nil || len(attempts) != 2 || attempts[0].Posting != "second" || !attempts[0].Manual {
		t.Fatalf("submitted %q; want the other upload of the chosen release, still the user's choice", fake.submitted())
	}
	// SABnzbd's words for a locked upload, as seen in production.
	fake.finish(attempts[0].JobID, "Failed", "Aborted, encryption detected", "")
	svc.Poll(ctx)
	if got := fake.submitted(); len(got) != 2 {
		t.Errorf("submitted %q after both uploads of the choice failed; want Garner to stop", got)
	}
	// The user learns why nothing else started.
	if attempts, _ := st.EpisodeDownloads(ctx, 101); !strings.HasPrefix(attempts[0].Stopped, "No other upload of this release is left") {
		t.Errorf("stopped = %q", attempts[0].Stopped)
	}
}

// A release Garner chose on its own is replaced only by one that meets the
// same preference: after both German 1080p releases fail, Garner stops
// rather than fall back to German 720p or English (GRR-64).
func TestReplaceKeepsThePreference(t *testing.T) {
	ctx := context.Background()
	st, svc, fake := setupSeasonReplacement(t)
	if err := st.SaveSearch(ctx, store.Search{EpisodeID: 101, Searched: time.Now().Add(-time.Hour), Candidates: []store.Candidate{
		{Title: "Harbor.Precinct.S01E01.German.DL.1080p.BluRay-ONE", Posting: "one", Verdict: "accept", Tier: "German 1080p", Link: "https://indexer.invalid/one"},
		{Title: "Harbor.Precinct.S01E01.German.DL.1080p.WEB-TWO", Posting: "two", Verdict: "accept", Tier: "German 1080p", Link: "https://indexer.invalid/two"},
		{Title: "Harbor.Precinct.S01E01.German.DL.720p.BluRay-ONE", Posting: "720p", Verdict: "accept", Tier: "German 720p", Link: "https://indexer.invalid/720p"},
		{Title: "Harbor.Precinct.S01E01.1080p.BluRay-THREE", Posting: "english", Verdict: "accept", Tier: "English 1080p", Link: "https://indexer.invalid/english"},
	}}); err != nil {
		t.Fatal(err)
	}
	d, err := svc.Accepted(ctx, 101)
	if err != nil {
		t.Fatal(err)
	}
	fake.finish(d.JobID, "Failed", "Aborted, cannot be completed", "")
	svc.Poll(ctx)
	fake.finish("job-2", "Failed", "Aborted, cannot be completed", "")
	svc.Poll(ctx)
	want := []string{"Harbor.Precinct.S01E01.German.DL.1080p.BluRay-ONE", "Harbor.Precinct.S01E01.German.DL.1080p.WEB-TWO"}
	if got := fake.submitted(); !slices.Equal(got, want) {
		t.Errorf("submitted %q; want only the German 1080p releases %q", got, want)
	}
	attempts, _ := st.EpisodeDownloads(ctx, 101)
	if !strings.HasPrefix(attempts[0].Stopped, "No other release meets German 1080p") || attempts[1].Stopped != "" {
		t.Errorf("stopped = %q, then %q; want why only on the last attempt", attempts[0].Stopped, attempts[1].Stopped)
	}
}
