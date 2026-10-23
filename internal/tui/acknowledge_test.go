package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/client"
	"github.com/trucke/garner/internal/connect"
)

// TestClearFailure checks the same action on both screens, including the
// same-state event that must refresh another client's warning count.
func TestClearFailure(t *testing.T) {
	for _, name := range []string{"activity", "episode"} {
		t.Run(name, func(t *testing.T) {
			d := api.Download{ID: 1, EpisodeID: 101, Title: "Harbor.Precinct.S01E01.German.1080p.WEB-GRP",
				State: api.Failed, JobID: "job-1", Failure: "Aborted, cannot be completed", FailureSummary: "27 articles missing",
				Updated: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
				Stages:  []api.Stage{{Name: "Download", Actions: []string{"27 articles were missing"}}}}
			acknowledged := d
			acknowledged.Acknowledged = d.Updated.Add(time.Hour)
			posts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/downloads/1/acknowledge" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				posts++
				json.NewEncoder(w).Encode(acknowledged)
			}))
			defer srv.Close()
			cl, err := client.New(connect.Code{URL: srv.URL, Token: "test"})
			if err != nil {
				t.Fatal(err)
			}
			sess := &session{client: cl, shows: []api.Show{{Counts: map[string]int{api.EpisodeFailed: 1},
				Focus: &api.Focus{EpisodeID: 101, State: api.EpisodeFailed, Download: &d}}}}
			library := newLibrary(sess)
			var current screen
			if name == "activity" {
				s := newActivity(sess, library)
				s.update(activityMsg{from: s, items: []api.ActivityItem{
					{Download: d, SeriesName: "Harbor Precinct", Season: 1, Number: 1},
					{Download: api.Download{ID: 2, EpisodeID: 102, State: api.Downloading}, SeriesName: "Harbor Precinct", Season: 1, Number: 2},
				}})
				current = s
			} else {
				show := newShow(sess, library, 42, "Harbor Precinct", "")
				s := newEpisode(sess, show, 101)
				s.detail = &api.EpisodeDetail{Downloads: []api.Download{d}}
				current = s
			}
			if view := ansi.Strip(current.view(120, 36)); !strings.Contains(view, "c clear failure") {
				t.Fatal("the failed download has no clear action")
			}
			_, cmd := current.update(tea.KeyPressMsg{Code: 'c', Text: "c"})
			if cmd == nil || !current.(capturer).captures() {
				t.Fatal("c did not send the clear request")
			}
			current.update(cmd())
			if posts != 1 {
				t.Errorf("c sent %d requests, want one acknowledgment without a confirmation", posts)
			}
			if library.patch(acknowledged) {
				t.Fatal("a same-state acknowledgment was mistaken for progress; other clients would keep the red badge")
			}
			sess.shows = []api.Show{{Counts: map[string]int{api.EpisodeWaiting: 1}}}
			view := ansi.Strip(current.view(120, 36))
			if !strings.Contains(view, "cleared") || strings.Contains(view, "c clear failure") || current.(capturer).captures() {
				t.Error("clearing was not reflected or the action remained available")
			}
			if name == "episode" && (!strings.Contains(view, "Aborted, cannot be completed") || !strings.Contains(view, "27 articles were missing")) {
				t.Error("clearing removed the episode's full failure report")
			}
			if s, ok := current.(*activityScreen); ok {
				rows := s.rows()
				if rows[0].Download.ID != 2 || activityState(rows[1].Download) != activityCleared {
					t.Error("the cleared failure still sorts ahead of running work")
				}
			}
		})
	}
}
