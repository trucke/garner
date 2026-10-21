package release

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

var extensions = []string{".mkv", ".mp4", ".avi", ".m4v", ".ts", ".nzb"}

// Parse reads the claims in a release name. Names longer than MaxNameLength
// yield no claims; anything the parser cannot read is left out rather than
// guessed.
func Parse(name string) Claims {
	if testing.Testing() {
		return parse(name)
	}
	// In the running server a broken invariant must not take garnerd down:
	// the monitor would search the same episode again after a restart and
	// crash in a loop. Tests and fuzzers see the panic instead, so the bug
	// is found.
	return degrade(name, func() Claims { return parse(name) })
}

// degrade runs build and, when an invariant breaks, logs it together with
// the input and falls back to the claims of an unreadable name.
func degrade(name string, build func() Claims) (c Claims) {
	defer func() {
		if r := recover(); r != nil {
			slog.Default().Error("release: treating the name as unreadable after a broken invariant",
				"invariant", r, "name", name)
			c = Claims{Name: name}
		}
	}()
	return build()
}

func parse(name string) Claims {
	c := Claims{Name: name}
	if name == "" || len(name) > MaxNameLength {
		return c
	}
	body := name
	for _, ext := range extensions {
		if len(body) > len(ext) && strings.EqualFold(body[len(body)-len(ext):], ext) {
			body = body[:len(body)-len(ext)]
			break
		}
	}
	p := &parser{toks: tokenize(body), c: &c}
	p.used = make([]bool, len(p.toks))
	p.parse()
	c.verify()
	return c
}

// verify panics when a claim breaks an invariant the parser must hold:
// every span lies inside the name, ranges stay within maxEpisodes, every
// numbering claim names a season or episodes, and text values carry no
// control or format characters, which could hide their meaning in a
// terminal. Checked here, they hold in every test, on every fuzz input and
// on every real release name.
func (c *Claims) verify() {
	inName := func(what string, s Span) {
		if s.Start < 0 || s.End > len(c.Name) || s.Start >= s.End {
			panic(fmt.Sprintf("invariant broken: the %s span %v does not lie inside the %d-byte name", what, s, len(c.Name)))
		}
	}
	for _, n := range c.Numbering {
		inName("numbering", n.Span)
		if len(n.Episodes) > maxEpisodes {
			panic(fmt.Sprintf("invariant broken: a range stays within %d episodes, but %v expands to %d", maxEpisodes, n, len(n.Episodes)))
		}
		if !n.Seasoned && len(n.Episodes) == 0 {
			panic(fmt.Sprintf("invariant broken: a numbering claim names a season or episodes, but %v names neither", n))
		}
	}
	unsafe := func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }
	for _, lists := range [][]Claim[string]{c.Series, c.EpisodeTitle, c.Group, c.Extras} {
		for _, t := range lists {
			inName("text", t.Span)
			if !utf8.ValidString(t.Value) || strings.IndexFunc(t.Value, unsafe) >= 0 {
				panic(fmt.Sprintf("invariant broken: text values hold no control characters, but %q does", t.Value))
			}
		}
	}
}

type parser struct {
	toks     []token
	used     []bool
	c        *Claims
	titleEnd int
	// grouped is set for fansub names that start with "[Group]".
	grouped bool
	// trailing is the episode number that ended a fansub title, or -1.
	trailing int
	// packed is a scene-style number such as 101 that ended the title, or -1.
	packed int
	// numberAt is where an anime-style "- 01" may follow the title.
	numberAt int
}

func (p *parser) parse() {
	p.trailing, p.packed = -1, -1
	p.titleEnd = p.title(p.leadingGroup())
	p.numberAt = p.dashNumberAt()
	p.trailingGroup()
	for i := p.titleEnd; i < len(p.toks); i++ {
		if !p.used[i] {
			p.claim(i)
		}
	}
}

