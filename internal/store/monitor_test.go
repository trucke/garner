package store

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"
)

// TestWanted checks which missing episodes and upgrades the monitor gets
// under each mode, and when it searches each again: counted from its own
// searches, whatever searches someone ran to look at releases, and for an
// upgrade from the arrival of the file.
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
	sched := Schedule{
		Recent: 14 * 24 * time.Hour, MissingRecent: 6 * time.Hour, MissingOlder: 24 * time.Hour,
		Upgrades: true, UpgradeRecent: 24 * time.Hour, UpgradeOlder: 7 * 24 * time.Hour,
		Best: func(string) string { return "German 1080p" },
	}
	// imported gives an episode a file from a release in tier, imported at.
	imported := func(episode int64, tier string, at time.Time) {
		t.Helper()
		d, err := st.StartDownload(ctx, Download{EpisodeID: episode, Title: "Show.S01E01.German.WEB-EXAMPLE", Indexer: "nzb-one", Link: "x", Tier: tier}, at)
		if err != nil {
			t.Fatal(err)
		}
		d.State = Completed
		if err := st.UpdateDownload(ctx, d, at); err != nil {
			t.Fatal(err)
		}
		if err := st.SetEpisodeFile(ctx, EpisodeFile{EpisodeID: episode, Path: fmt.Sprintf("/lib/%d.mkv", episode), Size: 1, Tier: tier}, d.ID, at); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateImport(ctx, d.ID, Import{State: ImportDone}, at); err != nil {
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
		ep(100, "2026-10-05", 0, 1),  // a special
		ep(101, "2026-09-24", 1, 1),  // aired before it was added
		ep(102, "2026-10-01", 1, 2),  // aired the day it was added, never searched
		ep(103, "2026-10-07", 1, 3),  // searched by the monitor without a result, later by someone looking
		ep(104, "2026-10-08", 1, 4),  // found, but the download failed later
		ep(105, "2026-10-09", 1, 5),  // downloading
		ep(106, "2026-10-10", 1, 6),  // in the library
		ep(107, "2026-10-20", 1, 7),  // not aired yet
		ep(108, "2026-10-11", 1, 8),  // searched only by someone looking
		ep(109, "2026-10-12", 1, 9),  // its download was cancelled
		ep(110, "2026-10-06", 1, 10), // in the library in 720p
		ep(111, "2026-10-06", 1, 11), // in the library in 1080p, the best tier
		ep(112, "2026-10-06", 1, 12)) // in the library in 720p, and searched by the monitor since
	monitored(103, now.Add(-24*time.Hour), "nothing")
	search(103, now.Add(-time.Hour), "found")
	monitored(104, now.Add(-26*time.Hour), "found")
	search(104, now.Add(-26*time.Hour), "found")
	download(104, now.Add(-25*time.Hour), Failed)
	search(108, now.Add(-time.Hour), "found")
	monitored(109, now.Add(-3*time.Hour), "found")
	download(109, now.Add(-2*time.Hour), Cancelled)
	download(105, now.Add(-time.Hour), Downloading)
	// A file of unknown tier is not upgraded.
	if err := st.SetEpisodeFile(ctx, EpisodeFile{EpisodeID: 106, Path: "/lib/Show.S01E06.mkv", Size: 1}, 0, now); err != nil {
		t.Fatal(err)
	}
	imported(110, "German 720p", now.Add(-30*time.Hour))
	imported(111, "German 1080p", now.Add(-30*time.Hour))
	imported(112, "German 720p", now.Add(-40*time.Hour))
	monitored(112, now.Add(-2*time.Hour), "nothing")
	// Choosing the same mode again keeps its start.
	if err := st.SetMonitor(ctx, 1, MonitorNew, now); err != nil {
		t.Fatal(err)
	}
	// All missing, from long ago; its last search failed.
	save(2, MonitorAll, ep(201, "2020-01-01", 1, 1), ep(202, "2020-01-08", 1, 2))
	monitored(201, now.Add(-30*time.Minute), "failed")
	imported(202, "German 720p", now.Add(-8*24*time.Hour))
	// Off.
	save(3, MonitorNew, ep(301, "2026-10-05", 1, 1), ep(302, "2026-10-05", 1, 2))
	imported(302, "German 720p", now.Add(-8*24*time.Hour))
	if err := st.SetMonitor(ctx, 3, MonitorOff, now); err != nil {
		t.Fatal(err)
	}

	wanted := func(sc Schedule) map[int64]time.Time {
		t.Helper()
		list, err := st.Wanted(ctx, now, sc)
		if err != nil {
			t.Fatal(err)
		}
		got := map[int64]time.Time{}
		for _, w := range list {
			got[w.EpisodeID] = w.Next
			if upgrade := w.EpisodeID == 110 || w.EpisodeID == 112 || w.EpisodeID == 202; upgrade != (w.Upgrade == "German 720p") {
				t.Errorf("episode %d upgrades %q", w.EpisodeID, w.Upgrade)
			}
		}
		return got
	}
	want := map[int64]time.Time{
		102: {},                                                   // at once
		103: now.Add(-24 * time.Hour).Add(6 * time.Hour),          // 6 hours after its own search: aired within two weeks
		104: now.Add(-25 * time.Hour).Add(6 * time.Hour),          // 6 hours after the failure, not the search
		108: {},                                                   // at once: the monitor never searched it
		110: now.Add(-30 * time.Hour).Add(24 * time.Hour),         // a day after the file came: aired within two weeks
		112: now.Add(-2 * time.Hour).Add(24 * time.Hour),          // a day after the monitor's search
		201: now.Add(-30 * time.Minute).Add(time.Hour),            // an hour after a failed search
		202: now.Add(-8 * 24 * time.Hour).Add(7 * 24 * time.Hour), // a week after the file came: aired long ago
	}
	if got := wanted(sched); !maps.EqualFunc(got, want, time.Time.Equal) {
		ids := slices.Sorted(maps.Keys(got))
		t.Fatalf("wanted %v: %v\nwant %v", ids, got, want)
	}
	noUpgrades := sched
	noUpgrades.Upgrades = false
	got := wanted(noUpgrades)
	if _, ok := got[110]; ok || len(got) != len(want)-3 {
		t.Errorf("without upgrades: %v", slices.Sorted(maps.Keys(got)))
	}

	// A download records the tier of the file it replaces, and the
	// episode is no longer wanted while it runs.
	d, err := st.StartDownload(ctx, Download{EpisodeID: 110, Title: "Show.S01E10.German.1080p.WEB-EXAMPLE", Indexer: "nzb-one", Link: "x", Tier: "German 1080p"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Replaces != "German 720p" {
		t.Errorf("the upgrade replaces %q, want German 720p", d.Replaces)
	}
	if _, ok := wanted(sched)[110]; ok {
		t.Error("an upgrade that runs is still wanted")
	}

	// Searching a show's missing episodes takes every aired one without a
	// file, whatever its mode, cancelled ones included.
	for series, want := range map[int64][]int64{1: {101, 102, 103, 104, 108, 109}, 3: {301}} {
		missing, err := st.Missing(ctx, now, series)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, w := range missing {
			ids = append(ids, w.EpisodeID)
		}
		slices.Sort(ids)
		if !slices.Equal(ids, want) {
			t.Errorf("missing of show %d: %v, want %v", series, ids, want)
		}
	}

	// The episode states carry the next search only while the monitor runs,
	// and a cancelled episode carries its download instead.
	for _, sc := range []*Schedule{&sched, nil} {
		monitoring := sc != nil
		_, states, err := st.ShowStatus(ctx, 1, now, sc)
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
