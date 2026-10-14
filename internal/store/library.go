package store

import (
	"context"
	"time"
)

// Episode states, most urgent first. Shows take the state of their most
// urgent episode.
const (
	EpisodeFailed      = "failed"       // the latest download or import failed and nothing replaced it
	EpisodeAttention   = "attention"    // in the library, but Jellyfin did not confirm it
	EpisodeDownloading = "downloading"  // with SABnzbd, waiting or downloading
	EpisodeProcessing  = "processing"   // SABnzbd repairs or unpacks it
	EpisodeImporting   = "importing"    // file check, move or waiting for Jellyfin
	EpisodeWaiting     = "waiting"      // aired and missing; searched without a usable result
	EpisodeNotSearched = "not_searched" // aired and missing; never searched
	EpisodeUpcoming    = "upcoming"     // not aired yet
	EpisodeImported    = "imported"     // in the library
)

var urgency = map[string]int{
	EpisodeFailed: 0, EpisodeAttention: 1, EpisodeDownloading: 2, EpisodeProcessing: 3, EpisodeImporting: 4,
	EpisodeWaiting: 5, EpisodeNotSearched: 5, EpisodeUpcoming: 6, EpisodeImported: 7,
}

// Show is a show with a summary of what Garner has of it.
type Show struct {
	Series
	// Aired counts the episodes aired so far, specials excluded; Imported
	// counts those in the library.
	Aired    int `json:"aired"`
	Imported int `json:"imported"`
	// Bytes is the size of the imported files, each file counted once.
	Bytes int64 `json:"bytes"`
	// NextAired is the air date of the next episode, specials excluded.
	NextAired string `json:"next_aired,omitempty"`
	// Counts holds how many episodes are in each state other than imported
	// and upcoming, by state name.
	Counts map[string]int `json:"counts"`
	// Focus is the most urgent episode; nil when every aired episode is in
	// the library and none is upcoming.
	Focus *Focus `json:"focus,omitempty"`
}

// Focus is the episode a show's state comes from.
type Focus struct {
	EpisodeID int64  `json:"episode_id"`
	Season    int    `json:"season"`
	Number    int    `json:"number"`
	State     string `json:"state"`
	Aired     string `json:"aired,omitempty"`
	// Download is the episode's latest download, when it has one.
	Download *Download `json:"download,omitempty"`
	// Summary and NextRetry come from the episode's latest search.
	Summary   string     `json:"summary,omitempty"`
	NextRetry *time.Time `json:"next_retry,omitempty"`
}

type searchOutcome struct {
	searched  time.Time
	summary   string
	nextRetry *time.Time
}

