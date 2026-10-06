package match

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/store"
)

// aired is TVDB's season type for the aired order.
const aired = "official"

// Options holds what matching needs beyond the catalogue.
type Options struct {
	// Anime reads an ambiguous bare number, such as 101, as an absolute
	// number first; regular TV reads it as season and episode first.
	Anime bool
	// ServiceOrders holds reviewed rules: releases from a streaming service
	// follow this TVDB episode order (season type), such as "alternate".
	ServiceOrders map[release.Service]string
}

// Show is a catalogue entry prepared for matching release names.
type Show struct {
	opts     Options
	name     string
	titles   []showTitle
	orders   []store.Order
	episodes map[int64]*episode
	// positions maps an order type and position to an episode.
	positions map[string]map[store.Position]int64
	// absolute numbers regular episodes in aired order, the way release
	// groups count. TVDB's absolute order can include specials and
	// TheXEM's absolute numbers restart every season, so neither fits.
	absolute map[int]int64
	// scene maps TheXEM's scene numbering, which releases use, to TVDB.
	scene map[store.Position]store.SceneNumber
}

type showTitle struct {
	text
	source    string
	season    int
	hasSeason bool
	// scene means season is in TheXEM's scene numbering, not TVDB's.
	scene bool
}

type episode struct {
	store.Episode
	names []text
}

// NewShow prepares a show for matching.
func NewShow(d store.SeriesData, opts Options) *Show {
	s := &Show{
		opts: opts, name: cmp.Or(d.Series.NameEng, d.Series.Name), orders: d.Orders,
		episodes: map[int64]*episode{}, positions: map[string]map[store.Position]int64{},
		absolute: map[int]int64{}, scene: map[store.Position]store.SceneNumber{},
	}
	for _, n := range []string{d.Series.Name, d.Series.NameEng, d.Series.NameDeu} {
		if t := newText(n); !t.empty() {
			s.titles = append(s.titles, showTitle{text: t, source: "tvdb"})
		}
	}
	for _, t := range d.Titles {
		st := showTitle{text: newText(t.Title), source: t.Source}
		switch {
		case t.Source == "xem" && t.SceneSeason != nil:
			st.season, st.hasSeason, st.scene = *t.SceneSeason, true, true
		case t.Season != nil:
			st.season, st.hasSeason = *t.Season, true
		}
		if !st.empty() {
			s.titles = append(s.titles, st)
		}
	}

	var regular []*episode
	for _, e := range d.Episodes {
		ep := &episode{Episode: e}
		for _, n := range []string{e.Name, e.NameEng, e.NameDeu} {
			if t := newText(n); !t.empty() {
				ep.names = append(ep.names, t)
			}
		}
		s.episodes[e.ID] = ep
		for order, p := range e.Numbers {
			if s.positions[order] == nil {
				s.positions[order] = map[store.Position]int64{}
			}
			s.positions[order][p] = e.ID
		}
		if p, ok := e.Numbers[aired]; ok && p.Season > 0 {
			regular = append(regular, ep)
		}
	}
	slices.SortFunc(regular, func(a, b *episode) int { return comparePosition(a.Numbers[aired], b.Numbers[aired]) })
	for i, e := range regular {
		s.absolute[i+1] = e.ID
	}
	for _, n := range d.Scene {
		if _, dup := s.scene[n.Scene]; !dup {
			s.scene[n.Scene] = n
		}
	}
	return s
}

// Label names an episode by its aired-order position, such as S01E03.
func (s *Show) Label(id int64) string {
	if e, ok := s.episodes[id]; ok {
		if p, ok := e.Numbers[aired]; ok {
			return position(p)
		}
	}
	return fmt.Sprintf("TVDB episode %d", id)
}

// describe names a set of episodes: one label, or the first and last.
func (s *Show) describe(ids []int64) string {
	switch len(ids) {
	case 0:
		return "no episodes"
	case 1:
		return s.Label(ids[0])
	}
	return fmt.Sprintf("%s to %s (%d episodes)", s.Label(ids[0]), s.Label(ids[len(ids)-1]), len(ids))
}

