// Package catalog keeps Garner's knowledge of shows: TVDB's series, episodes
// and episode orders, plus TheXEM's scene numbering and alternative titles,
// with reviewed corrections on top.
package catalog

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/store"
)

// ErrExists means the show is already in the catalogue.
var ErrExists = errors.New("already in the catalogue")

// ErrUnknownSeries means TVDB has no show with that ID.
var ErrUnknownSeries = errors.New("TVDB has no series with that ID")

//go:embed corrections.toml
var correctionsFile string

// Languages whose episode titles Garner keeps besides the original.
var languages = []string{"eng", "deu"}

// Catalog loads shows from TVDB and TheXEM into the store.
type Catalog struct {
	store       *store.Store
	tvdb        *TVDB
	xem         *XEM
	log         *slog.Logger
	corrections corrections
	// OnChange is called after a show was added or refreshed.
	OnChange func(seriesID int64)
}

type corrections struct {
	Title []titleCorrection `toml:"title"`
	Order []orderRule       `toml:"order"`
}

// orderRule says which TVDB episode order a streaming service's releases
// of a show follow.
type orderRule struct {
	Series  int64           `toml:"series"`
	Service release.Service `toml:"service"`
	Order   string          `toml:"order"`
	Reason  string          `toml:"reason"`
}

type titleCorrection struct {
	Series int64  `toml:"series"`
	Title  string `toml:"title"`
	Season *int   `toml:"season"` // a pointer, so a missing season is not read as specials
	Reason string `toml:"reason"`
}

// New returns a catalogue. xem may be nil to skip TheXEM.
func New(st *store.Store, tvdb *TVDB, xem *XEM, log *slog.Logger) (*Catalog, error) {
	var c corrections
	meta, err := toml.Decode(correctionsFile, &c)
	if err != nil {
		return nil, fmt.Errorf("corrections: %w", err)
	}
	if u := meta.Undecoded(); len(u) > 0 {
		return nil, fmt.Errorf("corrections: unknown keys %v", u)
	}
	for _, t := range c.Title {
		if t.Series == 0 || t.Title == "" || t.Season == nil || t.Reason == "" {
			return nil, fmt.Errorf("corrections: title %q needs series, title, season and reason", t.Title)
		}
	}
	seen := map[orderRule]bool{}
	for _, o := range c.Order {
		if o.Series == 0 || o.Service == "" || o.Order == "" || o.Reason == "" {
			return nil, fmt.Errorf("corrections: order rule for series %d needs series, service, order and reason", o.Series)
		}
		key := orderRule{Series: o.Series, Service: o.Service}
		if seen[key] {
			return nil, fmt.Errorf("corrections: two order rules for series %d and %s", o.Series, o.Service)
		}
		seen[key] = true
	}
	return &Catalog{store: st, tvdb: tvdb, xem: xem, log: log, corrections: c}, nil
}

// SearchResult is a show TVDB found.
type SearchResult struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug,omitempty"`
	Name     string `json:"name"`
	NameEng  string `json:"name_eng,omitempty"`
	NameDeu  string `json:"name_deu,omitempty"`
	Year     int    `json:"year,omitempty"`
	Status   string `json:"status,omitempty"`
	Language string `json:"language,omitempty"`
	Network  string `json:"network,omitempty"`
	Added    bool   `json:"added"`
}

// Add loads a show into the catalogue with the monitor mode, such as
// store.MonitorNew.
func (c *Catalog) Add(ctx context.Context, id int64, monitor string) error {
	exists, err := c.store.HasSeries(ctx, id)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("series %d: %w", id, ErrExists)
	}
	return c.load(ctx, id, monitor)
}

// Refresh reloads a show from TVDB and TheXEM. If TheXEM fails, the show's
// previous scene data is kept and the failure logged.
func (c *Catalog) Refresh(ctx context.Context, id int64) error {
	return c.load(ctx, id, "")
}

// load fetches a show and saves it, adding it with the monitor mode when
// one is given. Otherwise the show must still be in the catalogue when
// saving: a show deleted while it loaded stays deleted, and load returns
// store.ErrNotFound.
func (c *Catalog) load(ctx context.Context, id int64, monitor string) error {
	start := time.Now()
	d, err := c.fetchTVDB(ctx, id)
	if err != nil {
		return err
	}
	scene, titles, err := c.fetchXEM(ctx, id, d.Episodes)
	gotScene := err == nil
	if err != nil {
		c.log.Warn("TheXEM unavailable; keeping previous scene data", "series", id, "err", err)
		if old, oldErr := c.store.LoadSeries(ctx, id); oldErr == nil {
			titles = sceneTitles(old.Titles)
		}
	}
	d.Scene = scene
	d.Titles = append(d.Titles, c.applyCorrections(id, titles)...)
	save := c.store.UpdateSeries
	if monitor != "" {
		d.Series.Monitor = monitor
		save = c.store.SaveSeries
	}
	if err := save(ctx, d, gotScene, time.Now()); err != nil {
		return err
	}
	c.log.Info("loaded series", "id", id, "name", displayName(d.Series), "episodes", len(d.Episodes),
		"orders", len(d.Orders), "scene_numbers", len(scene), "duration", time.Since(start).Round(time.Millisecond))
	if c.OnChange != nil {
		c.OnChange(id)
	}
	return nil
}