// leadingGroup reads "[Group] Title ..." and returns where the title starts.
func (p *parser) leadingGroup() int {
	if len(p.toks) == 0 || p.toks[0].kind != kOpen {
		return 0
	}
	end := p.closing(0)
	if end < 2 {
		return 0
	}
	p.markUsed(0, end)
	// "[a.b.teevee]" names the newsgroup, not the release group.
	if end > 3 && (strings.EqualFold(p.toks[1].text, "a") && strings.EqualFold(p.toks[2].text, "b") ||
		strings.EqualFold(p.toks[1].text, "alt") && strings.EqualFold(p.toks[2].text, "binaries")) {
		return end + 1
	}
	if end == 2 && p.toks[1].kind != kWord {
		p.used[1] = false
		p.claim(1)
		return end + 1
	}
	p.c.Group = append(p.c.Group, p.text(1, end))
	p.grouped = true
	return end + 1
}

// trailingGroup reads the "-GROUP" suffix of scene names.
func (p *parser) trailingGroup() {
	n := len(p.toks)
	k := n - 1
	for k > 0 && p.toks[k].dash == gluedDash {
		k--
	}
	if k == n-1 || k < p.titleEnd {
		return
	}
	last := n
	for last > k+1 && (reposts[strings.ToLower(p.toks[last-1].text)] || strings.HasPrefix(strings.ToLower(p.toks[last-1].text), "rakuv")) {
		last--
	}
	for g := k + 1; g < last; g++ {
		if p.toks[g].kind != kWord {
			return
		}
	}
	p.markUsed(k+1, n-1)
	if last > k+1 {
		p.c.Group = append(p.c.Group, p.text(k+1, last))
	}
}

// title reads the series title from start and returns the first index after it.
func (p *parser) title(start int) int {
	end := start
	for end < len(p.toks) && p.continuesTitle(end, start, false) {
		end++
	}
	full := end
	switch {
	// "Title-TV + SP" lists contents; TV is not part of the title.
	case end > start && end < len(p.toks) && p.toks[end].kind == kPlus && strings.EqualFold(p.toks[end-1].text, "TV"):
		end--
	// "Title FINAL SEASON S08E03" labels the season.
	case end > start && end < len(p.toks) && p.toks[end].kind == kSeasonWord && strings.EqualFold(p.toks[end-1].text, "final"):
		end--
	// Fansub style "[Group] Title 01 [1080p]" or "Title 01 - 04": the trailing
	// number is the episode. The full text stays a candidate for titles that
	// end in a number, such as "Title 100".
	case p.grouped && end-1 > start && p.toks[end-1].kind == kNumber && p.toks[end-1].dash == noDash && p.tagFollows(end) && !p.numberedAfter(end):
		end--
		p.trailing = end
	// Scene style "Show.101.HDTV": 101 may be S01E01 or episode 101. The full
	// text stays a candidate here too.
	case !p.grouped && end-1 > start && p.toks[end-1].kind == kNumber && p.toks[end-1].dash == noDash &&
		len(p.toks[end-1].text) >= 3 && p.tagFollows(end) && !p.numberedAfter(end):
		end--
		p.packed = end
	}
	if end == start {
		return end
	}
	p.c.Series = append(p.c.Series, p.text(start, end))
	cuts := []int{start}
	for i := start + 1; i < end; i++ {
		if p.toks[i].dash == spacedDash {
			cuts = append(cuts, i)
		}
	}
	if len(cuts) > 1 {
		cuts = append(cuts, end)
		for k := 0; k+1 < len(cuts); k++ {
			p.c.Series = append(p.c.Series, p.text(cuts[k], cuts[k+1]))
		}
	}
	if p.trailing >= 0 || p.packed >= 0 {
		p.c.Series = append(p.c.Series, p.text(start, full))
	}
	p.markUsed(start, end-1)
	return end
}

// numberedAfter reports whether the name states a season or episode from
// token i on, as in "[Group] Title 100 (2016) - S01". The number that
// ends the title then belongs to it.
func (p *parser) numberedAfter(i int) bool {
	for ; i < len(p.toks); i++ {
		switch p.toks[i].kind {
		case kSeasonEpisode, kSeason, kEpisode, kCross:
			return true
		case kSeasonWord, kEpWord:
			if p.next(i, kNumber) {
				return true
			}
		case kOrdinal:
			if p.next(i, kSeasonWord) {
				return true
			}
		}
	}
	return false
}

