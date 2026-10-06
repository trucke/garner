// Package match turns what a release name claims into TVDB episodes of one
// show, or into "unknown" with a reason. It only reads the catalogue.
package match

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/trucke/garner/internal/release"
)

// Kind is what a release turned out to hold.
type Kind int

const (
	// Unknown means the release could not be matched; Reason says why.
	Unknown Kind = iota
	// Episodes means specific episodes.
	Episodes
	// Season means a season pack. It claims the season, not that every
	// episode is included.
	Season
	// Extra means content that is not a regular episode, such as a
	// creditless opening. It never satisfies an episode.
	Extra
)

func (k Kind) String() string {
	return [...]string{"unknown", "episodes", "season", "extra"}[k]
}

// Match is what a release holds.
type Match struct {
	Kind Kind
	// Episodes are TVDB episode IDs. For a season pack they are the
	// season's episodes.
	Episodes []int64
	// Uncertain means the file may hold other episodes than the name says;
	// file inspection decides.
	Uncertain bool
	// Reason explains Unknown and Extra in plain words.
	Reason string
	// Notes record decisions worth showing, such as an episode title
	// overruling a number.
	Notes []string
}

// Match reads which of the show's episodes a release holds. published is
// when the indexer received the release; zero if unknown.
func (s *Show) Match(c release.Claims, published time.Time) Match {
	if len(c.Extras) > 0 {
		var extras []string
		for _, e := range c.Extras {
			extras = append(extras, e.Value)
		}
		return Match{Kind: Extra, Reason: fmt.Sprintf("the name marks extra content (%s), not a regular episode", strings.Join(extras, ", "))}
	}
	if len(c.Series) == 0 {
		return Match{Reason: "the name has no show title"}
	}
	series, failure := s.matchSeries(c.Series)
	if failure != "" {
		return Match{Reason: failure}
	}
	m := &matcher{s: s, c: c, published: published, hint: series.hint}

	// A number that is part of the matched title is no episode number.
	var numbering []release.Numbering
	for _, n := range c.Numbering {
		if overlaps(n.Span, series.claim.Span) {
			m.note("%q belongs to the title", c.Name[n.Span.Start:n.Span.End])
			continue
		}
		numbering = append(numbering, n)
	}

	var got []reading
	for _, alts := range groupBySpan(numbering) {
		r := m.readGroup(alts)
		if !r.ok() {
			failure = r.err
			break
		}
		got = append(got, r)
	}
	if failure == "" && len(got) > 1 {
		for _, r := range got[1:] {
			if !slices.Equal(r.ids, got[0].ids) {
				failure = fmt.Sprintf("the name's numbers disagree: %s is %s, but %s is %s",
					got[0].label, s.Describe(got[0].ids), r.label, s.Describe(r.ids))
				break
			}
		}
	}
	if failure == "" && len(numbering) == 0 {
		failure = "the name has no episode or season number"
	}

	res := Match{}
	switch title, ok := m.episodeTitle(); {
	case failure != "" && ok:
		// Title beats number when the title clearly names one episode.
		m.note("%s; the episode title %q is %s", failure, c.EpisodeTitle[0].Value, s.Label(title))
		res = Match{Kind: Episodes, Episodes: []int64{title}}
	case failure != "":
		res = Match{Reason: failure}
	case ok && !got[0].pack && len(got[0].ids) == 1 && got[0].ids[0] != title:
		m.note("the name says %s, but its episode title %q is %s", s.Label(got[0].ids[0]), c.EpisodeTitle[0].Value, s.Label(title))
		res = Match{Kind: Episodes, Episodes: []int64{title}}
	default:
		for _, r := range got {
			if r.note != "" {
				m.note("%s", r.note)
			}
		}
		res = Match{Kind: Episodes, Episodes: got[0].ids}
		if got[0].pack {
			res.Kind = Season
		}
	}
	m.checkSpecial(&res)
	res.Notes = m.notes
	return res
}

type matcher struct {
	s         *Show
	c         release.Claims
	published time.Time
	hint      *seasonHint
	notes     []string
}

func (m *matcher) note(format string, args ...any) {
	m.notes = append(m.notes, fmt.Sprintf(format, args...))
}

