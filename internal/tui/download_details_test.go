package tui

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/store"
)

// TestFailedDownloadDetails follows an existing failed download from the
// database into Activity's short reason and the episode's full report.
func TestFailedDownloadDetails(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	data := api.SeriesData{
		Series:   api.Series{ID: 42, Name: "Harbor Precinct", DefaultOrder: "official", Type: "tv"},
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
	const stopped = "No other release meets German 1080p, and Garner does not switch to another preference on its own."
	if err := st.StopReplacing(ctx, d.ID, stopped); err != nil {
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
		evidence := []string{"27 articles missing"}
		if name == "episode" {
			evidence = append(evidence, "Aborted, cannot be completed", "Downloaded in 12 seconds", "Age: 210d", "news.example.invalid=96 MB",
				"No other release meets German 1080p") // why nothing else started
		}
		for _, want := range evidence {
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