// sceneEpisode finds the episode at a position in release numbering. The
// note is set when TheXEM moves it to another TVDB position.
func (s *Show) sceneEpisode(season int, ep release.Episode) (id int64, note, err string) {
	if ep.Fraction > 0 {
		return 0, "", fmt.Sprintf("TVDB has no S%02dE%s; half episodes such as recaps are usually specials", season, ep)
	}
	p := store.Position{Season: season, Number: ep.Number}
	if n, ok := s.scene[p]; ok {
		if n.EpisodeID == nil {
			return 0, "", fmt.Sprintf("TheXEM maps %s to TVDB %s, which TVDB does not list", position(p), position(n.TVDB))
		}
		if n.TVDB != p {
			note = fmt.Sprintf("TheXEM maps %s to TVDB %s", position(p), position(n.TVDB))
		}
		return *n.EpisodeID, note, ""
	}
	return s.orderEpisode(aired, season, ep)
}

// orderEpisode finds the episode at a position in one TVDB order.
func (s *Show) orderEpisode(order string, season int, ep release.Episode) (id int64, note, err string) {
	if ep.Fraction > 0 {
		return 0, "", fmt.Sprintf("TVDB has no S%02dE%s; half episodes such as recaps are usually specials", season, ep)
	}
	p := store.Position{Season: season, Number: ep.Number}
	if id, ok := s.positions[order][p]; ok {
		return id, "", ""
	}
	if order != aired {
		return 0, "", fmt.Sprintf("the %s order has no %s", s.orderName(order), position(p))
	}
	return 0, "", fmt.Sprintf("TVDB has no %s", position(p))
}

// absoluteEpisode finds an episode by its number among all regular episodes.
func (s *Show) absoluteEpisode(ep release.Episode) (int64, string) {
	if ep.Fraction > 0 {
		return 0, fmt.Sprintf("TVDB has no episode %s; half episodes such as recaps are usually specials", ep)
	}
	if id, ok := s.absolute[ep.Number]; ok {
		return id, ""
	}
	return 0, fmt.Sprintf("TVDB lists %d regular episodes, so there is no episode %d", len(s.absolute), ep.Number)
}

// season lists a season's episodes in release numbering: TheXEM's scene
// season if it has one, otherwise TVDB's season in the given order.
func (s *Show) season(order string, season int) []int64 {
	var ids []int64
	if order == aired {
		for p, n := range s.scene {
			if p.Season == season && n.EpisodeID != nil {
				ids = append(ids, *n.EpisodeID)
			}
		}
	}
	if len(ids) == 0 {
		for p, id := range s.positions[order] {
			if p.Season == season {
				ids = append(ids, id)
			}
		}
	}
	s.sortEpisodes(ids, order)
	return ids
}

func (s *Show) sortEpisodes(ids []int64, order string) {
	slices.SortFunc(ids, func(a, b int64) int {
		return comparePosition(s.episodes[a].Numbers[order], s.episodes[b].Numbers[order])
	})
}

// orderName is how an order is called, such as "Netflix".
func (s *Show) orderName(order string) string {
	for _, o := range s.orders {
		if o.Type == order {
			name := cmp.Or(o.AlternateName, o.Name)
			return strings.TrimSpace(strings.TrimSuffix(name, " Order"))
		}
	}
	return order
}

// serviceOrder finds the TVDB order releases from a service may follow: one
// a reviewed rule names, or one named after the service.
func (s *Show) serviceOrder(services []release.Claim[release.Service]) (order string, rule bool) {
	for _, sv := range services {
		if o, ok := s.opts.ServiceOrders[sv.Value]; ok {
			return o, true
		}
	}
	for _, sv := range services {
		for _, o := range s.orders {
			if o.Type == aired {
				continue
			}
			if slices.Contains(Words(o.AlternateName+" "+o.Name), string(sv.Value)) {
				return o.Type, false
			}
		}
	}
	return "", false
}

func comparePosition(a, b store.Position) int {
	return cmp.Or(cmp.Compare(a.Season, b.Season), cmp.Compare(a.Number, b.Number))
}

func position(p store.Position) string { return fmt.Sprintf("S%02dE%02d", p.Season, p.Number) }