// reading is one way to read a numbering claim.
type reading struct {
	ids      []int64
	pack     bool
	seasoned bool
	label    string // what the name says, such as "S01E04" or "episode 29"
	note     string // how it was read, when that is worth showing
	err      string // why it fails
}

func (r reading) ok() bool { return r.err == "" }

// readGroup picks among alternative readings of one number, such as 101
// read as S01E01 or as episode 101.
func (m *matcher) readGroup(alts []release.Numbering) reading {
	var ok []reading
	var first reading
	for i, a := range alts {
		r := m.readAlt(a)
		if i == 0 {
			first = r
		}
		if r.ok() {
			ok = append(ok, r)
		}
	}
	if len(ok) <= 1 {
		if len(ok) == 1 {
			return ok[0]
		}
		return first
	}
	if !m.published.IsZero() {
		var out []reading
		for _, r := range ok {
			if m.s.airedBy(r.ids, m.published) {
				out = append(out, r)
			} else {
				m.note("%s is ruled out: %s had not aired when the release was posted", r.label, m.s.Describe(r.ids))
			}
		}
		if len(out) == 0 {
			return reading{err: fmt.Sprintf("none of the readings of %q had aired when the release was posted", raw(alts[0], m.c.Name))}
		}
		ok = out
	}
	if len(ok) > 1 {
		var preferred []reading
		for _, r := range ok {
			if r.seasoned != m.s.opts.Anime {
				preferred = append(preferred, r)
			}
		}
		if len(preferred) == 1 {
			kind := "regular TV reads season and episode first"
			if m.s.opts.Anime {
				kind = "anime reads an absolute number first"
			}
			m.note("%q could mean %s or %s; %s", raw(alts[0], m.c.Name), m.s.Describe(ok[0].ids), m.s.Describe(ok[1].ids), kind)
			ok = preferred
		}
	}
	if len(ok) > 1 {
		return reading{err: fmt.Sprintf("%q could mean %s or %s", raw(alts[0], m.c.Name), m.s.Describe(ok[0].ids), m.s.Describe(ok[1].ids))}
	}
	return ok[0]
}

// readAlt reads one numbering claim in aired order and, when the release
// names a streaming service with its own TVDB order, in that order too.
func (m *matcher) readAlt(n release.Numbering) reading {
	r := m.readAired(n)
	order, rule := m.s.serviceOrder(m.c.Services)
	if order == "" || !n.Seasoned {
		return r
	}
	alt := m.readOrder(n, order)
	name := m.s.orderName(order)
	switch {
	case !alt.ok():
		return r
	case !r.ok():
		alt.note = fmt.Sprintf("read in the %s order", name)
		return alt
	case slices.Equal(r.ids, alt.ids):
		return r
	case rule:
		alt.note = fmt.Sprintf("read in the %s order: a reviewed rule says %s releases follow it", name, name)
		return alt
	}
	return reading{label: r.label, err: fmt.Sprintf("%s is %s in aired order but %s in the %s order, and no reviewed rule says which one %s releases follow",
		r.label, m.s.Describe(r.ids), m.s.Describe(alt.ids), name, name)}
}

func (m *matcher) readAired(n release.Numbering) reading {
	r := reading{label: n.String(), seasoned: n.Seasoned, pack: n.Seasoned && len(n.Episodes) == 0}
	switch {
	case r.pack:
		r.ids = m.s.season(aired, n.Season)
		if len(r.ids) == 0 {
			r.err = fmt.Sprintf("TVDB has no season %d", n.Season)
		}
	case n.Seasoned:
		r.ids, r.note, r.err = m.episodes(n.Episodes, func(e release.Episode) (int64, string, string) {
			return m.s.sceneEpisode(n.Season, e)
		})
	default:
		r.label = label(n, m.c.Name)
		if h := m.hint; h != nil {
			ids, note, err := m.episodes(n.Episodes, func(e release.Episode) (int64, string, string) {
				if h.scene {
					return m.s.sceneEpisode(h.season, e)
				}
				return m.s.orderEpisode(aired, h.season, e)
			})
			if err == "" {
				r.ids, r.note = ids, fmt.Sprintf("season %d comes from the title", h.season)
				if note != "" {
					r.note += "; " + note
				}
				return r
			}
			r.err = err
			if !h.soft {
				return r
			}
		}
		ids, _, err := m.episodes(n.Episodes, func(e release.Episode) (int64, string, string) {
			id, err := m.s.absoluteEpisode(e)
			return id, "", err
		})
		if err == "" {
			r.ids, r.err = ids, ""
		} else if r.err == "" {
			r.err = err
		}
	}
	return r
}