// tagFollows reports whether token i ends a title the way release tags do.
func (p *parser) tagFollows(i int) bool {
	if i >= len(p.toks) {
		return true
	}
	switch t := p.toks[i]; t.kind {
	case kOpen, kResolution, kSource, kVideo, kAudioCodec, kFlag, kMarker, kIgnore, kSubLang, kDL, kLanguage, kService:
		return true
	case kNumber:
		return t.dash == spacedDash
	}
	return false
}

// dashNumberAt finds where "- 01" may follow the title: directly, or after a
// year or bracketed note as in "Title (2023) - 01" or "Title (Mini Anime) - 01".
func (p *parser) dashNumberAt() int {
	j := p.titleEnd
	for j < len(p.toks) {
		switch p.toks[j].kind {
		case kYear:
			j++
		case kOpen:
			c := p.closing(j)
			if c < 0 {
				return j
			}
			j = c + 1
		default:
			return j
		}
	}
	return j
}

// continuesTitle reports whether token i still belongs to a title that began
// at start. Episode titles stop earlier, at service tags and years.
func (p *parser) continuesTitle(i, start int, episode bool) bool {
	t := p.toks[i]
	if p.used[i] {
		return false
	}
	numeric := t.kind == kNumber || t.kind == kDecimal || t.kind == kNumberVersion
	if numeric && i > start && t.dash != noDash {
		return false
	}
	switch t.kind {
	case kWord, kNumber, kDecimal:
		return true
	case kLanguage, kLangCode:
		return !p.languageFollowedByTag(i)
	case kSubWord, kDubWord, kService:
		return !episode
	case kYear:
		return !episode && i == start
	case kEpWord:
		return !p.next(i, kNumber)
	case kSeasonWord:
		return !p.next(i, kNumber) && !p.next(i, kDecimal) && !p.next(i, kSeasonEpisode) && !p.next(i, kSeason)
	case kOrdinal:
		return !p.next(i, kSeasonWord)
	}
	return false
}

// languageFollowedByTag tells a language claim ("German.DL") from a title word
// ("The German S01E01").
func (p *parser) languageFollowedByTag(i int) bool {
	j := i + 1
	for j < len(p.toks) && p.toks[j].dash == noDash && (p.toks[j].kind == kNumber || p.toks[j].kind == kDecimal) {
		j++
	}
	if j >= len(p.toks) {
		return true
	}
	switch p.toks[j].kind {
	case kWord, kNumber, kDecimal, kSeasonEpisode, kSeason, kEpisode, kCross, kEpWord, kSeasonWord, kOrdinal, kClose:
		return false
	}
	return true
}