// Library summarises every show in the catalogue as of now.
func (s *Store) Library(ctx context.Context, now time.Time) ([]Show, error) {
	series, err := s.ListSeries(ctx)
	if err != nil {
		return nil, err
	}
	episodes, err := s.q.ListLibraryEpisodes(ctx)
	if err != nil {
		return nil, err
	}
	fileRows, err := s.q.ListEpisodeFiles(ctx)
	if err != nil {
		return nil, err
	}
	files := make(map[int64]string, len(fileRows))
	sizes := make(map[string]int64, len(fileRows))
	for _, f := range fileRows {
		files[f.EpisodeID] = f.Path
		sizes[f.Path] = f.Size
	}
	downloadRows, err := s.q.ListLatestDownloads(ctx)
	if err != nil {
		return nil, err
	}
	latest, err := downloadsFromRows(downloadRows)
	if err != nil {
		return nil, err
	}
	downloads := make(map[int64]*Download, len(latest))
	for i := range latest {
		downloads[latest[i].EpisodeID] = &latest[i]
	}
	searchRows, err := s.q.ListSearchOutcomes(ctx)
	if err != nil {
		return nil, err
	}
	searches := make(map[int64]searchOutcome, len(searchRows))
	for _, r := range searchRows {
		o := searchOutcome{searched: time.Unix(r.SearchedAt, 0), summary: r.Summary}
		if r.NextRetryAt.Valid {
			t := time.Unix(r.NextRetryAt.Int64, 0)
			o.nextRetry = &t
		}
		searches[r.EpisodeID] = o
	}

	today := now.Format(time.DateOnly)
	shows := make([]Show, len(series))
	index := make(map[int64]int, len(series))
	paths := make([]map[string]bool, len(series))
	for i, sr := range series {
		shows[i] = Show{Series: sr, Counts: map[string]int{}}
		index[sr.ID] = i
		paths[i] = map[string]bool{}
	}
	for _, e := range episodes {
		i, ok := index[e.SeriesID]
		if !ok {
			continue
		}
		sh := &shows[i]
		aired := e.Aired.Valid && e.Aired.String != "" && e.Aired.String <= today
		regular := e.Season.Valid && e.Season.Int64 > 0
		path, hasFile := files[e.ID]
		if hasFile && !paths[i][path] {
			paths[i][path] = true
			sh.Bytes += sizes[path]
		}
		if regular && aired {
			sh.Aired++
			if hasFile {
				sh.Imported++
			}
		}
		if regular && !aired && e.Aired.Valid && e.Aired.String != "" && (sh.NextAired == "" || e.Aired.String < sh.NextAired) {
			sh.NextAired = e.Aired.String
		}

		d := downloads[e.ID]
		search, searched := searches[e.ID]
		state := episodeState(hasFile, d, search, searched, aired)
		if state == EpisodeUpcoming && !regular || state == EpisodeNotSearched && !regular {
			// Specials nobody asked for are not missing.
			continue
		}
		if state != EpisodeImported && state != EpisodeUpcoming {
			sh.Counts[state]++
		}
		if state == EpisodeImported {
			continue
		}
		f := &Focus{EpisodeID: e.ID, Season: int(e.Season.Int64), Number: int(e.Number.Int64), State: state, Aired: e.Aired.String, Download: d}
		if searched {
			f.Summary, f.NextRetry = search.summary, search.nextRetry
		}
		if moreUrgent(f, sh.Focus) {
			sh.Focus = f
		}
	}
	for i := range shows {
		if f := shows[i].Focus; f != nil && f.State != EpisodeDownloading && f.State != EpisodeProcessing &&
			f.State != EpisodeImporting && f.State != EpisodeFailed && f.State != EpisodeAttention {
			// The download only explains the states above.
			f.Download = nil
		}
	}
	return shows, nil
}

// episodeState decides an episode's state from its file, latest download
// and latest search.
func episodeState(hasFile bool, d *Download, s searchOutcome, searched, aired bool) string {
	if d != nil {
		switch {
		case d.State == Processing:
			return EpisodeProcessing
		case d.Active():
			return EpisodeDownloading
		case d.State == Completed && (d.Import.State == "" || d.Import.State == ImportPending ||
			d.Import.State == ImportMoving || d.Import.State == ImportScanning):
			return EpisodeImporting
		case d.State == Completed && d.Import.State == ImportUnverified:
			return EpisodeAttention
		}
	}
	if hasFile {
		return EpisodeImported
	}
	if d != nil && (d.State == Failed || d.Import.State == ImportFailed || d.Import.State == ImportRejected) &&
		(!searched || !s.searched.After(d.Updated)) {
		return EpisodeFailed
	}
	switch {
	case !aired:
		return EpisodeUpcoming
	case searched:
		return EpisodeWaiting
	}
	return EpisodeNotSearched
}

// moreUrgent orders episodes by state, then by position.
func moreUrgent(a, b *Focus) bool {
	if b == nil {
		return true
	}
	if urgency[a.State] != urgency[b.State] {
		return urgency[a.State] < urgency[b.State]
	}
	if a.State == EpisodeUpcoming && a.Aired != b.Aired {
		// Dated episodes come before undated ones.
		return b.Aired == "" || a.Aired != "" && a.Aired < b.Aired
	}
	if a.Season != b.Season {
		return a.Season < b.Season
	}
	return a.Number < b.Number
}
