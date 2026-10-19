package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/download"
	"github.com/trucke/garner/internal/prowlarr"
	"github.com/trucke/garner/internal/sabnzbd"
	"github.com/trucke/garner/internal/search"
	"github.com/trucke/garner/internal/store"
)

// TestDue checks what the monitor searches now, and in which order: new
// episodes ahead of an older backlog, which goes from its oldest, and
// missing episodes ahead of upgrades of the same age.
func TestDue(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
	wanted := []store.Wanted{
		{EpisodeID: 1, SeriesID: 1, Season: 1, Number: 2, Aired: "2015-01-08"},
		{EpisodeID: 2, SeriesID: 1, Season: 1, Number: 1, Aired: "2015-01-01"},
		{EpisodeID: 3, SeriesID: 2, Season: 3, Number: 5, Aired: "2026-10-13"},
		{EpisodeID: 4, SeriesID: 2, Season: 3, Number: 4, Aired: "2026-10-06", Next: now.Add(time.Hour)}, // not yet
		{EpisodeID: 5, SeriesID: 3, Season: 1, Number: 1, Aired: "2026-10-01", Next: now.Add(-time.Hour)},
		{EpisodeID: 6, SeriesID: 3, Season: 1, Number: 2, Aired: "2026-10-08"}, // paused
		{EpisodeID: 7, SeriesID: 4, Season: 1, Number: 1, Aired: "2026-09-30", Upgrade: "German 720p"},
		{EpisodeID: 8, SeriesID: 4, Season: 1, Number: 1, Aired: "2010-01-01", Upgrade: "German 720p"},
	}
	paused := map[int64]time.Time{6: now.Add(time.Minute)}
	var got []int64
	for _, w := range due(wanted, paused, now, 14*24*time.Hour) {
		got = append(got, w.EpisodeID)
	}
	if want := []int64{5, 3, 7, 2, 1, 8}; !slices.Equal(got, want) {
		t.Fatalf("due %v, want %v", got, want)
	}
}

