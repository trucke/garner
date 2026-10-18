package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/trucke/garner/internal/store/query"
)

// Monitor modes: which of a show's episodes the monitor gets on its own.
const (
	MonitorNew = "new" // those airing from the day the mode was chosen
	MonitorAll = "all" // every aired episode not in the library
	MonitorOff = "off" // none; Garner searches when asked
)

// MonitorModes lists the modes, the default first.
var MonitorModes = []string{MonitorNew, MonitorAll, MonitorOff}

// Monitors reports whether the monitor gets the show's episode that airs on
// aired, a date.
func (sr Series) Monitors(aired string) bool {
	switch sr.Monitor {
	case MonitorAll:
		return true
	case MonitorNew:
		return aired != "" && aired >= sr.MonitorSince.Local().Format(time.DateOnly)
	}
	return false
}

// SetMonitor changes which of a show's episodes the monitor gets. A mode
// chosen anew counts from now.
func (s *Store) SetMonitor(ctx context.Context, seriesID int64, mode string, now time.Time) error {
	n, err := s.q.SetSeriesMonitor(ctx, query.SetSeriesMonitorParams{Monitor: mode, Since: now.Unix(), ID: seriesID})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("series %d: %w", seriesID, ErrNotFound)
	}
	return nil
}

// Retry delays when nothing was downloaded. Recent episodes get new
// releases often; older ones rarely.
const (
	retryRecent = 6 * time.Hour
	retryOld    = 24 * time.Hour
	retryFailed = time.Hour
	recent      = 14 * 24 * time.Hour
)

// RetryDelay is how long after a search with this outcome to search again
// for an episode that airs on aired: an hour after a failed search,
// otherwise 6 hours for an episode of the last two weeks and a day for an
// older one. An episode that has not aired yet waits for its air date.
func RetryDelay(outcome, aired string, now time.Time) time.Duration {
	if outcome == "failed" {
		return retryFailed
	}
	d, err := time.ParseInLocation(time.DateOnly, aired, now.Location())
	switch {
	case err != nil:
		return retryRecent
	case d.After(now):
		return d.Sub(now)
	case now.Sub(d) < recent:
		return retryRecent
	}
	return retryOld
}

// SaveMonitorSearch records the monitor's own search of an episode, which
// its schedule counts from.
func (s *Store) SaveMonitorSearch(ctx context.Context, episodeID int64, searched time.Time, outcome string) error {
	return s.q.SaveMonitorSearch(ctx, query.SaveMonitorSearchParams{EpisodeID: episodeID, SearchedAt: searched.Unix(), Outcome: outcome})
}

// nextSearch is when the monitor searches a missing episode again: at once
// (zero) when it never searched it; otherwise after the delay from its own
// latest search or, when a download ended since without bringing a file,
// the same delay from that end, so a failed release is not replaced at
// once. Searches someone ran to look at releases do not count.
func nextSearch(aired string, own searchOutcome, searched bool, d *Download) time.Time {
	if !searched {
		return time.Time{}
	}
	from, outcome := own.searched, own.outcome
	if d != nil {
		if end := ended(*d); end.After(from) {
			from, outcome = end, ""
		}
	}
	return from.Add(RetryDelay(outcome, aired, from))
}

// leftToYou reports whether the episode's latest download was cancelled:
// the monitor then leaves the episode alone until someone downloads it.
func leftToYou(d *Download) bool { return d != nil && d.State == Cancelled }

// ended is when a download that brought no file ended: its own end, or its
// import's.
func ended(d Download) time.Time {
	if d.Import.Updated.After(d.Updated) {
		return d.Import.Updated
	}
	return d.Updated
}

// missingState reports whether an aired episode in this state is missing:
// never searched, waiting, or failed. A running download or import is not.
func missingState(state string) bool {
	return state == EpisodeNotSearched || state == EpisodeWaiting || state == EpisodeFailed
}

// wants reports whether the monitor gets an episode in this state with this
// latest download: missing, and not cancelled.
func wants(state string, d *Download) bool { return missingState(state) && !leftToYou(d) }

// Wanted is a missing episode.
type Wanted struct {
	EpisodeID int64
	SeriesID  int64
	// Season and Number are in the show's default order.
	Season, Number int
	Aired          string
	// Next is when the monitor searches it; zero when it never did.
	Next time.Time
}

