package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/store"
)

// TestAcknowledgeDownload clears only the alert, through the authenticated
// API: history, failed-release exclusions, retry budget and timing stay put.
func TestAcknowledgeDownload(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	st, err := store.Open(ctx, t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var episodes []api.Episode
	for i := range 9 {
		e := api.Episode{ID: int64(101 + i), Aired: "2026-01-01",
			Numbers: map[string]api.Position{"official": {Season: 1, Number: i + 1}}}
		if i == 7 {
			e.Aired = "2027-01-01"
		} else if i == 8 {
			e.Numbers["official"] = api.Position{Season: 0, Number: 1}
		}
		episodes = append(episodes, e)
	}
	if err := st.SaveSeries(ctx, api.SeriesData{
		Series:   api.Series{ID: 42, Name: "Harbor Precinct", DefaultOrder: "official", Type: "tv", Monitor: api.MonitorAll},
		Episodes: episodes,
	}, true, now); err != nil {
		t.Fatal(err)
	}
	searched := now.Add(-2 * time.Hour)
	if err := st.SaveSearch(ctx, api.Search{EpisodeID: 101, Searched: searched, Outcome: "found", Summary: "Found a matching release"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveMonitorSearch(ctx, 101, searched, "found"); err != nil {
		t.Fatal(err)
	}
	makeDownload := func(episode int64, title, state, imported string, at time.Time) api.Download {
		t.Helper()
		d, err := st.StartDownload(ctx, api.Download{EpisodeID: episode, Title: title, Link: "unused", Verdict: "accept"}, at.Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		d.State, d.Failure, d.Retryable = state, "Aborted, cannot be completed", true
		d.Stages = []api.Stage{{Name: "Download", Actions: []string{"27 articles were missing"}}}
		if err := st.UpdateDownload(ctx, d, at); err != nil {
			t.Fatal(err)
		}
		if imported != "" {
			if err := st.UpdateImport(ctx, d.ID, api.Import{State: imported}, at); err != nil {
				t.Fatal(err)
			}
		}
		d, err = st.GetDownload(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := makeDownload(101, "Harbor.Precinct.S01E01.German.1080p.WEB-GRP", api.Failed, "", now.Add(-time.Hour))
	schedule := store.Schedule{Recent: 14 * 24 * time.Hour, MissingRecent: 6 * time.Hour, MissingOlder: 24 * time.Hour}
	wantedBefore, err := st.Wanted(ctx, now, schedule)
	if err != nil || len(wantedBefore) != 7 {
		t.Fatalf("wanted before clearing: %d episodes, %v", len(wantedBefore), err)
	}
	s := &Server{cfg: &config.Config{}, store: st, log: log, events: newBroker(), deps: Deps{Now: func() time.Time { return now }}}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	token, err := st.CreateToken(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	sub := s.events.subscribe(0)
	defer s.events.remove(sub)
	call := func(id int64, authenticated bool) (int, time.Time) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/downloads/%d/acknowledge", srv.URL, id), nil)
		if authenticated {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Acknowledged time.Time `json:"acknowledged"`
		}
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, body.Acknowledged
	}
	if code, _ := call(d.ID, false); code != http.StatusUnauthorized {
		t.Fatalf("clear without a token: HTTP %d, want 401", code)
	}
	code, acknowledged := call(d.ID, true)
	if code != http.StatusOK || !acknowledged.Equal(now) {
		t.Fatalf("clear failed download: HTTP %d, acknowledged %v; want 200 and %v", code, acknowledged, now)
	}
	select {
	case event := <-sub.ch:
		if event.Type != "download.updated" {
			t.Errorf("clearing published %q, want download.updated for other clients", event.Type)
		}
		if changed, ok := event.Data.(api.Download); !ok || !changed.Acknowledged.Equal(acknowledged) {
			t.Error("the update did not carry the cleared timestamp to other clients")
		}
	default:
		t.Error("other clients were not notified that the failure was cleared")
	}
	now = now.Add(time.Minute)
	if code, again := call(d.ID, true); code != http.StatusOK || !again.Equal(acknowledged) {
		t.Error("clearing twice must preserve the first acknowledgment")
	}
	stored, err := st.GetDownload(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != api.Failed || stored.Failure != d.Failure || !stored.Updated.Equal(d.Updated) || len(stored.Stages) != 1 {
		// The failed state also keeps the posting from being picked again.
		t.Error("clearing changed the failure, its evidence or its original timestamp")
	}
	if failures, err := st.FailedSince(ctx, 101, searched); err != nil || failures != 1 {
		t.Errorf("retry budget after clearing: %d failures, %v; want 1", failures, err)
	}
	wantedAfter, err := st.Wanted(ctx, now, schedule)
	if err != nil || !slices.Equal(wantedBefore, wantedAfter) {
		t.Error("clearing changed the monitor's wanted episodes or next search times")
	}
	shows, err := st.Library(ctx, now, &schedule)
	if err != nil || len(shows) != 1 || shows[0].Counts[api.EpisodeFailed] != 0 || shows[0].Focus == nil || shows[0].Focus.State == api.EpisodeFailed {
		t.Fatal("the cleared failure still contributes to the library or header warning")
	}
	if shows[0].Focus.Download == nil || shows[0].Focus.Summary != "" {
		t.Error("the cleared failure lost its explanation or fell back to a stale search summary")
	}
	items, err := st.Activity(ctx, now.Add(-24*time.Hour))
	if err != nil || len(items) != 1 || items[0].Download.ID != d.ID {
		t.Fatal("clearing removed recent activity rather than keeping the ended download")
	}
	if items, err := st.Activity(ctx, d.Updated.Add(time.Second)); err != nil || len(items) != 0 {
		t.Error("acknowledging extended the download's activity lifetime")
	}

	// A later search is current information, not the stale search that
	// preceded the failure. Clearing must not hide its new outcome.
	if err := st.SaveSearch(ctx, api.Search{EpisodeID: 101, Searched: now.Add(time.Minute), Outcome: "found", Summary: "Found a newer release"}); err != nil {
		t.Fatal(err)
	}
	shows, err = st.Library(ctx, now.Add(time.Minute), &schedule)
	if err != nil || shows[0].Focus.Summary != "Found a newer release" || shows[0].Focus.Download != nil {
		t.Error("the old cleared failure obscures the newer search")
	}

	// A new failure must raise a new warning; the old screen cannot clear it.
	now = now.Add(2 * time.Minute)
	newer := makeDownload(101, "Harbor.Precinct.S01E01.German.1080p.WEB-OTHER", api.Failed, "", now)
	shows, err = st.Library(ctx, now, &schedule)
	if err != nil || shows[0].Counts[api.EpisodeFailed] != 1 {
		t.Fatal("a new attempt inherited the old attempt's cleared warning")
	}
	if code, _ := call(d.ID, true); code != http.StatusConflict {
		t.Errorf("clear superseded attempt: HTTP %d, want 409", code)
	}
	if newer.ID == d.ID {
		t.Fatal("the replacement did not get its own attempt")
	}
	for i, tc := range []struct{ state, imported string }{
		{api.Downloading, ""}, {api.Completed, api.ImportDone},
		{api.Completed, api.ImportFailed}, {api.Completed, api.ImportUnverified},
		{api.Failed, api.ImportFailed},
	} {
		guarded := makeDownload(int64(102+i), "Harbor.Precinct.S01.German.1080p.WEB-GUARD", tc.state, tc.imported, now)
		if code, _ := call(guarded.ID, true); code != http.StatusConflict {
			t.Errorf("clear %s/import %s: HTTP %d, want 409", tc.state, tc.imported, code)
		}
	}
	rejected := makeDownload(107, "Harbor.Precinct.S01E07.German.1080p.WEB-GRP", api.Failed, api.ImportRejected, now)
	if code, _ := call(rejected.ID, true); code != http.StatusOK {
		t.Errorf("clear a rejected release: HTTP %d, want 200", code)
	}
	// Clearing must restore the usual presentation for an unaired episode
	// or an unsearched special, not make either one newly missing.
	for _, tc := range []struct {
		episode int64
		state   string
	}{{108, api.EpisodeUpcoming}, {109, ""}} {
		failed := makeDownload(tc.episode, "Harbor.Precinct.German.1080p.WEB-EXTRA", api.Failed, "", now)
		if code, _ := call(failed.ID, true); code != http.StatusOK {
			t.Fatalf("clear episode %d: HTTP %d, want 200", tc.episode, code)
		}
		_, states, err := st.ShowStatus(ctx, 42, now, &schedule)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(states, func(s api.EpisodeStatus) bool { return s.EpisodeID == tc.episode })
		if i < 0 || states[i].State != tc.state {
			t.Errorf("cleared episode %d did not return to its usual %q state", tc.episode, tc.state)
		}
	}
	if code, _ := call(99999, true); code != http.StatusNotFound {
		t.Errorf("clear missing download: HTTP %d, want 404", code)
	}
}
