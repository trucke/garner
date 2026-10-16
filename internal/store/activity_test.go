package store

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"
)

// TestActivity lists each episode's latest download: running ones and
// imports waiting for the user always, finished ones within the window.
func TestActivity(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pos := func(n int) map[string]Position { return map[string]Position{"official": {Season: 1, Number: n}} }
	if err := st.SaveSeries(ctx, SeriesData{
		Series: Series{ID: 42, Slug: "x", Name: "Harbor Precinct", OriginalLanguage: "eng", OriginalCountry: "usa",
			Status: "Ended", DefaultOrder: "official", TVDBUpdated: "2026-01-01", Profile: "tv"},
		Episodes: []Episode{{ID: 101, Numbers: pos(1)}, {ID: 102, Numbers: pos(2)}, {ID: 103, Numbers: pos(3)}, {ID: 104, Numbers: pos(4)}},
	}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old, recent := now.Add(-72*time.Hour), now.Add(-time.Hour)
	download := func(episode int64, title string, at time.Time, state, imported string) {
		t.Helper()
		d, err := st.StartDownload(ctx, Download{EpisodeID: episode, Title: title, Link: "x", Verdict: "accept"}, at)
		if err != nil {
			t.Fatal(err)
		}
		d.State, d.JobID = state, title
		if err := st.UpdateDownload(ctx, d, at); err != nil {
			t.Fatal(err)
		}
		if imported != "" {
			if err := st.UpdateImport(ctx, d.ID, Import{State: imported}, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	download(101, "replaced", old, Completed, ImportDone)
	download(101, "running", now, Downloading, "")
	download(102, "imported long ago", old, Completed, ImportDone)
	download(103, "import failed long ago", old, Completed, ImportFailed)
	download(104, "imported an hour ago", recent, Completed, ImportDone)

	items, err := st.Activity(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Download.Title)
		if it.SeriesName != "Harbor Precinct" || it.Season != 1 || it.Number == 0 {
			t.Errorf("%s: %+v", it.Download.Title, it)
		}
	}
	slices.Sort(got)
	if want := []string{"import failed long ago", "imported an hour ago", "running"}; !slices.Equal(got, want) {
		t.Errorf("activity = %q, want %q", got, want)
	}
}