func (p *parser) claim(i int) {
	t := p.toks[i]
	p.used[i] = true
	span := Span{t.start, t.end}
	switch t.kind {
	case kSeasonEpisode:
		p.seasonEpisode(i)
	case kSeason, kSeasonWord, kOrdinal:
		p.season(i)
	case kEpisode:
		p.bare(i, i, episodesOf(t.text), true)
	case kEpWord:
		if p.next(i, kNumber) {
			p.bare(i, i+1, []Episode{{Number: p.toks[i+1].num}}, true)
		}
	case kCross:
		p.cross(i)
	case kNumber, kDecimal, kNumberVersion:
		switch {
		case i == p.packed:
			p.packedEpisode(i)
		// Anime style: "Title - 29", "Title-24" or "[Group] Title 01".
		case i == p.trailing || i == p.numberAt && t.dash != noDash:
			p.animeEpisodes(i)
		}
	case kSpecial:
		p.c.Extras = append(p.c.Extras, Claim[string]{strings.ToUpper(t.text), span})
	case kResolution:
		p.c.Resolution = append(p.c.Resolution, Claim[Resolution]{Resolution(t.num), span})
	case kSource:
		p.c.Source = append(p.c.Source, Claim[Source]{t.source, span})
		if t.flag != "" {
			p.c.Flags = append(p.c.Flags, Claim[Flag]{t.flag, span})
		}
		if t.service != "" {
			p.c.Services = append(p.c.Services, Claim[Service]{t.service, span})
		}
	case kService:
		p.c.Services = append(p.c.Services, Claim[Service]{t.service, span})
	case kFlag:
		p.c.Flags = append(p.c.Flags, Claim[Flag]{t.flag, span})
	case kYear:
		p.c.Year = append(p.c.Year, Claim[int]{t.num, span})
	case kVersion:
		p.c.Version = append(p.c.Version, Claim[int]{t.num, span})
	case kOpen:
		p.languageBracket(i)
	case kLanguage, kLangCode:
		p.languageRun(i)
	case kSubLang:
		p.c.Subtitles = append(p.c.Subtitles, Claim[Language]{t.lang, span})
	case kSubWord:
		p.c.Markers = append(p.c.Markers, Claim[Marker]{Subs, span})
	case kDubWord:
		p.c.Markers = append(p.c.Markers, Claim[Marker]{Dubbed, span})
	case kMarker:
		p.c.Markers = append(p.c.Markers, Claim[Marker]{t.marker, span})
	case kDL:
		p.c.Audio = append(p.c.Audio, Claim[Language]{Original, span})
	}
}

func (p *parser) seasonEpisode(i int) {
	m := reSeasonEpisode.FindStringSubmatch(p.toks[i].text)
	season, _ := strconv.Atoi(m[1])
	first, _ := strconv.Atoi(m[2])
	frac, _ := strconv.Atoi(m[3])
	eps := []Episode{{Number: first, Fraction: frac}}
	for _, e := range strings.FieldsFunc(m[4], func(r rune) bool { return r == 'E' || r == 'e' }) {
		n, _ := strconv.Atoi(e)
		eps = append(eps, Episode{Number: n})
	}
	eps, end, ok := p.extendRange(i, season, eps)
	if !ok {
		return
	}
	if m[5] != "" {
		v, _ := strconv.Atoi(m[5])
		p.c.Version = append(p.c.Version, Claim[int]{v, Span{p.toks[i].start, p.toks[i].end}})
	}
	p.add(season, true, eps, i, end)
	p.episodeTitle(end)
}

// season reads S01, "Season 8", "Staffel 1" or "2nd Season", with the episode
// numbers that may follow it.
func (p *parser) season(i int) {
	t := p.toks[i]
	last := i
	var season int
	switch t.kind {
	case kSeason:
		season, _ = strconv.Atoi(t.text[1:])
	case kSeasonWord:
		if p.next(i, kDecimal) {
			p.used[i+1] = true // "Staffel 1.2": unreadable
			return
		}
		if !p.next(i, kNumber) {
			return
		}
		season, last = p.toks[i+1].num, i+1
	case kOrdinal:
		if !p.next(i, kSeasonWord) {
			return
		}
		season, _ = strconv.Atoi(strings.TrimRight(t.text, "stndrhSTNDRH"))
		last = i + 1
	}
	p.markUsed(i, last)

	j := last + 1
	paren := j < len(p.toks) && p.toks[j].kind == kOpen
	if paren {
		j++
	}
	if j < len(p.toks) && !p.used[j] {
		e := p.toks[j]
		var eps []Episode
		switch {
		case e.kind == kEpisode:
			eps = episodesOf(e.text)
		case (e.kind == kNumber || e.kind == kDecimal || e.kind == kNumberVersion) && (paren || e.dash != noDash):
			eps = []Episode{episodeOf(e)}
		// Fansub style "[Group] Title S3 01 (BDRip ...)".
		case e.kind == kNumber && p.grouped && p.tagFollows(j+1):
			eps = []Episode{episodeOf(e)}
		}
		if eps != nil {
			eps, end, ok := p.extendRange(j, season, eps)
			if !ok {
				return
			}
			p.versionOf(j)
			if paren && end+1 < len(p.toks) && p.toks[end+1].kind == kClose {
				p.used[end+1] = true
			}
			if paren {
				p.used[j-1] = true
			}
			p.add(season, true, eps, i, end)
			p.episodeTitle(end)
			return
		}
	}
	p.add(season, true, nil, i, last)
}

