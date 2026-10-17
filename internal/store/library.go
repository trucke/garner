package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	// Summary comes from the episode's latest search; NextRetry is when
	// the monitor searches it again, unset when it does not get it.
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
	// Summary comes from the latest search; NextRetry is when the monitor
	// searches again, unset when it does not get the episode.
	Summary   string     `json:"summary,omitempty"`
	NextRetry *time.Time `json:"next_retry,omitempty"`
}

type searchOutcome struct {
	searched time.Time
	outcome  string
	summary  string
}

// Library summarises every show in the catalogue as of now. monitoring
// says whether the monitor runs, so missing episodes it gets carry its next
// search.
func (s *Store) Library(ctx context.Context, now time.Time, monitoring bool) ([]Show, error) {
	shows, _, err := s.summarise(ctx, now, 0, monitoring)
	return shows, err
}

// ShowStatus summarises one show and lists what Garner has of each of its
// episodes, as Library does.
func (s *Store) ShowStatus(ctx context.Context, seriesID int64, now time.Time, monitoring bool) (Show, []EpisodeStatus, error) {
	shows, states, err := s.summarise(ctx, now, seriesID, monitoring)
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
func (s *Store) summarise(ctx context.Context, now time.Time, only int64, monitoring bool) ([]Show, []EpisodeStatus, error) {
	f, err := s.loadFacts(ctx, only)
	if err != nil {
		return nil, nil, err
	}
	today := now.Format(time.DateOnly)
	shows := make([]Show, len(f.series))
	index := make(map[int64]int, len(f.series))
	paths := make([]map[string]bool, len(f.series))
	for i, sr := range f.series {
		shows[i] = Show{Series: sr, Counts: map[string]int{}}
		index[sr.ID] = i
		paths[i] = map[string]bool{}
	}
	var states []EpisodeStatus
	for _, e := range f.episodes {
		i, ok := index[e.SeriesID]
		if !ok {
			continue
		}
		sh := &shows[i]
		aired := e.Aired.Valid && e.Aired.String != "" && e.Aired.String <= today
		regular := e.Season.Valid && e.Season.Int64 > 0
		file, hasFile := f.files[e.ID]
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

		d := f.downloads[e.ID]
		search, searched := f.searches[e.ID]
		state := episodeState(hasFile, d, search, searched, aired)
		if state == EpisodeUpcoming && !regular || state == EpisodeNotSearched && !regular {
			// Specials nobody asked for are not missing.
			state = ""
		}
		var next *time.Time
		if monitoring && regular && wants(state, d) && sh.Monitors(e.Aired.String) {
			own, ran := f.monitored[e.ID]
			t := nextSearch(e.Aired.String, own, ran, d)
			if t.IsZero() {
				t = now // due: the monitor gets to it soon
			}
			next = &t
		}
		if only > 0 {
			st := EpisodeStatus{EpisodeID: e.ID, State: state, Attempts: f.attempts[e.ID], NextRetry: next}
			if hasFile {
				st.Size, st.Tier = file.Size, file.Tier
			}
			if explains(state, d) {
				st.Download = d
			}
			if searched {
				st.Summary = search.summary
			}
			states = append(states, st)
		}
		if state == "" || state == EpisodeImported {
			continue
		}
		if state != EpisodeUpcoming {
			sh.Counts[state]++
		}
		focus := &Focus{EpisodeID: e.ID, Season: int(e.Season.Int64), Number: int(e.Number.Int64), State: state, Aired: e.Aired.String, NextRetry: next}
		if explains(state, d) {
			focus.Download = d
		}
		if searched {
			focus.Summary = search.summary
		}
		if moreUrgent(focus, sh.Focus) {
			sh.Focus = focus
		}
	}
	return shows, states, nil
}

// explains reports whether an episode's latest download explains its
// state; a cancelled one explains why the monitor leaves a missing episode
// alone.
func explains(state string, d *Download) bool {
	switch state {
	case EpisodeDownloading, EpisodeProcessing, EpisodeImporting, EpisodeFailed, EpisodeAttention:
		return true
	case EpisodeWaiting, EpisodeNotSearched:
		return leftToYou(d)
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

// SeriesFileTiers maps the show's episodes that have a file to the tier of
// the release it came from, such as "German 1080p"; empty when unknown.
func (s *Store) SeriesFileTiers(ctx context.Context, seriesID int64) (map[int64]string, error) {
	rows, err := s.q.ListSeriesFileTiers(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(rows))
	for _, r := range rows {
		out[r.EpisodeID] = r.Tier
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
	if err := q.ClearSeriesSeasonSearches(ctx, seriesID); err != nil {
		return err
	}
	// The monitor searches the show anew, by the new rules.
	if err := q.ClearSeriesMonitorSearches(ctx, seriesID); err != nil {
		return err
	}
	return tx.Commit()
}