func (c *Catalog) fetchTVDB(ctx context.Context, id int64) (store.SeriesData, error) {
	s, err := c.tvdb.series(ctx, id)
	if errors.Is(err, errNotFound) {
		return store.SeriesData{}, fmt.Errorf("series %d: %w", id, ErrUnknownSeries)
	}
	if err != nil {
		return store.SeriesData{}, err
	}
	year, _ := strconv.Atoi(s.Year)
	d := store.SeriesData{Series: store.Series{
		ID: s.ID, Slug: s.Slug, Name: s.Name, OriginalLanguage: s.OriginalLanguage, OriginalCountry: s.OriginalCountry,
		Status: s.Status.Name, Year: year, FirstAired: s.FirstAired, LastAired: s.LastAired, AverageRuntime: s.AverageRuntime,
		TVDBUpdated: s.LastUpdated,
	}}
	if s.OriginalNetwork != nil {
		d.Series.Network = s.OriginalNetwork.Name
	}
	// The profile is only stored for new shows; a changed one stays.
	d.Series.Profile = profileFor(s)
	for _, a := range s.Aliases {
		d.Titles = append(d.Titles, store.Title{Title: a.Name, Language: a.Language, Source: "tvdb"})
	}
	for _, lang := range languages {
		tr, ok, err := c.tvdb.translation(ctx, id, lang)
		if err != nil {
			return d, err
		}
		if !ok {
			continue
		}
		if lang == "eng" {
			d.Series.NameEng = tr.Name
		} else {
			d.Series.NameDeu = tr.Name
		}
		if d.Series.Overview == "" && tr.Overview != "" {
			d.Series.Overview, d.Series.OverviewLanguage = tr.Overview, lang
		}
		for _, a := range tr.Aliases {
			d.Titles = append(d.Titles, store.Title{Title: a, Language: lang, Source: "tvdb"})
		}
	}
	if d.Series.Overview == "" && s.Overview != "" {
		d.Series.Overview, d.Series.OverviewLanguage = s.Overview, s.OriginalLanguage
	}
	d.Titles = dedupeTitles(d.Titles)

	episodes := map[int64]*store.Episode{}
	var ids []int64
	for _, st := range s.SeasonTypes {
		if st.ID == s.DefaultSeason {
			d.Series.DefaultOrder = st.Type
		}
		order := store.Order{Type: st.Type, Name: st.Name}
		if st.AlternateName != nil {
			order.AlternateName = *st.AlternateName
		}
		d.Orders = append(d.Orders, order)
		list, _, err := c.tvdb.episodes(ctx, id, st.Type, "")
		if err != nil {
			return d, err
		}
		for _, e := range list {
			ep, ok := episodes[e.ID]
			if !ok {
				ep = &store.Episode{ID: e.ID, Numbers: map[string]store.Position{}}
				episodes[e.ID] = ep
				ids = append(ids, e.ID)
			}
			// An episode listed twice in one order keeps its first position.
			if _, seen := ep.Numbers[st.Type]; !seen {
				ep.Numbers[st.Type] = store.Position{Season: e.SeasonNumber, Number: e.Number}
			}
			if st.Type == "official" {
				ep.Aired, ep.Runtime, ep.Absolute, ep.IsMovie = deref(e.Aired), derefInt(e.Runtime), e.AbsoluteNumber, e.IsMovie != 0
				ep.Name = deref(e.Name)
				if ov := deref(e.Overview); ov != "" {
					ep.Overview, ep.OverviewLanguage = ov, s.OriginalLanguage
				}
			}
		}
	}
	if d.Series.DefaultOrder == "" {
		d.Series.DefaultOrder = "official"
	}
	// Overviews as for the show: English, else German, else the original.
	translated := map[int64]bool{}
	for _, lang := range languages {
		list, ok, err := c.tvdb.episodes(ctx, id, "official", lang)
		if err != nil {
			return d, err
		}
		if !ok {
			continue
		}
		for _, e := range list {
			ep, found := episodes[e.ID]
			if !found {
				continue
			}
			if lang == "eng" {
				ep.NameEng = deref(e.Name)
			} else {
				ep.NameDeu = deref(e.Name)
			}
			if ov := deref(e.Overview); ov != "" && !translated[e.ID] {
				ep.Overview, ep.OverviewLanguage, translated[e.ID] = ov, lang, true
			}
		}
	}
	for _, id := range ids {
		d.Episodes = append(d.Episodes, *episodes[id])
	}
	return d, nil
}

