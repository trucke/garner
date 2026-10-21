package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/trucke/garner/internal/store/query"
)

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

// Schedule is when the monitor searches an episode again, and whether it
// looks for upgrades. Recent episodes get new and better releases often;
// older ones rarely.
type Schedule struct {
	// Recent is how long after airing an episode counts as recent.
	Recent time.Duration
	// MissingRecent and MissingOlder are the waits between searches for a
	// missing episode.
	MissingRecent, MissingOlder time.Duration
	// Upgrades has the monitor look for a release in a better tier for a
	// file below its profile's best tier, waiting UpgradeRecent or
	// UpgradeOlder between looks.
	Upgrades                    bool
	UpgradeRecent, UpgradeOlder time.Duration
	// Best names a profile's best tier, such as "German 1080p".
	Best func(profile string) string
}

// retryFailed is the wait after a search every indexer failed.
const retryFailed = time.Hour

// delay is how long after a search with this outcome to search again for
// an episode that airs on aired: an hour after a failed search, otherwise
// the wait for a recent or an older episode. An episode that has not aired
// yet waits for its air date.
func (sc Schedule) delay(outcome, aired string, upgrade bool, now time.Time) time.Duration {
	if outcome == "failed" {
		return retryFailed
	}
	recent, older := sc.MissingRecent, sc.MissingOlder
	if upgrade {
		recent, older = sc.UpgradeRecent, sc.UpgradeOlder
	}
	d, err := time.ParseInLocation(time.DateOnly, aired, now.Location())
	switch {
	case err != nil:
		return recent
	case d.After(now):
		return d.Sub(now)
	case now.Sub(d) < sc.Recent:
		return recent
	}
	return older
}

// SaveMonitorSearch records the monitor's own search of an episode, which
// its schedule counts from.
func (s *Store) SaveMonitorSearch(ctx context.Context, episodeID int64, searched time.Time, outcome string) error {
	return s.q.SaveMonitorSearch(ctx, query.SaveMonitorSearchParams{EpisodeID: episodeID, SearchedAt: searched.Unix(), Outcome: outcome})
}

// next is when the monitor searches an episode again. A missing episode
// is due at once (zero) when the monitor never searched it; otherwise it
// waits from the monitor's own latest search or, when a download ended
// since without bringing a file, from that end, so a failed release is not
// replaced at once. An upgrade waits from the same search or from the end
// of the latest download, usually the one that brought the file, so a new
// file is not searched again at once. Searches someone ran to look at
// releases do not count.
func (sc Schedule) next(upgrade bool, aired string, own searchOutcome, searched bool, d *Download) time.Time {
	if !searched && !upgrade {
		return time.Time{}
	}
	var from time.Time
	outcome := ""
	if searched {
		from, outcome = own.searched, own.outcome
	}
	if d != nil {
		if end := ended(*d); end.After(from) {
			from, outcome = end, ""
		}
	}
	if from.IsZero() {
		return time.Time{}
	}
	return from.Add(sc.delay(outcome, aired, upgrade, from))
}

// upgradable reports whether the monitor looks for a better release of a
// show's episode whose file came from a release in tier: one below the
// profile's best. A file of unknown tier is left alone.
func (sc Schedule) upgradable(sr Series, tier string) bool {
	return sc.Upgrades && tier != "" && sc.Best != nil && tier != sc.Best(sr.Profile)
}

// leftToYou reports whether the episode's latest download waits for the
// user: it was cancelled, or its import failed on a path or permission
// problem that another download would hit as well. The monitor then
// leaves the episode alone until someone downloads it or retries the
// import.
func leftToYou(d *Download) bool {
	return d != nil && (d.State == Cancelled || d.Import.State == ImportFailed)
}

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
// latest download: missing, and not left to the user.
func wants(state string, d *Download) bool { return missingState(state) && !leftToYou(d) }

// Wanted is an episode the monitor searches: a missing one, or one whose
// file it upgrades.
type Wanted struct {
	EpisodeID int64
	SeriesID  int64
	// Season and Number are in the show's default order.
	Season, Number int
	Aired          string
	// Upgrade is the tier of the file a better release would replace, such
	// as "German 720p"; empty for a missing episode.
	Upgrade string
	// Next is when the monitor searches it; zero when it is due at once.
	Next time.Time
}

// Wanted lists the aired episodes of monitored shows that the monitor
// searches, with when it searches each: the missing ones and, when the
// schedule says so, those with a file below the best tier. Those whose
// latest download was cancelled are left to the user.
func (s *Store) Wanted(ctx context.Context, now time.Time, sc Schedule) ([]Wanted, error) {
	return s.wanted(ctx, now, 0, &sc, func(sr Series, aired string, d *Download) bool {
		return sr.Monitors(aired) && !leftToYou(d)
	})
}

// Missing lists a show's aired episodes that are not in the library,
// whatever its monitor mode, cancelled ones included. Their Next is unset.
func (s *Store) Missing(ctx context.Context, now time.Time, seriesID int64) ([]Wanted, error) {
	return s.wanted(ctx, now, seriesID, nil, func(Series, string, *Download) bool { return true })
}

// wanted lists the aired regular episodes that are missing, or upgradable
// by the schedule, and that keep accepts, of every show or of only that
// one. Without a schedule, it lists only missing ones and leaves Next unset.
func (s *Store) wanted(ctx context.Context, now time.Time, only int64, sc *Schedule, keep func(sr Series, aired string, d *Download) bool) ([]Wanted, error) {
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
		file, hasFile := f.files[e.ID]
		d := f.downloads[e.ID]
		search, searched := f.searches[e.ID]
		state := episodeState(hasFile, d, search, searched, true)
		upgrade := sc != nil && state == EpisodeImported && sc.upgradable(sr, file.Tier)
		if !missingState(state) && !upgrade || !keep(sr, aired, d) {
			continue
		}
		w := Wanted{EpisodeID: e.ID, SeriesID: e.SeriesID, Season: int(e.Season.Int64), Number: int(e.Number.Int64), Aired: aired}
		if upgrade {
			w.Upgrade = file.Tier
		}
		if own, ran := f.monitored[e.ID]; sc != nil {
			w.Next = sc.next(upgrade, aired, own, ran, d)
		}
		out = append(out, w)
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
