package store

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// TestLibrary checks each show's summary: episodes counted without specials,
// each file's size once, and the state of the most urgent episode.
func TestLibrary(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	ep := func(id int64, aired string, season, number int) Episode {
		return Episode{ID: id, Aired: aired, Numbers: map[string]Position{"official": {Season: season, Number: number}}}
	}
	save := func(id int64, episodes ...Episode) {
		t.Helper()
		if err := st.SaveSeries(ctx, SeriesData{
			Series:   Series{ID: id, Slug: "s", Name: "Show", OriginalLanguage: "eng", OriginalCountry: "usa", Status: "Continuing", DefaultOrder: "official", TVDBUpdated: "x", Profile: "tv"},
			Episodes: episodes,
		}, false, now); err != nil {
			t.Fatal(err)
		}
	}
	download := func(episode int64, state, importState string) {
		t.Helper()
		d, err := st.StartDownload(ctx, Download{EpisodeID: episode, Title: "Show.S01E01.1080p.WEB-EXAMPLE", Indexer: "nzb-one", Link: "x"}, now)
		if err != nil {
			t.Fatal(err)
		}
		d.State = state
		if err := st.UpdateDownload(ctx, d, now); err != nil {
			t.Fatal(err)
		}
		if importState != "" {
			if err := st.UpdateImport(ctx, d.ID, Import{State: importState}, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	file := func(episode int64, path string, size int64) {
		t.Helper()
		if err := st.SetEpisodeFile(ctx, EpisodeFile{EpisodeID: episode, Path: path, Size: size}, 0, now); err != nil {
			t.Fatal(err)
		}
	}

	// Show 1: two episodes in one file, a special in the library, one
	// downloading, one whose import failed, one never searched, one upcoming.
	save(1, ep(11, "2026-09-01", 1, 1), ep(12, "2026-09-01", 1, 2), ep(10, "2026-08-01", 0, 1),
		ep(13, "2026-09-08", 1, 3), ep(14, "2026-09-15", 1, 4), ep(15, "2026-09-22", 1, 5), ep(16, "2026-10-20", 1, 6))
	file(11, "/lib/Show.S01E01-E02.mkv", 1000)
	file(12, "/lib/Show.S01E01-E02.mkv", 1000)
	file(10, "/lib/Show.S00E01.mkv", 200)
	download(13, Downloading, "")
	download(14, Completed, ImportFailed)
	// Show 2: everything in the library, next episode dated.
	save(2, ep(21, "2026-09-01", 1, 1), ep(22, "2026-11-02", 1, 2))
	file(21, "/lib/Two.S01E01.mkv", 500)
	// Show 3: one episode searched without a result, one waiting for Jellyfin.
	save(3, ep(31, "2026-09-01", 1, 1), ep(32, "2026-09-08", 1, 2))
	retry := now.Add(time.Hour)
	if err := st.SaveSearch(ctx, Search{EpisodeID: 31, Searched: now, Outcome: "nothing", Summary: "No matching German release found", NextRetry: &retry}); err != nil {
		t.Fatal(err)
	}
	download(32, Completed, ImportScanning)

	shows, err := st.Library(ctx, now, false)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]Show{}
	for _, s := range shows {
		byID[s.ID] = s
	}

	one := byID[1]
	if one.Aired != 5 || one.Imported != 2 || one.Bytes != 1200 || one.NextAired != "2026-10-20" {
		t.Errorf("show 1: aired %d, imported %d, bytes %d, next %q; want 5, 2, 1200, 2026-10-20", one.Aired, one.Imported, one.Bytes, one.NextAired)
	}
	if one.Focus == nil || one.Focus.State != EpisodeFailed || one.Focus.EpisodeID != 14 || one.Focus.Download == nil {
		t.Errorf("show 1 focus: %+v, want the failed import of episode 14 with its download", one.Focus)
	}
	if c := one.Counts; c[EpisodeFailed] != 1 || c[EpisodeDownloading] != 1 || c[EpisodeNotSearched] != 1 || len(c) != 3 {
		t.Errorf("show 1 counts: %v", c)
	}

	two := byID[2]
	if two.Aired != 1 || two.Imported != 1 || two.Focus == nil || two.Focus.State != EpisodeUpcoming || two.Focus.Download != nil || len(two.Counts) != 0 {
		t.Errorf("show 2: %+v, focus %+v; want complete with the next episode upcoming", two, two.Focus)
	}

	three := byID[3]
	if three.Focus == nil || three.Focus.State != EpisodeImporting || three.Focus.EpisodeID != 32 {
		t.Errorf("show 3 focus: %+v, want episode 32 importing", three.Focus)
	}
	if c := three.Counts; c[EpisodeWaiting] != 1 || c[EpisodeImporting] != 1 {
		t.Errorf("show 3 counts: %v", c)
	}
}

// TestUpdateSeriesKeepsDeleted checks that a refresh that finishes after
// its show was deleted does not bring the show back.
func TestUpdateSeriesKeepsDeleted(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := SeriesData{Series: Series{ID: 7, Slug: "s", Name: "Show", OriginalLanguage: "eng", OriginalCountry: "usa",
		Status: "Ended", DefaultOrder: "official", TVDBUpdated: "x", Profile: "tv"}}
	now := time.Now()
	if err := st.SaveSeries(ctx, d, false, now); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSeries(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSeries(ctx, d, false, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a deleted show: %v, want ErrNotFound", err)
	}
	if ok, _ := st.HasSeries(ctx, 7); ok {
		t.Fatal("the update brought the deleted show back")
	}
}
