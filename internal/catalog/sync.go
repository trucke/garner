package catalog

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/trucke/garner/internal/store"
)

const (
	syncKey  = "catalog_synced_at"
	retryKey = "catalog_retry" // comma-separated IDs of shows that failed to refresh
	// syncInterval is how often Garner asks TVDB what changed.
	syncInterval = 24 * time.Hour
	// maxUpdatesAge limits how far back TVDB's change list is used; after a
	// longer gap every show is reloaded.
	maxUpdatesAge = 7 * 24 * time.Hour
	// xemMaxAge reloads a show at least this often so TheXEM changes arrive;
	// TheXEM has no change list.
	xemMaxAge = 7 * 24 * time.Hour
)

// Run keeps the catalogue current until ctx is cancelled: once a day it
// reloads shows TVDB reports as changed and shows whose TheXEM data is old.
func (c *Catalog) Run(ctx context.Context) {
	retry := false
	for {
		wait := syncInterval
		last, err := c.lastSync(ctx)
		switch {
		case err != nil:
			c.log.Error("catalog sync state", "err", err)
		case !retry && time.Now().Before(last.Add(syncInterval)):
			wait = time.Until(last.Add(syncInterval))
		default:
			retry = false
			if err := c.Sync(ctx); err != nil {
				c.log.Error("catalog sync", "err", err)
				retry, wait = true, time.Hour
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (c *Catalog) lastSync(ctx context.Context) (time.Time, error) {
	v, err := c.store.SyncState(ctx, syncKey)
	if err != nil || v == "" {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, v)
}

// Sync reloads changed and stale shows.
func (c *Catalog) Sync(ctx context.Context) error {
	start := time.Now()
	last, err := c.lastSync(ctx)
	if err != nil {
		return err
	}
	all, err := c.store.ListSeries(ctx)
	if err != nil {
		return err
	}
	// Shows that failed last time are retried like changed ones.
	changed, err := c.retryList(ctx)
	if err != nil {
		return err
	}
	if !last.IsZero() && time.Since(last) < maxUpdatesAge {
		ids, err := c.tvdb.updatedSeries(ctx, last)
		if err != nil {
			return err
		}
		for _, id := range ids {
			changed[id] = true
		}
	}
	refreshed := 0
	var failed []string
	for _, s := range all {
		if !c.due(s, last, changed) {
			continue
		}
		if err := c.Refresh(ctx, s.ID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.log.Warn("could not refresh series", "id", s.ID, "err", err)
			failed = append(failed, strconv.FormatInt(s.ID, 10))
			continue
		}
		refreshed++
	}
	c.log.Info("catalog synced", "refreshed", refreshed, "failed", len(failed), "shows", len(all),
		"duration", time.Since(start).Round(time.Second))
	// Record the start, so changes made during the sync are seen next time,
	// and keep failed shows for a retry instead of repeating the whole sync.
	if err := c.store.SetSyncState(ctx, retryKey, strings.Join(failed, ",")); err != nil {
		return err
	}
	if err := c.store.SetSyncState(ctx, syncKey, start.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d shows failed to refresh", len(failed), refreshed+len(failed))
	}
	return nil
}

func (c *Catalog) retryList(ctx context.Context) (map[int64]bool, error) {
	v, err := c.store.SyncState(ctx, retryKey)
	if err != nil {
		return nil, err
	}
	ids := map[int64]bool{}
	for f := range strings.SplitSeq(v, ",") {
		if id, err := strconv.ParseInt(f, 10, 64); err == nil {
			ids[id] = true
		}
	}
	return ids, nil
}

func (c *Catalog) due(s store.Series, last time.Time, changed map[int64]bool) bool {
	return last.IsZero() || time.Since(last) >= maxUpdatesAge || changed[s.ID] ||
		s.XEMFetched == nil || time.Since(*s.XEMFetched) >= xemMaxAge
}

// Issue is a problem with imported data that needs a review.
type Issue struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Issues checks a show's imported scene numbering against TVDB.
func Issues(d store.SeriesData) []Issue {
	out := []Issue{}
	episodes := map[int64]store.Episode{}
	for _, e := range d.Episodes {
		episodes[e.ID] = e
	}
	absolute := 0
	var example store.SceneNumber
	for _, n := range d.Scene {
		if n.EpisodeID == nil {
			out = append(out, Issue{Kind: "missing_episode", Message: fmt.Sprintf(
				"TheXEM maps scene S%02dE%02d to TVDB S%02dE%02d, which TVDB does not list",
				n.Scene.Season, n.Scene.Number, n.TVDB.Season, n.TVDB.Number)})
			continue
		}
		if e := episodes[*n.EpisodeID]; n.TVDBAbsolute != 0 && e.Absolute != 0 && n.TVDBAbsolute != e.Absolute {
			if absolute == 0 {
				example = n
			}
			absolute++
		}
	}
	if absolute > 0 {
		e := episodes[*example.EpisodeID]
		out = append(out, Issue{Kind: "absolute_differs", Message: fmt.Sprintf(
			"TheXEM's absolute numbers differ from TVDB's for %d episodes, first at S%02dE%02d (TheXEM %d, TVDB %d)",
			absolute, example.TVDB.Season, example.TVDB.Number, example.TVDBAbsolute, e.Absolute)})
	}
	for _, t := range d.Titles {
		if t.Source == "xem" && t.SceneSeason != nil && t.Season == nil {
			out = append(out, Issue{Kind: "ambiguous_title", Message: fmt.Sprintf("title %q: %s", t.Title, t.Note)})
		}
	}
	return out
}