// fetchXEM loads TheXEM's scene numbering and titles. Titles come keyed by
// scene season; they are translated to TVDB seasons through the numbering.
func (c *Catalog) fetchXEM(ctx context.Context, id int64, episodes []store.Episode) ([]store.SceneNumber, []store.Title, error) {
	if c.xem == nil {
		return nil, nil, nil
	}
	mappings, err := c.xem.mappings(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	byPosition := map[store.Position]int64{}
	for _, e := range episodes {
		if p, ok := e.Numbers["official"]; ok {
			byPosition[p] = e.ID
		}
	}
	sceneToTVDB := map[int]map[int]bool{}
	var scene []store.SceneNumber
	for _, m := range mappings {
		if m.Scene == nil || m.TVDB == nil {
			continue
		}
		n := store.SceneNumber{
			TVDB: store.Position{Season: m.TVDB.Season, Number: m.TVDB.Episode}, TVDBAbsolute: m.TVDB.Absolute,
			Scene: store.Position{Season: m.Scene.Season, Number: m.Scene.Episode}, SceneAbsolute: m.Scene.Absolute,
		}
		if epID, ok := byPosition[n.TVDB]; ok {
			n.EpisodeID = &epID
		}
		scene = append(scene, n)
		if sceneToTVDB[n.Scene.Season] == nil {
			sceneToTVDB[n.Scene.Season] = map[int]bool{}
		}
		sceneToTVDB[n.Scene.Season][n.TVDB.Season] = true
	}

	names, err := c.xem.titles(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	var titles []store.Title
	for key, byLang := range names {
		t := store.Title{Source: "xem"}
		if key != "all" {
			sceneSeason, err := strconv.Atoi(key)
			if err != nil {
				continue
			}
			t.SceneSeason = &sceneSeason
			tvdbSeasons := sceneToTVDB[sceneSeason]
			switch {
			case len(mappings) == 0:
				// Without a mapping, scene numbering equals TVDB numbering.
				t.Season = &sceneSeason
			case len(tvdbSeasons) == 1:
				for s := range tvdbSeasons {
					t.Season = &s
				}
			default:
				t.Note = fmt.Sprintf("scene season %d does not map to exactly one TVDB season", sceneSeason)
			}
		}
		for lang, list := range byLang {
			for _, name := range list {
				t := t
				t.Title, t.Language = string(name), lang
				titles = append(titles, t)
			}
		}
	}
	slices.SortFunc(titles, func(a, b store.Title) int { return strings.Compare(a.Title, b.Title) })
	return scene, titles, nil
}

// applyCorrections adds reviewed corrections for the show and drops TheXEM
// titles they replace.
func (c *Catalog) applyCorrections(id int64, titles []store.Title) []store.Title {
	replaced := map[string]bool{}
	var out []store.Title
	for _, corr := range c.corrections.Title {
		if corr.Series != id {
			continue
		}
		season := *corr.Season
		replaced[strings.ToLower(corr.Title)] = true
		out = append(out, store.Title{Title: corr.Title, Season: &season, Source: "garner", Note: corr.Reason})
	}
	for _, t := range titles {
		if t.Source == "xem" && replaced[strings.ToLower(t.Title)] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ServiceOrders returns the reviewed rules for a show: which TVDB episode
// order each streaming service's releases follow.
func (c *Catalog) ServiceOrders(seriesID int64) map[release.Service]string {
	out := map[release.Service]string{}
	for _, o := range c.corrections.Order {
		if o.Series == seriesID {
			out[o.Service] = o.Order
		}
	}
	return out
}

func sceneTitles(titles []store.Title) []store.Title {
	var out []store.Title
	for _, t := range titles {
		if t.Source == "xem" {
			out = append(out, t)
		}
	}
	return out
}

func dedupeTitles(titles []store.Title) []store.Title {
	seen := map[string]bool{}
	var out []store.Title
	for _, t := range titles {
		k := strings.ToLower(t.Title) + "\x00" + t.Language
		if t.Title == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	return out
}

func displayName(s store.Series) string {
	if s.NameEng != "" {
		return s.NameEng
	}
	return s.Name
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}