// bare reads episode numbers without a season, from token from up to the
// number at last.
func (p *parser) bare(from, last int, eps []Episode, titled bool) {
	if len(eps) == 0 {
		return
	}
	eps, end, ok := p.extendRange(last, -1, eps)
	if !ok {
		return
	}
	p.add(0, false, eps, from, end)
	if titled {
		p.episodeTitle(end)
	}
}

// packedEpisode reads "101" both as S01E01 and as absolute episode 101. Both
// claims share the span; matching keeps the reading whose episode had aired
// when the release was posted.
func (p *parser) packedEpisode(i int) {
	n := p.toks[i].num
	if season, ep := n/100, n%100; season > 0 && ep > 0 {
		p.add(season, true, []Episode{{Number: ep}}, i, i)
	}
	p.add(0, false, []Episode{{Number: n}}, i, i)
}

// animeEpisodes reads an episode number that follows the title, with an
// optional batch range as in "Title 01 - 04" or "Title - 01 ~ 24".
func (p *parser) animeEpisodes(i int) {
	first := episodeOf(p.toks[i])
	eps, end := []Episode{first}, i
	if j := i + 1; first.Fraction == 0 && j < len(p.toks) && p.toks[j].kind == kNumber && p.toks[j].dash != noDash {
		last := p.toks[j].num
		if last <= first.Number || last-first.Number >= maxEpisodes {
			p.used[j] = true
			return
		}
		eps, end = make([]Episode, 0, last-first.Number+1), j
		for n := first.Number; n <= last; n++ {
			eps = append(eps, Episode{Number: n})
		}
	}
	p.versionOf(i)
	p.add(0, false, eps, i, end)
}

func (p *parser) cross(i int) {
	m := reCross.FindStringSubmatch(p.toks[i].text)
	season, _ := strconv.Atoi(m[1])
	ep, _ := strconv.Atoi(m[2])
	p.add(season, true, []Episode{{Number: ep}}, i, i)
	p.episodeTitle(i)
}

// extendRange turns "S01E158-S01E170", "S01E01-E03" or "25-37" into a range.
// A range that is reversed or too long makes the whole claim unreadable.
func (p *parser) extendRange(i, season int, eps []Episode) ([]Episode, int, bool) {
	j := i + 1
	if len(eps) != 1 || eps[0].Fraction != 0 || j >= len(p.toks) || p.toks[j].dash != gluedDash || p.used[j] {
		return eps, i, true
	}
	t := p.toks[j]
	last := -1
	switch t.kind {
	case kNumber:
		last = t.num
	case kEpisode:
		if e := episodesOf(t.text); len(e) == 1 {
			last = e[0].Number
		}
	case kSeasonEpisode:
		if m := reSeasonEpisode.FindStringSubmatch(t.text); m[3] == "" && m[4] == "" {
			if s, _ := strconv.Atoi(m[1]); s == season {
				last, _ = strconv.Atoi(m[2])
			}
		}
	}
	if last < 0 {
		return eps, i, true
	}
	first := eps[0].Number
	if last <= first || last-first >= maxEpisodes {
		p.used[j] = true
		return nil, j, false
	}
	out := make([]Episode, 0, last-first+1)
	for n := first; n <= last; n++ {
		out = append(out, Episode{Number: n})
	}
	p.used[j] = true
	return out, j, true
}

func (p *parser) add(season int, seasoned bool, eps []Episode, from, to int) {
	p.markUsed(from, to)
	p.c.Numbering = append(p.c.Numbering, Numbering{
		Season: season, Seasoned: seasoned, Episodes: eps,
		Span: Span{p.toks[from].start, p.toks[to].end},
	})
}

