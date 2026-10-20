// Package monitor gets the episodes that monitored shows miss: it searches
// each one when it is due and downloads the best release it accepts. It
// upgrades their files below the profile's best tier the same way, taking
// only a release in a better tier. It also searches every missing episode
// of a show when asked to. From there the download service follows the
// download and replaces a failed release, and the importer moves the file
// into the library, replacing the old one.
package monitor

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/download"
	"github.com/trucke/garner/internal/match"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/search"
	"github.com/trucke/garner/internal/selection"
	"github.com/trucke/garner/internal/store"
)

const (
	// interval is how often the monitor looks for due episodes. Searches
	// are hours apart; this only bounds how late one starts.
	interval = time.Minute
	// pause holds back an episode whose search could not run at all, such
	// as one TVDB gives no aired-order position, so it is not retried
	// every minute. Indexer failures are a search outcome with their own
	// retry.
	pause = time.Hour
)

// Service runs the monitor and the searches of shows asked for, one
// search at a time.
type Service struct {
	store     *store.Store
	search    *search.Service
	downloads *download.Service
	// schedule runs the monitor's own rounds; without it, only the shows
	// asked for are searched.
	schedule *store.Schedule
	// rules returns a show's reviewed streaming-service order rules.
	rules  func(seriesID int64) map[release.Service]string
	log    *slog.Logger
	now    func() time.Time
	wake   chan struct{}
	paused map[int64]time.Time // by episode: until when
	shows  showQueue
	// OnShowProgress is called when a show's search was asked for, after
	// each of its episodes, and when it ends.
	OnShowProgress func(api.ShowProgress)
}

// New returns a service that runs the monitor's rounds by the schedule,
// unless it is nil. rules may be nil.
func New(st *store.Store, s *search.Service, d *download.Service, schedule *store.Schedule, rules func(int64) map[release.Service]string, log *slog.Logger) *Service {
	if rules == nil {
		rules = func(int64) map[release.Service]string { return nil }
	}
	return &Service{
		store: st, search: s, downloads: d, schedule: schedule, rules: rules, log: log, now: time.Now,
		wake: make(chan struct{}, 1), paused: map[int64]time.Time{}, shows: showQueue{progress: map[int64]api.ShowProgress{}},
	}
}

// Wake makes the monitor look now, such as after a show was added.
func (m *Service) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run searches the shows asked for, and looks for due episodes every
// minute and when woken, until ctx is cancelled. A show asked for goes
// ahead of the rest of a round.
func (m *Service) Run(ctx context.Context) {
	for {
		for id, ok := m.shows.next(); ok && ctx.Err() == nil; id, ok = m.shows.next() {
			m.searchShow(ctx, id)
		}
		if m.schedule != nil {
			m.round(ctx)
		}
		if m.shows.waiting() {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-time.After(interval):
		}
	}
}

// round gets every due episode, one after another, so the indexers see one
// search at a time.
func (m *Service) round(ctx context.Context) {
	now := m.now()
	wanted, err := m.store.Wanted(ctx, now, *m.schedule)
	if err != nil {
		m.log.Error("monitor: list wanted episodes", "err", err)
		return
	}
	maps.DeleteFunc(m.paused, func(_ int64, until time.Time) bool { return !until.After(now) })
	todo := due(wanted, m.paused, now, m.schedule.Recent)
	held := map[int64]map[int64]bool{} // by show; looked up when needed
	missing, upgrades, started := 0, 0, 0
	for _, w := range todo {
		if ctx.Err() != nil || m.shows.waiting() {
			break // the rest stays due for the next round
		}
		h, ok := held[w.SeriesID]
		if !ok {
			if h, err = m.held(ctx, w.SeriesID); err != nil {
				m.log.Error("monitor: list running downloads", "series", w.SeriesID, "err", err)
				continue
			}
			held[w.SeriesID] = h
		}
		if h[w.EpisodeID] {
			continue
		}
		if w.Upgrade != "" {
			upgrades++
		} else {
			missing++
		}
		if m.get(ctx, w) {
			started++
			delete(held, w.SeriesID) // the new download may hold more episodes
		}
	}
	if missing+upgrades > 0 {
		m.log.Info("monitor: searched episodes", "missing", missing, "upgrades", upgrades, "downloads", started, "duration", m.now().Sub(now).Round(time.Second))
	}
}

