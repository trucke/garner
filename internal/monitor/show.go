package monitor

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/store"
)

// ErrSearching means the show's missing episodes are being searched already.
var ErrSearching = errors.New("Garner is already searching this show's missing episodes")

// showQueue holds the shows asked for, in order, and their progress.
type showQueue struct {
	mu       sync.Mutex
	order    []int64
	progress map[int64]api.ShowProgress // queued or running
}

func (q *showQueue) add(id int64) (api.ShowProgress, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.progress[id]; ok {
		return api.ShowProgress{}, false
	}
	p := api.ShowProgress{SeriesID: id, Waiting: true}
	q.order, q.progress[id] = append(q.order, id), p
	return p, true
}

// next takes the next show asked for.
func (q *showQueue) next() (int64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.order) == 0 {
		return 0, false
	}
	id := q.order[0]
	q.order = q.order[1:]
	return id, true
}

// waiting reports whether a show waits to be searched.
func (q *showQueue) waiting() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.order) > 0
}

func (q *showQueue) set(p api.ShowProgress) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if p.Finished {
		delete(q.progress, p.SeriesID)
	} else {
		q.progress[p.SeriesID] = p
	}
}

func (q *showQueue) get(id int64) (api.ShowProgress, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	p, ok := q.progress[id]
	return p, ok
}

// SearchShow asks for every missing episode of a show to be searched, and
// the best release of each that meets the show's rules to be downloaded,
// whatever its monitor mode and including episodes whose download was
// cancelled. It runs after the searches asked for before; ErrSearching
// means the show is asked for already.
func (m *Service) SearchShow(seriesID int64) error {
	p, ok := m.shows.add(seriesID)
	if !ok {
		return ErrSearching
	}
	m.report(p)
	m.Wake()
	return nil
}

// ShowSearches counts the show searches asked for or running.
func (m *Service) ShowSearches() int {
	m.shows.mu.Lock()
	defer m.shows.mu.Unlock()
	return len(m.shows.progress)
}

// ShowSearch returns the progress of a show's search, while it is asked
// for or runs.
func (m *Service) ShowSearch(seriesID int64) (api.ShowProgress, bool) { return m.shows.get(seriesID) }

// searchShow searches every missing episode of a show, oldest first, as
// the monitor does: each search counts as the monitor's.
func (m *Service) searchShow(ctx context.Context, seriesID int64) {
	start := m.Now()
	p := api.ShowProgress{SeriesID: seriesID}
	defer func() {
		p.Waiting, p.Finished = false, true
		m.shows.set(p)
		m.report(p)
	}()
	missing, err := m.store.Missing(ctx, start, seriesID)
	if err != nil {
		m.log.Error("search missing episodes", "series", seriesID, "err", err)
		return
	}
	slices.SortFunc(missing, func(a, b store.Wanted) int {
		return cmp.Or(cmp.Compare(a.Season, b.Season), cmp.Compare(a.Number, b.Number))
	})
	p.Total = len(missing)
	m.shows.set(p)
	m.report(p)
	var held map[int64]bool
	for _, w := range missing {
		if ctx.Err() != nil {
			return
		}
		if exists, err := m.store.HasSeries(ctx, seriesID); err != nil || !exists {
			m.log.Info("stopped searching missing episodes: the show is gone", "series", seriesID)
			return
		}
		if held == nil {
			if held, err = m.held(ctx, seriesID); err != nil {
				m.log.Error("search missing episodes: list running downloads", "series", seriesID, "err", err)
				return
			}
			if held == nil {
				held = map[int64]bool{}
			}
		}
		if !held[w.EpisodeID] && m.get(ctx, w) {
			p.Downloads++
			held = nil // the new download may hold more episodes
		}
		p.Done++
		m.shows.set(p)
		m.report(p)
	}
	m.log.Info("searched the missing episodes of a show", "series", seriesID, "episodes", p.Total,
		"downloads", p.Downloads, "duration", m.Now().Sub(start).Round(time.Second))
}

func (m *Service) report(p api.ShowProgress) {
	if m.OnShowProgress != nil {
		m.OnShowProgress(p)
	}
}
