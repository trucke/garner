// Package monitor gets the episodes that monitored shows miss: it searches
// each one when it is due and downloads the best release it accepts. From
// there the download service follows the download and replaces a failed
// release, and the importer moves the file into the library.
package monitor

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"time"

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
	// recent episodes go first, ahead of a backlog of older ones.
	recent = 14 * 24 * time.Hour
)

// Service runs the monitor.
type Service struct {
	store     *store.Store
	search    *search.Service
	downloads *download.Service
	// rules returns a show's reviewed streaming-service order rules.
	rules  func(seriesID int64) map[release.Service]string
	log    *slog.Logger
	now    func() time.Time
	wake   chan struct{}
	paused map[int64]time.Time // by episode: until when
}

// New returns a monitor. rules may be nil.
func New(st *store.Store, s *search.Service, d *download.Service, rules func(int64) map[release.Service]string, log *slog.Logger) *Service {
	if rules == nil {
		rules = func(int64) map[release.Service]string { return nil }
	}
	return &Service{
		store: st, search: s, downloads: d, rules: rules, log: log, now: time.Now,
		wake: make(chan struct{}, 1), paused: map[int64]time.Time{},
	}
}

// Wake makes the monitor look now, such as after a show was added.
func (m *Service) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run looks for due episodes every minute and when woken, until ctx is
// cancelled.
func (m *Service) Run(ctx context.Context) {
	for {
		m.round(ctx)
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
	wanted, err := m.store.Wanted(ctx, now)
	if err != nil {
		m.log.Error("monitor: list missing episodes", "err", err)
		return
	}
	maps.DeleteFunc(m.paused, func(_ int64, until time.Time) bool { return !until.After(now) })
	todo := due(wanted, m.paused, now)
	held := map[int64]map[int64]bool{} // by show; looked up when needed
	searched, started := 0, 0
	for _, w := range todo {
		if ctx.Err() != nil {
			return
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
		searched++
		if m.get(ctx, w) {
			started++
			delete(held, w.SeriesID) // the new download may hold more episodes
		}
	}
	if searched > 0 {
		m.log.Info("monitor: searched missing episodes", "episodes", searched, "downloads", started, "duration", m.now().Sub(now).Round(time.Second))
	}
}

// due picks the episodes to search now, in order: those of the last two
// weeks first, then by air date, so a new episode does not wait behind a
// show's backlog.
func due(wanted []store.Wanted, paused map[int64]time.Time, now time.Time) []store.Wanted {
	out := slices.DeleteFunc(slices.Clone(wanted), func(w store.Wanted) bool {
		return w.Next.After(now) || paused[w.EpisodeID].After(now)
	})
	since := now.Add(-recent).Format(time.DateOnly)
	older := func(w store.Wanted) int {
		if w.Aired >= since {
			return 0
		}
		return 1
	}
	slices.SortFunc(out, func(a, b store.Wanted) int {
		return cmp.Or(
			cmp.Compare(older(a), older(b)),
			cmp.Compare(a.Aired, b.Aired),
			cmp.Compare(a.SeriesID, b.SeriesID),
			cmp.Compare(a.Season, b.Season),
			cmp.Compare(a.Number, b.Number),
		)
	})
	return out
}

// get searches an episode and downloads the best release it accepts. It
// reports whether a download started.
func (m *Service) get(ctx context.Context, w store.Wanted) bool {
	sr, err := m.search.Episode(ctx, w.EpisodeID)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("monitor: search failed", "episode", w.EpisodeID, "err", err)
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
	return true
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
	profile := selection.ProfileNamed(d.Series.Profile)
	show := match.NewShow(d, match.Options{Anime: profile.Name == selection.Anime.Name, ServiceOrders: m.rules(seriesID)})
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