// episodeTitle reads the episode title that follows an episode number.
func (p *parser) episodeTitle(after int) {
	if len(p.c.EpisodeTitle) > 0 {
		return
	}
	start := after + 1
	end := start
	for end < len(p.toks) && p.continuesTitle(end, start, true) {
		end++
	}
	if end > start {
		p.c.EpisodeTitle = append(p.c.EpisodeTitle, p.text(start, end))
		p.markUsed(start, end-1)
	}
}

func (p *parser) versionOf(i int) {
	if t := p.toks[i]; t.kind == kNumberVersion {
		p.c.Version = append(p.c.Version, Claim[int]{t.frac, Span{t.start, t.end}})
	}
}

// languageBracket reads lists such as [TR-EN] or [Spanish-English] as audio.
func (p *parser) languageBracket(i int) {
	end := p.closing(i)
	if end <= i+1 {
		return
	}
	for k := i + 1; k < end; k++ {
		if p.toks[k].kind != kLanguage && p.toks[k].kind != kLangCode {
			return
		}
	}
	for k := i + 1; k < end; k++ {
		t := p.toks[k]
		p.c.Audio = append(p.c.Audio, Claim[Language]{t.lang, Span{t.start, t.end}})
	}
	p.markUsed(i, end)
}

// languageRun reads consecutive languages. A following Sub or Subbed makes
// them subtitle languages. Two-letter codes alone are too ambiguous to count.
func (p *parser) languageRun(i int) {
	j, strong := i, false
	for j < len(p.toks) && (j == i || !p.used[j]) && (p.toks[j].kind == kLanguage || p.toks[j].kind == kLangCode) {
		strong = strong || p.toks[j].kind == kLanguage
		j++
	}
	p.markUsed(i, j-1)
	target := &p.c.Audio
	switch {
	case j < len(p.toks) && p.toks[j].kind == kSubWord:
		target = &p.c.Subtitles
		p.used[j] = true
	case j < len(p.toks) && p.toks[j].kind == kDubWord:
		p.used[j] = true
	case !strong:
		return
	}
	for k := i; k < j; k++ {
		t := p.toks[k]
		*target = append(*target, Claim[Language]{t.lang, Span{t.start, t.end}})
	}
}

func (p *parser) next(i int, k kind) bool {
	return i+1 < len(p.toks) && p.toks[i+1].kind == k
}

func (p *parser) closing(i int) int {
	for j := i + 1; j < len(p.toks); j++ {
		if p.toks[j].kind == kClose {
			return j
		}
	}
	return -1
}

func (p *parser) markUsed(from, to int) {
	for k := from; k <= to; k++ {
		p.used[k] = true
	}
}

// text joins tokens as a title, keeping hyphens that were part of words.
func (p *parser) text(from, to int) Claim[string] {
	var b strings.Builder
	for i := from; i < to; i++ {
		t := p.toks[i]
		if i > from {
			switch t.dash {
			case gluedDash:
				b.WriteByte('-')
			case spacedDash:
				b.WriteString(" - ")
			default:
				b.WriteByte(' ')
			}
		}
		b.WriteString(t.text)
	}
	return Claim[string]{b.String(), Span{p.toks[from].start, p.toks[to-1].end}}
}

func episodeOf(t token) Episode {
	if t.kind == kDecimal {
		return Episode{Number: t.num, Fraction: t.frac}
	}
	return Episode{Number: t.num}
}

// episodesOf reads E01, E01+02 or E11.5.
func episodesOf(text string) []Episode {
	if whole, frac, ok := strings.Cut(strings.TrimLeft(text, "Ee"), "."); ok {
		n, err1 := strconv.Atoi(whole)
		f, err2 := strconv.Atoi(frac)
		if err1 != nil || err2 != nil {
			return nil
		}
		return []Episode{{Number: n, Fraction: f}}
	}
	var eps []Episode
	for _, part := range strings.Split(text, "+") {
		n, err := strconv.Atoi(strings.TrimLeft(part, "Ee"))
		if err != nil {
			return nil
		}
		eps = append(eps, Episode{Number: n})
	}
	return eps
}
