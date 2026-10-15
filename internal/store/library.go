package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/trucke/garner/internal/store/query"
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

// EpisodeStatus is what Garner has of one episode.
type EpisodeStatus struct {
	EpisodeID int64 `json:"episode_id"`
	// State is one of the episode states, or empty for a special nobody
	// asked for.
	State string `json:"state"`
	// Size and Tier describe the imported file: its size and the tier of
	// the release it came from, such as "German 1080p".
	Size int64  `json:"size,omitempty"`
	Tier string `json:"tier,omitempty"`
	// Attempts counts the downloads started for the episode.
	Attempts int `json:"attempts,omitempty"`
	// Download is the latest download when it explains the state.
	Download *Download `json:"download,omitempty"`
	// Summary and NextRetry come from the latest search.
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
	shows, _, err := s.summarise(ctx, now, 0)
	return shows, err
}

// ShowStatus summarises one show and lists what Garner has of each of its
// episodes.
func (s *Store) ShowStatus(ctx context.Context, seriesID int64, now time.Time) (Show, []EpisodeStatus, error) {
	shows, states, err := s.summarise(ctx, now, seriesID)
	if err != nil {
		return Show{}, nil, err
	}
	if len(shows) == 0 {
		return Show{}, nil, fmt.Errorf("series %d: %w", seriesID, ErrNotFound)
	}
	return shows[0], states, nil
}

// summarise works out every episode's state. With only set, it covers that
// show alone and also returns the episodes' states.
func (s *Store) summarise(ctx context.Context, now time.Time, only int64) ([]Show, []EpisodeStatus, error) {
	series, err := s.ListSeries(ctx)
	if err != nil {
		return nil, nil, err
	}
	if only > 0 {
		series = slices.DeleteFunc(series, func(sr Series) bool { return sr.ID != only })
	}
	episodes, err := s.q.ListLibraryEpisodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	fileRows, err := s.q.ListEpisodeFiles(ctx)
	if err != nil {
		return nil, nil, err
	}
	files := make(map[int64]query.ListEpisodeFilesRow, len(fileRows))
	for _, f := range fileRows {
		files[f.EpisodeID] = f
	}
	downloadRows, err := s.q.ListLatestDownloads(ctx)
	if err != nil {
		return nil, nil, err
	}
	latest, err := downloadsFromRows(downloadRows)
	if err != nil {
		return nil, nil, err
	}
	downloads := make(map[int64]*Download, len(latest))
	for i := range latest {
		downloads[latest[i].EpisodeID] = &latest[i]
	}
	countRows, err := s.q.CountDownloadsByEpisode(ctx)
	if err != nil {
		return nil, nil, err
	}
	attempts := make(map[int64]int, len(countRows))
	for _, r := range countRows {
		attempts[r.EpisodeID] = int(r.Attempts)
	}
	searchRows, err := s.q.ListSearchOutcomes(ctx)
	if err != nil {
		return nil, nil, err
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
	var states []EpisodeStatus
	for _, e := range episodes {
		i, ok := index[e.SeriesID]
		if !ok {
			continue
		}
		sh := &shows[i]
		aired := e.Aired.Valid && e.Aired.String != "" && e.Aired.String <= today
		regular := e.Season.Valid && e.Season.Int64 > 0
		file, hasFile := files[e.ID]
		if hasFile && !paths[i][file.Path] {
			paths[i][file.Path] = true
			sh.Bytes += file.Size
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
			state = ""
		}
		if only > 0 {
			st := EpisodeStatus{EpisodeID: e.ID, State: state, Attempts: attempts[e.ID]}
			if hasFile {
				st.Size, st.Tier = file.Size, file.Tier
			}
			if explains(state) {
				st.Download = d
			}
			if searched {
				st.Summary, st.NextRetry = search.summary, search.nextRetry
			}
			states = append(states, st)
		}
		if state == "" || state == EpisodeImported {
			continue
		}
		if state != EpisodeUpcoming {
			sh.Counts[state]++
		}
		f := &Focus{EpisodeID: e.ID, Season: int(e.Season.Int64), Number: int(e.Number.Int64), State: state, Aired: e.Aired.String}
		if explains(state) {
			f.Download = d
		}
		if searched {
			f.Summary, f.NextRetry = search.summary, search.nextRetry
		}
		if moreUrgent(f, sh.Focus) {
			sh.Focus = f
		}
	}
	return shows, states, nil
}

// explains reports whether an episode's latest download explains its state.
func explains(state string) bool {
	switch state {
	case EpisodeDownloading, EpisodeProcessing, EpisodeImporting, EpisodeFailed, EpisodeAttention:
		return true
	}
	return false
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

// SeriesFiles lists the files Garner imported for a show: the paths as
// Garner reaches them and their sizes.
func (s *Store) SeriesFiles(ctx context.Context, seriesID int64) ([]EpisodeFile, error) {
	rows, err := s.q.ListSeriesFiles(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	out := make([]EpisodeFile, len(rows))
	for i, r := range rows {
		out[i] = EpisodeFile{Path: r.Path, Size: r.Size}
	}
	return out, nil
}

// BusyDownloads lists a show's downloads that are running or whose files
// are being moved into the library.
func (s *Store) BusyDownloads(ctx context.Context, seriesID int64) ([]Download, error) {
	rows, err := s.q.ListSeriesBusyDownloads(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	return downloadsFromRows(rows)
}

// ErrBusy means a show has a running download or an import that moves
// files, so it cannot be deleted now.
var ErrBusy = errors.New("a download or import of this show is running")

// DeleteSeries removes a show with its episodes, searches, downloads and
// the record of its files, unless a download or import of it is running.
// The check and the removal are one transaction, so nothing starts in
// between. The files themselves are the caller's.
func (s *Store) DeleteSeries(ctx context.Context, seriesID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	busy, err := q.CountSeriesBusyDownloads(ctx, seriesID)
	if err != nil {
		return err
	}
	if busy > 0 {
		return ErrBusy
	}
	n, err := q.DeleteSeries(ctx, seriesID)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("series %d: %w", seriesID, ErrNotFound)
	}
	return tx.Commit()
}

// ChangeProfile sets a show's profile and, when it changed, drops the
// show's search results in the same transaction: they were judged by the
// old rules, and the next search judges by the new ones. Episodes with a
// running download keep theirs: it may need a replacement from them.
func (s *Store) ChangeProfile(ctx context.Context, seriesID int64, profile string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	row, err := q.GetSeries(ctx, seriesID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("series %d: %w", seriesID, ErrNotFound)
	}
	if err != nil {
		return err
	}
	if row.Profile.String == profile {
		return nil
	}
	if _, err := q.SetSeriesProfile(ctx, query.SetSeriesProfileParams{Profile: nullString(profile), ID: seriesID}); err != nil {
		return err
	}
	if err := q.ClearSeriesSearches(ctx, seriesID); err != nil {
		return err
	}
	return tx.Commit()
}