// Wanted lists the aired episodes that monitored shows miss, with when the
// monitor searches each. Those whose download was cancelled are left to
// the user.
func (s *Store) Wanted(ctx context.Context, now time.Time) ([]Wanted, error) {
	return s.missing(ctx, now, 0, func(sr Series, aired string, d *Download) bool {
		return sr.Monitors(aired) && !leftToYou(d)
	})
}

// Missing lists a show's aired episodes that are not in the library,
// whatever its monitor mode, cancelled ones included.
func (s *Store) Missing(ctx context.Context, now time.Time, seriesID int64) ([]Wanted, error) {
	return s.missing(ctx, now, seriesID, func(Series, string, *Download) bool { return true })
}

// missing lists the aired regular episodes that are missing and that keep
// accepts, of every show or of only that one.
func (s *Store) missing(ctx context.Context, now time.Time, only int64, keep func(sr Series, aired string, d *Download) bool) ([]Wanted, error) {
	f, err := s.loadFacts(ctx, only)
	if err != nil {
		return nil, err
	}
	series := make(map[int64]Series, len(f.series))
	for _, sr := range f.series {
		series[sr.ID] = sr
	}
	today := now.Format(time.DateOnly)
	var out []Wanted
	for _, e := range f.episodes {
		sr, ok := series[e.SeriesID]
		aired := e.Aired.String
		if !ok || !e.Season.Valid || e.Season.Int64 < 1 || aired == "" || aired > today {
			continue
		}
		_, hasFile := f.files[e.ID]
		d := f.downloads[e.ID]
		search, searched := f.searches[e.ID]
		if !missingState(episodeState(hasFile, d, search, searched, true)) || !keep(sr, aired, d) {
			continue
		}
		own, ran := f.monitored[e.ID]
		out = append(out, Wanted{
			EpisodeID: e.ID, SeriesID: e.SeriesID, Season: int(e.Season.Int64), Number: int(e.Number.Int64), Aired: aired,
			Next: nextSearch(aired, own, ran, d),
		})
	}
	return out, nil
}

// facts is what episode states are worked out from.
type facts struct {
	series    []Series
	episodes  []query.ListLibraryEpisodesRow
	files     map[int64]query.ListEpisodeFilesRow
	downloads map[int64]*Download // the latest of each episode
	attempts  map[int64]int
	searches  map[int64]searchOutcome // the latest search, whoever ran it
	monitored map[int64]searchOutcome // the monitor's own latest search
}

// loadFacts loads the facts of every show, or of only that one.
func (s *Store) loadFacts(ctx context.Context, only int64) (facts, error) {
	var f facts
	var err error
	if f.series, err = s.ListSeries(ctx); err != nil {
		return f, err
	}
	if only > 0 {
		f.series = slices.DeleteFunc(f.series, func(sr Series) bool { return sr.ID != only })
	}
	if f.episodes, err = s.q.ListLibraryEpisodes(ctx); err != nil {
		return f, err
	}
	fileRows, err := s.q.ListEpisodeFiles(ctx)
	if err != nil {
		return f, err
	}
	f.files = make(map[int64]query.ListEpisodeFilesRow, len(fileRows))
	for _, r := range fileRows {
		f.files[r.EpisodeID] = r
	}
	downloadRows, err := s.q.ListLatestDownloads(ctx)
	if err != nil {
		return f, err
	}
	latest, err := downloadsFromRows(downloadRows)
	if err != nil {
		return f, err
	}
	f.downloads = make(map[int64]*Download, len(latest))
	for i := range latest {
		f.downloads[latest[i].EpisodeID] = &latest[i]
	}
	countRows, err := s.q.CountDownloadsByEpisode(ctx)
	if err != nil {
		return f, err
	}
	f.attempts = make(map[int64]int, len(countRows))
	for _, r := range countRows {
		f.attempts[r.EpisodeID] = int(r.Attempts)
	}
	searchRows, err := s.q.ListSearchOutcomes(ctx)
	if err != nil {
		return f, err
	}
	f.searches = make(map[int64]searchOutcome, len(searchRows))
	for _, r := range searchRows {
		f.searches[r.EpisodeID] = searchOutcome{searched: time.Unix(r.SearchedAt, 0), outcome: r.Outcome, summary: r.Summary}
	}
	monitorRows, err := s.q.ListMonitorSearches(ctx)
	if err != nil {
		return f, err
	}
	f.monitored = make(map[int64]searchOutcome, len(monitorRows))
	for _, r := range monitorRows {
		f.monitored[r.EpisodeID] = searchOutcome{searched: time.Unix(r.SearchedAt, 0), outcome: r.Outcome}
	}
	return f, nil
}