// TestSearchShow searches every missing episode of a show that is not
// monitored, cancelled ones included, and counts each search as the
// monitor's, so the monitor does not repeat it at once.
func TestSearchShow(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	st, err := store.Open(ctx, t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	ep := func(id int64, number int) store.Episode {
		return store.Episode{ID: id, Aired: "2020-01-0" + fmt.Sprint(number), Numbers: map[string]store.Position{"official": {Season: 1, Number: number}}}
	}
	if err := st.SaveSeries(ctx, store.SeriesData{
		Series: store.Series{ID: 42, Slug: "harbor-precinct", Name: "Harbor Precinct", OriginalLanguage: "deu", OriginalCountry: "deu",
			Status: "Ended", DefaultOrder: "official", TVDBUpdated: "x", Profile: "tv", Monitor: store.MonitorOff},
		Episodes: []store.Episode{ep(101, 1), ep(102, 2), ep(103, 3)},
	}, false, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEpisodeFile(ctx, store.EpisodeFile{EpisodeID: 101, Path: "/lib/Harbor.S01E01.mkv", Size: 1}, 0, now); err != nil {
		t.Fatal(err)
	}
	d, err := st.StartDownload(ctx, store.Download{EpisodeID: 103, Title: "Harbor.Precinct.S01E03.German.1080p.WEB-EXAMPLE", Indexer: "One", Link: "x"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CancelDownload(ctx, d.ID, "cancelled", now); err != nil {
		t.Fatal(err)
	}

	// An indexer that finds nothing: no download starts.
	var searches []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/indexer":
			fmt.Fprint(w, `[{"id":1,"name":"One","enable":true,"protocol":"usenet"}]`)
		case "/1/api":
			searches = append(searches, r.URL.Query().Get("ep"))
			fmt.Fprint(w, `<?xml version="1.0"?><rss><channel></channel></rss>`)
		}
	}))
	defer srv.Close()
	m := New(st, search.New(st, prowlarr.New(srv.URL, "key", srv.Client()), nil, log), download.New(st, nil, "tv", log), nil, nil, log)
	var reports []api.ShowProgress
	m.OnShowProgress = func(p api.ShowProgress) { reports = append(reports, p) }

	if err := m.SearchShow(42); err != nil {
		t.Fatal(err)
	}
	if err := m.SearchShow(42); err != ErrSearching {
		t.Errorf("asked twice: %v, want ErrSearching", err)
	}
	id, _ := m.shows.next()
	m.searchShow(ctx, id)

	if !slices.Equal(searches, []string{"2", "3"}) {
		t.Errorf("searched episodes %q, want 2 and 3", searches)
	}
	if last := reports[len(reports)-1]; !last.Finished || last.Total != 2 || last.Done != 2 || last.Downloads != 0 {
		t.Errorf("last report %+v", last)
	}
	if _, ok := m.ShowSearch(42); ok {
		t.Error("the finished search is still listed")
	}
	if err := st.SetMonitor(ctx, 42, store.MonitorAll, now); err != nil {
		t.Fatal(err)
	}
	wanted, err := st.Wanted(ctx, time.Now(), store.Schedule{MissingRecent: time.Hour, MissingOlder: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(wanted) != 1 || wanted[0].EpisodeID != 102 {
		t.Fatalf("wanted %+v, want episode 102; 103 stays left to you", wanted)
	}
	for _, w := range wanted {
		if w.Next.IsZero() {
			t.Errorf("episode %d is due at once; its search should count as the monitor's", w.EpisodeID)
		}
	}
}

// TestUpgrade runs a round over a show with a 720p file and a 1080p one:
// the monitor searches only the 720p episode and downloads a release only
// when one in a better tier turns up, not a better source of the same.
func TestUpgrade(t *testing.T) {
	for _, tc := range []struct {
		offered []string
		want    string // the release downloaded; empty for none
	}{
		{[]string{"Harbor.Precinct.S01E01.German.DL.720p.BluRay-A"}, ""},
		{[]string{"Harbor.Precinct.S01E01.German.DL.720p.BluRay-A", "Harbor.Precinct.S01E01.German.DL.1080p.WEB-DL-B"}, "Harbor.Precinct.S01E01.German.DL.1080p.WEB-DL-B"},
	} {
		ctx := context.Background()
		log := slog.New(slog.DiscardHandler)
		st, err := store.Open(ctx, t.TempDir(), log)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		now := time.Now()
		ep := func(id int64, number int) store.Episode {
			return store.Episode{ID: id, Aired: "2020-01-0" + fmt.Sprint(number), Numbers: map[string]store.Position{"official": {Season: 1, Number: number}}}
		}
		if err := st.SaveSeries(ctx, store.SeriesData{
			Series: store.Series{ID: 42, Slug: "harbor-precinct", Name: "Harbor Precinct", OriginalLanguage: "eng", OriginalCountry: "usa",
				Status: "Ended", DefaultOrder: "official", TVDBUpdated: "x", Profile: "tv", Monitor: store.MonitorAll},
			Episodes: []store.Episode{ep(101, 1), ep(102, 2)},
		}, false, now); err != nil {
			t.Fatal(err)
		}
		for id, tier := range map[int64]string{101: "German 720p", 102: "German 1080p"} {
			d, err := st.StartDownload(ctx, store.Download{EpisodeID: id, Title: "x", Indexer: "One", Link: "x", Tier: tier}, now.Add(-30*24*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			d.State = store.Completed
			if err := st.UpdateDownload(ctx, d, d.Created); err != nil {
				t.Fatal(err)
			}
			if err := st.SetEpisodeFile(ctx, store.EpisodeFile{EpisodeID: id, Path: fmt.Sprint("/lib/", id), Size: 1}, d.ID, d.Created); err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateImport(ctx, d.ID, store.Import{State: store.ImportDone}, d.Created); err != nil {
				t.Fatal(err)
			}
		}

		var searched []string
		indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/indexer":
				fmt.Fprint(w, `[{"id":1,"name":"One","enable":true,"protocol":"usenet"}]`)
			case "/1/api":
				searched = append(searched, r.URL.Query().Get("ep"))
				var items strings.Builder
				for _, title := range tc.offered {
					fmt.Fprintf(&items, `<item><title>%s</title><link>http://prowlarr/%[1]s</link><size>1</size></item>`, title)
				}
				fmt.Fprintf(w, `<rss><channel>%s</channel></rss>`, items.String())
			}
		}))
		defer indexer.Close()
		sab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"status":true,"nzo_ids":["job-1"]}`)
		}))
		defer sab.Close()
		schedule := &store.Schedule{
			Recent: 14 * 24 * time.Hour, MissingRecent: time.Hour, MissingOlder: time.Hour,
			Upgrades: true, UpgradeRecent: time.Hour, UpgradeOlder: time.Hour,
			Best: func(string) string { return "German 1080p" },
		}
		m := New(st, search.New(st, prowlarr.New(indexer.URL, "key", indexer.Client()), nil, log),
			download.New(st, sabnzbd.New(sab.URL, "key", sab.Client()), "tv", log), schedule, nil, log)
		m.round(ctx)

		if !slices.Equal(searched, []string{"1"}) {
			t.Errorf("searched episodes %q, want only 1", searched)
		}
		running, err := st.ActiveDownloads(ctx)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case tc.want == "" && len(running) > 0:
			t.Errorf("downloaded %q, no better tier was offered", running[0].Title)
		case tc.want != "" && (len(running) != 1 || running[0].Title != tc.want || running[0].Replaces != "German 720p"):
			t.Errorf("downloads %+v, want %s replacing German 720p", running, tc.want)
		}
	}
}
