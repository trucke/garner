package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/trucke/garner/internal/store/query"
)

// SaveSeries stores a show, replacing its orders, titles, numbering and
// episodes. When scene is false, existing scene numbering is kept.
func (s *Store) SaveSeries(ctx context.Context, d SeriesData, scene bool, now time.Time) error {
	return s.saveSeries(ctx, d, scene, now, true)
}

// UpdateSeries is SaveSeries for a show that must still be in the
// catalogue: it returns ErrNotFound instead of adding a show deleted while
// its update was loading.
func (s *Store) UpdateSeries(ctx context.Context, d SeriesData, scene bool, now time.Time) error {
	return s.saveSeries(ctx, d, scene, now, false)
}

func (s *Store) saveSeries(ctx context.Context, d SeriesData, scene bool, now time.Time, create bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	sr := d.Series
	if !create {
		n, err := q.SeriesExists(ctx, sr.ID)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("series %d: %w", sr.ID, ErrNotFound)
		}
	}
	if err := q.UpsertSeries(ctx, query.UpsertSeriesParams{
		ID: sr.ID, Slug: sr.Slug, Name: sr.Name, NameEng: nullString(sr.NameEng), NameDeu: nullString(sr.NameDeu),
		OriginalLanguage: sr.OriginalLanguage, OriginalCountry: sr.OriginalCountry, Status: sr.Status,
		Year: nullInt(sr.Year), FirstAired: nullString(sr.FirstAired), AverageRuntime: nullInt(sr.AverageRuntime),
		DefaultOrder: sr.DefaultOrder, TvdbUpdated: sr.TVDBUpdated, AddedAt: now.Unix(), FetchedAt: now.Unix(),
		Profile: nullString(sr.Profile),
		// Never NULL once loaded, even without a network: NULL marks shows
		// loaded before the details were kept.
		Network: sql.NullString{String: sr.Network, Valid: true}, LastAired: nullString(sr.LastAired),
		Overview: nullString(sr.Overview), OverviewLanguage: nullString(sr.OverviewLanguage),
		Monitor: cmp.Or(sr.Monitor, MonitorNew), MonitorSince: now.Unix(),
	}); err != nil {
		return fmt.Errorf("save series: %w", err)
	}

	if err := q.DeleteSeriesOrders(ctx, sr.ID); err != nil {
		return err
	}
	for _, o := range d.Orders {
		if err := q.InsertSeriesOrder(ctx, query.InsertSeriesOrderParams{SeriesID: sr.ID, Type: o.Type, Name: o.Name, AlternateName: nullString(o.AlternateName)}); err != nil {
			return fmt.Errorf("save order %s: %w", o.Type, err)
		}
	}

	if err := q.DeleteSeriesTitles(ctx, sr.ID); err != nil {
		return err
	}
	for _, t := range d.Titles {
		if err := q.InsertSeriesTitle(ctx, query.InsertSeriesTitleParams{
			SeriesID: sr.ID, Title: t.Title, Language: nullString(t.Language), Season: nullIntPtr(t.Season),
			Source: t.Source, SceneSeason: nullIntPtr(t.SceneSeason), Note: nullString(t.Note),
		}); err != nil {
			return fmt.Errorf("save title %q: %w", t.Title, err)
		}
	}

	// Numbering references episodes, so clear it before episodes change.
	if err := q.DeleteEpisodeNumbers(ctx, sr.ID); err != nil {
		return err
	}
	keep := make(map[int64]bool, len(d.Episodes))
	for _, e := range d.Episodes {
		keep[e.ID] = true
		if err := q.UpsertEpisode(ctx, query.UpsertEpisodeParams{
			ID: e.ID, SeriesID: sr.ID, Aired: nullString(e.Aired), Runtime: nullInt(e.Runtime),
			AbsoluteNumber: nullInt(e.Absolute), IsMovie: boolInt(e.IsMovie),
			Name: nullString(e.Name), NameEng: nullString(e.NameEng), NameDeu: nullString(e.NameDeu),
			Overview: e.Overview, OverviewLanguage: e.OverviewLanguage,
		}); err != nil {
			return fmt.Errorf("save episode %d: %w", e.ID, err)
		}
		for orderType, p := range e.Numbers {
			if err := q.InsertEpisodeNumber(ctx, query.InsertEpisodeNumberParams{
				EpisodeID: e.ID, SeriesID: sr.ID, OrderType: orderType, Season: int64(p.Season), Number: int64(p.Number),
			}); err != nil {
				return fmt.Errorf("save episode %d in %s order: %w", e.ID, orderType, err)
			}
		}
	}
	existing, err := q.ListEpisodeIDs(ctx, sr.ID)
	if err != nil {
		return err
	}
	for _, id := range existing {
		if !keep[id] {
			if err := q.DeleteEpisode(ctx, id); err != nil {
				return err
			}
		}
	}

	if scene {
		if err := q.DeleteSceneNumbers(ctx, sr.ID); err != nil {
			return err
		}
		for _, n := range d.Scene {
			var episode sql.NullInt64
			if n.EpisodeID != nil {
				episode = sql.NullInt64{Int64: *n.EpisodeID, Valid: true}
			}
			if err := q.InsertSceneNumber(ctx, query.InsertSceneNumberParams{
				SeriesID: sr.ID, TvdbSeason: int64(n.TVDB.Season), TvdbEpisode: int64(n.TVDB.Number), TvdbAbsolute: nullInt(n.TVDBAbsolute),
				SceneSeason: int64(n.Scene.Season), SceneEpisode: int64(n.Scene.Number), SceneAbsolute: nullInt(n.SceneAbsolute), EpisodeID: episode,
			}); err != nil {
				return fmt.Errorf("save scene numbering: %w", err)
			}
		}
		if err := q.SetXEMFetched(ctx, query.SetXEMFetchedParams{XemFetchedAt: sql.NullInt64{Int64: now.Unix(), Valid: true}, ID: sr.ID}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HasSeries reports whether the show is in the catalogue.
func (s *Store) HasSeries(ctx context.Context, id int64) (bool, error) {
	n, err := s.q.SeriesExists(ctx, id)
	return n > 0, err
}

// ListSeries lists the catalogue.
func (s *Store) ListSeries(ctx context.Context) ([]Series, error) {
	rows, err := s.q.ListSeries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Series, len(rows))
	for i, r := range rows {
		out[i] = seriesFromRow(r)
	}
	return out, nil
}

// SeriesIDs lists the IDs of all shows in the catalogue.
func (s *Store) SeriesIDs(ctx context.Context) ([]int64, error) {
	return s.q.ListSeriesIDs(ctx)
}

// LoadSeries returns a show with everything Garner knows about it.
func (s *Store) LoadSeries(ctx context.Context, id int64) (SeriesData, error) {
	row, err := s.q.GetSeries(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return SeriesData{}, fmt.Errorf("series %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return SeriesData{}, err
	}
	d := SeriesData{Series: seriesFromRow(row), Orders: []Order{}, Titles: []Title{}, Episodes: []Episode{}, Scene: []SceneNumber{}}

	orders, err := s.q.ListSeriesOrders(ctx, id)
	if err != nil {
		return d, err
	}
	for _, o := range orders {
		d.Orders = append(d.Orders, Order{Type: o.Type, Name: o.Name, AlternateName: o.AlternateName.String})
	}

	titles, err := s.q.ListSeriesTitles(ctx, id)
	if err != nil {
		return d, err
	}
	for _, t := range titles {
		d.Titles = append(d.Titles, Title{
			Title: t.Title, Language: t.Language.String, Season: intPtr(t.Season), Source: t.Source,
			SceneSeason: intPtr(t.SceneSeason), Note: t.Note.String,
		})
	}

	episodes, err := s.q.ListEpisodes(ctx, id)
	if err != nil {
		return d, err
	}
	index := make(map[int64]int, len(episodes))
	for i, e := range episodes {
		index[e.ID] = i
		d.Episodes = append(d.Episodes, Episode{
			ID: e.ID, Aired: e.Aired.String, Runtime: int(e.Runtime.Int64), Absolute: int(e.AbsoluteNumber.Int64),
			IsMovie: e.IsMovie != 0, Name: e.Name.String, NameEng: e.NameEng.String, NameDeu: e.NameDeu.String,
			Numbers: map[string]Position{},
		})
	}
	numbers, err := s.q.ListEpisodeNumbers(ctx, id)
	if err != nil {
		return d, err
	}
	for _, n := range numbers {
		if i, ok := index[n.EpisodeID]; ok {
			d.Episodes[i].Numbers[n.OrderType] = Position{Season: int(n.Season), Number: int(n.Number)}
		}
	}

	scene, err := s.q.ListSceneNumbers(ctx, id)
	if err != nil {
		return d, err
	}
	for _, n := range scene {
		sn := SceneNumber{
			TVDB: Position{Season: int(n.TvdbSeason), Number: int(n.TvdbEpisode)}, TVDBAbsolute: int(n.TvdbAbsolute.Int64),
			Scene: Position{Season: int(n.SceneSeason), Number: int(n.SceneEpisode)}, SceneAbsolute: int(n.SceneAbsolute.Int64),
		}
		if n.EpisodeID.Valid {
			id := n.EpisodeID.Int64
			sn.EpisodeID = &id
		}
		d.Scene = append(d.Scene, sn)
	}
	return d, nil
}

// SyncState returns a stored value, or "" when unset.
func (s *Store) SyncState(ctx context.Context, key string) (string, error) {
	v, err := s.q.GetSyncState(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSyncState stores a value.
func (s *Store) SetSyncState(ctx context.Context, key, value string) error {
	return s.q.SetSyncState(ctx, query.SetSyncStateParams{Key: key, Value: value})
}

func seriesFromRow(r query.Series) Series {
	s := Series{
		ID: r.ID, Slug: r.Slug, Name: r.Name, NameEng: r.NameEng.String, NameDeu: r.NameDeu.String,
		OriginalLanguage: r.OriginalLanguage, OriginalCountry: r.OriginalCountry, Status: r.Status,
		Year: int(r.Year.Int64), FirstAired: r.FirstAired.String, AverageRuntime: int(r.AverageRuntime.Int64),
		DefaultOrder: r.DefaultOrder, Profile: r.Profile.String, TVDBUpdated: r.TvdbUpdated,
		Added: time.Unix(r.AddedAt, 0), Fetched: time.Unix(r.FetchedAt, 0),
		Network: r.Network.String, LastAired: r.LastAired.String, Overview: r.Overview.String,
		OverviewLanguage: r.OverviewLanguage.String, HasDetails: r.Network.Valid,
		Monitor: r.Monitor, MonitorSince: time.Unix(r.MonitorSince, 0),
	}
	if r.XemFetchedAt.Valid {
		t := time.Unix(r.XemFetchedAt.Int64, 0)
		s.XEMFetched = &t
	}
	return s
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullInt(n int) sql.NullInt64 { return sql.NullInt64{Int64: int64(n), Valid: n != 0} }

func nullIntPtr(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}

func intPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// EpisodeOverview returns TVDB's description of an episode and its
// language; both are empty when TVDB has none.
func (s *Store) EpisodeOverview(ctx context.Context, episodeID int64) (overview, language string, err error) {
	r, err := s.q.GetEpisodeOverview(ctx, episodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("episode %d: %w", episodeID, ErrNotFound)
	}
	return r.Overview, r.OverviewLanguage, err
}