func (m *matcher) readOrder(n release.Numbering, order string) reading {
	r := reading{label: n.String(), seasoned: true, pack: len(n.Episodes) == 0}
	if r.pack {
		r.ids = m.s.season(order, n.Season)
		if len(r.ids) == 0 {
			r.err = fmt.Sprintf("the %s order has no season %d", m.s.orderName(order), n.Season)
		}
		return r
	}
	r.ids, _, r.err = m.episodes(n.Episodes, func(e release.Episode) (int64, string, string) {
		return m.s.orderEpisode(order, n.Season, e)
	})
	return r
}

// episodes looks up each episode and returns the first note and error.
func (m *matcher) episodes(eps []release.Episode, find func(release.Episode) (int64, string, string)) (ids []int64, note, err string) {
	for _, ep := range eps {
		id, n, failed := find(ep)
		if failed != "" {
			return nil, "", failed
		}
		ids = append(ids, id)
		if note == "" {
			note = n
		}
	}
	return ids, note, ""
}

// episodeTitle finds the one episode the name's episode title names.
func (m *matcher) episodeTitle() (int64, bool) {
	if len(m.c.EpisodeTitle) == 0 {
		return 0, false
	}
	t := newText(m.c.EpisodeTitle[0].Value)
	var exact, near []int64
	for id, e := range m.s.episodes {
		for _, name := range e.names {
			if t.equal(name) {
				exact = append(exact, id)
				break
			}
			if t.close(name, 3) && !slices.Contains(near, id) {
				near = append(near, id)
			}
		}
	}
	found := exact
	if len(found) == 0 {
		found = near
	}
	switch len(found) {
	case 0:
		return 0, false
	case 1:
		return found[0], true
	}
	m.note("the episode title %q fits %d episodes, so the number decides", m.c.EpisodeTitle[0].Value, len(found))
	return 0, false
}

// checkSpecial marks a match uncertain when the name mentions a special and
// TVDB lists several episodes on the episode's air date: a double-length
// premiere may hold all of them.
func (m *matcher) checkSpecial(res *Match) {
	words := Words(m.c.Name)
	if res.Kind != Episodes || len(res.Episodes) != 1 || !slices.Contains(words, "special") && !slices.Contains(words, "specials") {
		return
	}
	e := m.s.episodes[res.Episodes[0]]
	p, ok := e.Numbers[aired]
	if !ok || p.Season == 0 || e.Aired == "" {
		return
	}
	same := 0
	for _, o := range m.s.episodes {
		if op, ok := o.Numbers[aired]; ok && op.Season == p.Season && o.Aired == e.Aired {
			same++
		}
	}
	if same > 1 {
		res.Uncertain = true
		m.note("the name mentions a special, and TVDB lists %d episodes aired on %s; the file may hold more than one", same, e.Aired)
	}
}

// airedBy reports whether all episodes had aired by the given time, with a
// day's margin for time zones. Episodes without a date count as aired.
func (s *Show) airedBy(ids []int64, t time.Time) bool {
	for _, id := range ids {
		d, err := time.Parse(time.DateOnly, s.episodes[id].Aired)
		if err == nil && d.After(t.AddDate(0, 0, 1)) {
			return false
		}
	}
	return true
}

type seasonHint struct {
	season int
	scene  bool
	// soft means the title also names the whole show, as when TheXEM lists
	// the plain show title under season 1. A number that season lacks is
	// then read as an absolute number instead.
	soft bool
}

type seriesMatch struct {
	claim release.Claim[string]
	exact bool
	hint  *seasonHint
}