// due picks the episodes to search now, in order: recent ones first, so a
// new episode does not wait behind a show's backlog; within those and the
// older ones, missing episodes before upgrades, then by air date.
func due(wanted []store.Wanted, paused map[int64]time.Time, now time.Time, recent time.Duration) []store.Wanted {
	out := slices.DeleteFunc(slices.Clone(wanted), func(w store.Wanted) bool {
		return w.Next.After(now) || paused[w.EpisodeID].After(now)
	})
	since := now.Add(-recent).Format(time.DateOnly)
	older := func(w store.Wanted) bool { return w.Aired < since }
	upgrade := func(w store.Wanted) bool { return w.Upgrade != "" }
	slices.SortFunc(out, func(a, b store.Wanted) int {
		return cmp.Or(
			compareBool(older(a), older(b)),
			compareBool(upgrade(a), upgrade(b)),
			cmp.Compare(a.Aired, b.Aired),
			cmp.Compare(a.SeriesID, b.SeriesID),
			cmp.Compare(a.Season, b.Season),
			cmp.Compare(a.Number, b.Number),
		)
	})
	return out
}

// get searches an episode and downloads the best release it accepts; for
// an upgrade, the search accepts only releases in a better tier than the
// file's. It reports whether a download started.
func (m *Service) get(ctx context.Context, w store.Wanted) bool {
	sr, err := m.search.Episode(ctx, w.EpisodeID)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("monitor: search failed", "episode", w.EpisodeID, "upgrade", w.Upgrade, "err", err)
			m.paused[w.EpisodeID] = m.now().Add(pause)
		}
		return false
	}
	if err := m.store.SaveMonitorSearch(ctx, w.EpisodeID, sr.Searched, sr.Outcome); err != nil {
		m.log.Error("monitor: record search", "episode", w.EpisodeID, "err", err)
		m.paused[w.EpisodeID] = m.now().Add(pause)
	}
	if sr.Outcome != "found" {
		return false
	}
	_, err = m.downloads.Accepted(ctx, w.EpisodeID)
	switch {
	case errors.Is(err, download.ErrNoCandidate), errors.Is(err, store.ErrDownloadRunning):
		// Every accepted release failed before, or someone started a
		// download meanwhile; the next search is due later.
		m.log.Info("monitor: nothing to download", "episode", w.EpisodeID, "reason", err)
		return false
	case err != nil:
		m.log.Warn("monitor: download failed to start", "episode", w.EpisodeID, "err", err)
		return false
	}
	if w.Upgrade != "" {
		m.log.Info("monitor: upgrading", "episode", w.EpisodeID, "from", w.Upgrade)
	}
	return true
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// held lists the episodes a show's running downloads and imports hold: a
// pack or a double episode holds more than the one it was started for.
func (m *Service) held(ctx context.Context, seriesID int64) (map[int64]bool, error) {
	busy, err := m.store.BusyDownloads(ctx, seriesID)
	if err != nil || len(busy) == 0 {
		return nil, err
	}
	d, err := m.store.LoadSeries(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	show := match.NewShow(d, match.Options{Anime: d.Series.Profile == selection.AnimeName, ServiceOrders: m.rules(seriesID)})
	held := map[int64]bool{}
	for _, dl := range busy {
		held[dl.EpisodeID] = true
		if r := show.Match(release.Parse(dl.Title), time.Time{}); r.Kind == match.Episodes {
			for _, id := range r.Episodes {
				held[id] = true
			}
		}
	}
	return held, nil
}
