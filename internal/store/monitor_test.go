package store

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"
)

// TestWanted checks which missing episodes the monitor gets under each
// mode, and when it searches each again: counted from its own searches,
// whatever searches someone ran to look at releases.
func TestWanted(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
	added := time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local)
	ep := func(id int64, aired string, season, number int) Episode {
		return Episode{ID: id, Aired: aired, Numbers: map[string]Position{"official": {Season: season, Number: number}}}
	}
	save := func(id int64, mode string, episodes ...Episode) {
		t.Helper()
		if err := st.SaveSeries(ctx, SeriesData{
			Series:   Series{ID: id, Slug: "s", Name: "Show", OriginalLanguage: "deu", OriginalCountry: "deu", Status: "Continuing", DefaultOrder: "official", TVDBUpdated: "x", Profile: "tv", Monitor: mode},
			Episodes: episodes,
		}, false, added); err != nil {
			t.Fatal(err)
		}
	}
	search := func(episode int64, at time.Time, outcome string) {
		t.Helper()
		if err := st.SaveSearch(ctx, Search{EpisodeID: episode, Searched: at, Outcome: outcome, Summary: outcome}); err != nil {
			t.Fatal(err)
		}
	}
	monitored := func(episode int64, at time.Time, outcome string) {
		t.Helper()
		if err := st.SaveMonitorSearch(ctx, episode, at, outcome); err != nil {
			t.Fatal(err)
		}
	}
	download := func(episode int64, at time.Time, state string) {
		t.Helper()
		d, err := st.StartDownload(ctx, Download{EpisodeID: episode, Title: "Show.S01E01.German.1080p.WEB-EXAMPLE", Indexer: "nzb-one", Link: "x"}, at)
		if err != nil {
			t.Fatal(err)
		}
		if state == Cancelled {
			_, err = st.CancelDownload(ctx, d.ID, "cancelled", at)
		} else {
			d.State = state
			err = st.UpdateDownload(ctx, d, at)
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	// New episodes since the show was added on 1 Oct.
	save(1, MonitorNew,
		ep(100, "2026-10-05", 0, 1), // a special
		ep(101, "2026-09-24", 1, 1), // aired before it was added
		ep(102, "2026-10-01", 1, 2), // aired the day it was added, never searched
		ep(103, "2026-10-07", 1, 3), // searched by the monitor without a result, later by someone looking
		ep(104, "2026-10-08", 1, 4), // found, but the download failed later
		ep(105, "2026-10-09", 1, 5), // downloading
		ep(106, "2026-10-10", 1, 6), // in the library
		ep(107, "2026-10-20", 1, 7), // not aired yet
		ep(108, "2026-10-11", 1, 8), // searched only by someone looking
		ep(109, "2026-10-12", 1, 9)) // its download was cancelled
	monitored(103, now.Add(-24*time.Hour), "nothing")
	search(103, now.Add(-time.Hour), "found")
	monitored(104, now.Add(-26*time.Hour), "found")
	search(104, now.Add(-26*time.Hour), "found")
	download(104, now.Add(-25*time.Hour), Failed)
	search(108, now.Add(-time.Hour), "found")
	monitored(109, now.Add(-3*time.Hour), "found")
	download(109, now.Add(-2*time.Hour), Cancelled)
	download(105, now.Add(-time.Hour), Downloading)
	if err := st.SetEpisodeFile(ctx, EpisodeFile{EpisodeID: 106, Path: "/lib/Show.S01E06.mkv", Size: 1}, 0, now); err != nil {
		t.Fatal(err)
	}
	// Choosing the same mode again keeps its start.
	if err := st.SetMonitor(ctx, 1, MonitorNew, now); err != nil {
		t.Fatal(err)
	}
	// All missing, from long ago; its last search failed.
	save(2, MonitorAll, ep(201, "2020-01-01", 1, 1))
	monitored(201, now.Add(-30*time.Minute), "failed")
	// Off.
	save(3, MonitorNew, ep(301, "2026-10-05", 1, 1))
	if err := st.SetMonitor(ctx, 3, MonitorOff, now); err != nil {
		t.Fatal(err)
	}

	wanted, err := st.Wanted(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]time.Time{}
	for _, w := range wanted {
		got[w.EpisodeID] = w.Next
	}
	want := map[int64]time.Time{
		102: {},                                          // at once
		103: now.Add(-24 * time.Hour).Add(6 * time.Hour), // 6 hours after its own search: aired within two weeks
		104: now.Add(-25 * time.Hour).Add(6 * time.Hour), // 6 hours after the failure, not the search
		108: {},                                          // at once: the monitor never searched it
		201: now.Add(-30 * time.Minute).Add(time.Hour),   // an hour after a failed search
	}
	if !maps.EqualFunc(got, want, time.Time.Equal) {
		ids := slices.Sorted(maps.Keys(got))
		t.Fatalf("wanted %v: %v\nwant %v", ids, got, want)
	}

	// The episode states carry the next search only while the monitor runs,
	// and a cancelled episode carries its download instead.
	for _, monitoring := range []bool{true, false} {
		_, states, err := st.ShowStatus(ctx, 1, now, monitoring)
		if err != nil {
			t.Fatal(err)
		}
		state := func(id int64) EpisodeStatus {
			return states[slices.IndexFunc(states, func(s EpisodeStatus) bool { return s.EpisodeID == id })]
		}
		if next := state(103).NextRetry; monitoring != (next != nil) || next != nil && !next.Equal(want[103]) {
			t.Errorf("monitoring %v: next search %v", monitoring, next)
		}
		if c := state(109); c.NextRetry != nil || c.Download == nil || c.Download.State != Cancelled {
			t.Errorf("monitoring %v: cancelled episode %+v", monitoring, c)
		}
	}
}