// matchSeries finds the title candidate that best matches the show: exact
// matches before ones that differ by a word, earlier candidates first. When
// only a part of a "Title - Subtitle" name matches, every part must match:
// the other part may name a spin-off ("Toro no Minato - Kaze no Hibi").
func (s *Show) matchSeries(claims []release.Claim[string]) (seriesMatch, string) {
	var best seriesMatch
	bestAt := -1
	matched := make([]bool, len(claims))
	for i, c := range claims {
		cand, ok := s.matchTitle(c)
		matched[i] = ok
		if ok && (bestAt < 0 || cand.exact && !best.exact) {
			best, bestAt = cand, i
		}
	}
	if bestAt < 0 {
		return best, fmt.Sprintf("the title %q does not match %s", claims[0].Value, s.name)
	}
	full := claims[0]
	if isPart(claims[bestAt], full) {
		for i, c := range claims {
			if isPart(c, full) && !matched[i] {
				return best, fmt.Sprintf("only part of the title %q matches %s; %q may name a spin-off or a season", full.Value, s.name, c.Value)
			}
		}
	}
	return best, ""
}

func (s *Show) matchTitle(c release.Claim[string]) (seriesMatch, bool) {
	t := newText(c.Value)
	if t.empty() {
		return seriesMatch{}, false
	}
	var exact, near []showTitle
	for _, st := range s.titles {
		switch {
		case t.equal(st.text):
			exact = append(exact, st)
		case t.close(st.text, 2):
			near = append(near, st)
		}
	}
	if len(exact) > 0 {
		return seriesMatch{claim: c, exact: true, hint: seasonFromTitles(t, exact, true)}, true
	}
	if len(near) > 0 {
		return seriesMatch{claim: c, hint: seasonFromTitles(t, near, false)}, true
	}
	return seriesMatch{}, false
}

// isPart reports whether a title candidate is a part of the full title.
func isPart(c, full release.Claim[string]) bool {
	return c.Span != full.Span && c.Span.Start >= full.Span.Start && c.Span.End <= full.Span.End
}

// seasonFromTitles reads the season a matched title stands for. A release
// title that adds only a number 2 to 9 to a known title ("Show III")
// suggests that season; readings using it must still find the episodes.
// Otherwise the season comes from reviewed corrections first, then TheXEM.
func seasonFromTitles(t text, titles []showTitle, exact bool) *seasonHint {
	if !exact {
		for _, st := range titles {
			if len(t.words) != len(st.words)+1 {
				continue
			}
			for _, w := range t.words {
				if len(w) == 1 && w >= "2" && w <= "9" && !slices.Contains(st.words, w) {
					return &seasonHint{season: int(w[0] - '0'), scene: true}
				}
			}
		}
	}
	soft := slices.ContainsFunc(titles, func(st showTitle) bool { return !st.hasSeason })
	for _, source := range []string{"garner", "xem"} {
		seasons := map[seasonHint]bool{}
		for _, st := range titles {
			if st.source == source && st.hasSeason {
				seasons[seasonHint{season: st.season, scene: st.scene, soft: soft}] = true
			}
		}
		if len(seasons) > 1 {
			return nil
		}
		for h := range seasons {
			return &h
		}
	}
	return nil
}

// groupBySpan groups alternative readings of the same number.
func groupBySpan(ns []release.Numbering) [][]release.Numbering {
	var groups [][]release.Numbering
	for _, n := range ns {
		if i := slices.IndexFunc(groups, func(g []release.Numbering) bool { return g[0].Span == n.Span }); i >= 0 {
			groups[i] = append(groups[i], n)
		} else {
			groups = append(groups, []release.Numbering{n})
		}
	}
	return groups
}

// label is how a numbering claim reads in messages: S01E04, or the number
// as the name writes it.
func label(n release.Numbering, name string) string {
	if n.Seasoned {
		return n.String()
	}
	return "episode " + raw(n, name)
}

// raw is the number as the name writes it.
func raw(n release.Numbering, name string) string {
	return strings.Trim(name[n.Span.Start:n.Span.End], " .-_[]()")
}

func overlaps(a, b release.Span) bool { return a.Start < b.End && b.Start < a.End }
