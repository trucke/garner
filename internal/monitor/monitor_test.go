package monitor

import (
	"slices"
	"testing"
	"time"

	"github.com/trucke/garner/internal/store"
)

// TestDue checks what the monitor searches now, and in which order: new
// episodes ahead of an older backlog, which goes from its oldest.
func TestDue(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
	wanted := []store.Wanted{
		{EpisodeID: 1, SeriesID: 1, Season: 1, Number: 2, Aired: "2015-01-08"},
		{EpisodeID: 2, SeriesID: 1, Season: 1, Number: 1, Aired: "2015-01-01"},
		{EpisodeID: 3, SeriesID: 2, Season: 3, Number: 5, Aired: "2026-10-13"},
		{EpisodeID: 4, SeriesID: 2, Season: 3, Number: 4, Aired: "2026-10-06", Next: now.Add(time.Hour)}, // not yet
		{EpisodeID: 5, SeriesID: 3, Season: 1, Number: 1, Aired: "2026-10-01", Next: now.Add(-time.Hour)},
		{EpisodeID: 6, SeriesID: 3, Season: 1, Number: 2, Aired: "2026-10-08"}, // paused
	}
	paused := map[int64]time.Time{6: now.Add(time.Minute)}
	var got []int64
	for _, w := range due(wanted, paused, now) {
		got = append(got, w.EpisodeID)
	}
	if want := []int64{5, 3, 2, 1}; !slices.Equal(got, want) {
		t.Fatalf("due %v, want %v", got, want)
	}
}
