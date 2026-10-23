package tui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/store"
)

// TestFailedDownloadDetails follows an existing failed download from the
// database into both screens: its evidence must not stop at the API.
func TestFailedDownloadDetails(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	data := api.SeriesData{
		Series:   api.Series{ID: 42, Name: "Harbor Precinct", DefaultOrder: "official", Profile: "tv"},
		Episodes: []api.Episode{{ID: 101, Numbers: map[string]api.Position{"official": {Season: 1, Number: 1}}}},
	}
	if err := st.SaveSeries(ctx, data, true, now); err != nil {
		t.Fatal(err)
	}
	d, err := st.StartDownload(ctx, api.Download{EpisodeID: 101, Title: "Harbor.Precinct.S01.German.1080p.WEB-GRP"}, now)
	if err != nil {
		t.Fatal(err)
	}
	d.State, d.JobID, d.Retryable = api.Failed, "job-1", true
	d.Failure = "Aborted, cannot be completed - https://sabnzbd.org/not-complete"
	d.Stages = []api.Stage{
		{Name: "Source", Actions: []string{"https://indexer.invalid/download?file=1&amp;apikey=SECRET"}},
		{Name: "Download", Actions: []string{"Downloaded in 12 seconds at an average of 8 MB/s<br/>Age: 210d<br/>27 articles were missing"}},
		{Name: "Servers", Actions: []string{"news.example.invalid=96 MB"}},
	}
	if err := st.UpdateDownload(ctx, d, now); err != nil {
		t.Fatal(err)
	}
	items, err := st.Activity(ctx, now.Add(-time.Hour))
	if err != nil || len(items) != 1 {
		t.Fatalf("activity: %d items, %v", len(items), err)
	}
	sess := &session{}
	activity := newActivity(sess, nil)
	activity.update(activityMsg{from: activity, items: items})
	show := newShow(sess, nil, 42, "Harbor Precinct", "")
	show.setDetail(api.SeriesDetail{SeriesData: data})
	episode := newEpisode(sess, show, 101)
	downloads, err := st.EpisodeDownloads(ctx, 101)
	if err != nil {
		t.Fatal(err)
	}
	episode.detail = &api.EpisodeDetail{Downloads: downloads}
	for name, view := range map[string]string{
		"activity": activity.view(120, 36),
		"episode":  episode.view(120, 36),
	} {
		plain := ansi.Strip(view)
		for _, want := range []string{"27 articles missing", "Aborted, cannot be completed", "Downloaded in 12 seconds", "Age: 210d", "news.example.invalid=96 MB"} {
			if !strings.Contains(plain, want) {
				t.Errorf("%s hides %q", name, want)
			}
		}
		for _, unwanted := range []string{"<br", "&amp;", "SECRET", "100%"} {
			if strings.Contains(plain, unwanted) {
				t.Errorf("%s shows HTML, a credential, or invented completion (%q)", name, unwanted)
			}
		}
	}
}

// TestActivityDetailsScroll keeps long diagnostics reachable without
// moving the selected download or losing the Activity key bar.
func TestActivityDetailsScroll(t *testing.T) {
	for _, dimensions := range [][2]int{{120, 36}, {80, 24}, {60, 18}} {
		t.Run(fmt.Sprint(dimensions), func(t *testing.T) {
			d := api.Download{ID: 1, EpisodeID: 101, State: api.Failed, Failure: "An unfamiliar failure: keep this exact explanation"}
			stage := api.Stage{Name: "Repair"}
			for i := range 40 {
				stage.Actions = append(stage.Actions, fmt.Sprintf("Repair diagnostic %02d", i))
			}
			d.Stages = []api.Stage{stage}
			s := newActivity(&session{}, nil)
			s.update(activityMsg{from: s, items: []api.ActivityItem{
				{Download: d, SeriesName: "Harbor Precinct", Season: 1, Number: 1},
				{Download: api.Download{EpisodeID: 102, State: api.Completed, Import: api.Import{State: api.ImportDone}}},
			}})
			width, height := dimensions[0], dimensions[1]
			if initial := ansi.Strip(s.view(width, height)); !strings.Contains(initial, "An unfamiliar failure:") {
				t.Error("the original unknown failure is hidden")
			}
			s.key("tab")
			s.key("G")
			view := ansi.Strip(s.view(width, height))
			if !strings.Contains(view, "Repair diagnostic 39") {
				t.Error("the last diagnostic cannot be reached")
			}
			if s.selected != 101 || !strings.Contains(view, "esc back") {
				t.Error("scrolling diagnostics changed the download or hid navigation")
			}
			if lines := strings.Split(view, "\n"); len(lines) != height {
				t.Errorf("view has %d rows, want %d", len(lines), height)
			} else {
				for _, line := range lines {
					if ansi.StringWidth(line) > width {
						t.Error("a diagnostic overflows the terminal")
					}
				}
			}
			s.key("tab")
			s.key("j")
			if next := ansi.Strip(s.view(width, height)); strings.Contains(next, "Repair diagnostic") || s.inDetails {
				t.Error("the previous download's details stayed after moving to another row")
			}
			s.key("k")
			if s.detailScroll.offset != 0 {
				t.Error("another download inherited the old diagnostic scroll position")
			}
		})
	}
}
