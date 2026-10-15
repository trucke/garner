// Package selection judges search results against a show's profile and
// ranks them. Every release gets plain reasons for its verdict.
package selection

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/trucke/garner/internal/match"
	"github.com/trucke/garner/internal/release"
)

// Profile states which releases a show accepts.
type Profile struct {
	Name string
	// Tiers are the acceptable language and resolution pairs, best first.
	Tiers []Tier
	// PreferDual ranks German plus original audio above German alone
	// within a tier.
	PreferDual bool
	// UnnamedFallback allows releases whose audio is marked dual, multi or
	// dubbed without naming languages, as a last resort.
	UnnamedFallback bool
}

// Tier is an acceptable audio language and resolution.
type Tier struct {
	Language   release.Language
	Resolution release.Resolution
}

func (t Tier) String() string { return t.Language.Name() + " " + t.Resolution.String() }

// The built-in profiles. Editable shared profiles come later.
var (
	// TV needs German audio; 720p is accepted until 1080p turns up.
	TV = Profile{Name: "tv", Tiers: []Tier{{"de", 1080}, {"de", 720}}, PreferDual: true}
	// Anime falls back to English audio when no German release exists.
	Anime = Profile{Name: "anime", Tiers: []Tier{{"de", 1080}, {"de", 720}, {"en", 1080}, {"en", 720}}, UnnamedFallback: true}
)

// ProfileNamed returns the profile with that name, TV by default.
func ProfileNamed(name string) Profile {
	if name == Anime.Name {
		return Anime
	}
	return TV
}

// Describe states the profile's tiers in order, such as "German 1080p →
// German 720p · dual audio preferred".
func (p Profile) Describe() string {
	tiers := make([]string, len(p.Tiers))
	for i, t := range p.Tiers {
		tiers[i] = t.String()
	}
	s := strings.Join(tiers, " → ")
	if p.PreferDual {
		s += " · dual audio preferred"
	}
	return s
}

// OriginalNote says what a show's original language means for its
// releases' audio.
func OriginalNote(original release.Language) string {
	if original == "de" {
		return "Releases that name no language are assumed German and checked after download."
	}
	return "German audio only comes from a dub."
}

// Wanted names the profile's audio languages, such as "German or English".
func (p Profile) Wanted() string {
	var names []string
	for _, t := range p.Tiers {
		if n := t.Language.Name(); !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return strings.Join(names, " or ")
}

// Verdict is what Garner does with a release.
type Verdict string

const (
	Accept Verdict = "accept"
	// Fallback releases are used only when nothing is accepted; the file
	// check confirms them after download.
	Fallback Verdict = "fallback"
	Reject   Verdict = "reject"
)

// Context is what judging needs about the show.
type Context struct {
	Profile Profile
	Show    *match.Show
	// Want is the TVDB episode searched for.
	Want int64
	// Original is the show's original language.
	Original release.Language
}

// Release is one search result.
type Release struct {
	Claims release.Claims
	Match  match.Match
	// IndexerLanguages are the audio languages the indexer reports.
	IndexerLanguages []release.Language
	// OtherShow means the indexer files the release under another show.
	OtherShow bool
	Grabs     int
	Published time.Time
}

// Evaluation is the verdict on a release and why.
type Evaluation struct {
	Verdict Verdict
	// Tier is the name of the profile tier the release meets, such as
	// "German 1080p"; empty when it meets none.
	Tier string
	// Audio are the audio languages Garner assumes; empty when unknown.
	Audio []release.Language
	// Reasons explain the verdict, most important first. The first
	// Decisive of them decided it: why the release is rejected or only a
	// last resort. The rest are facts.
	Reasons  []string
	Decisive int

	rank rankKey
}

type rankKey struct {
	verdict   int
	tier      int
	whole     bool // exactly the wanted episode, with certain contents
	assumed   bool // audio assumed from the show's original language
	dual      bool
	source    int
	fixes     int
	grabs     int
	published time.Time
}

// Evaluate judges one release.
func Evaluate(ctx Context, r Release) Evaluation {
	var rejects, fallbacks, info []string
	e := Evaluation{rank: rankKey{tier: len(ctx.Profile.Tiers) + 2, grabs: r.Grabs, published: r.Published}}

	switch r.Match.Kind {
	case match.Unknown:
		rejects = append(rejects, "could not tell which episode it holds: "+r.Match.Reason)
	case match.Extra:
		rejects = append(rejects, r.Match.Reason)
	default:
		if !slices.Contains(r.Match.Episodes, ctx.Want) {
			rejects = append(rejects, fmt.Sprintf("holds %s, not %s", ctx.Show.Describe(r.Match.Episodes), ctx.Show.Label(ctx.Want)))
			break
		}
		switch {
		case r.Match.Kind == match.Season:
			info = append(info, fmt.Sprintf("season pack of %d episodes; it may not hold every one", len(r.Match.Episodes)))
		case len(r.Match.Episodes) > 1:
			info = append(info, "holds "+ctx.Show.Describe(r.Match.Episodes))
		default:
			e.rank.whole = !r.Match.Uncertain
		}
		info = append(info, r.Match.Notes...)
	}
	if r.OtherShow {
		rejects = append(rejects, "the indexer files it under another show")
	}

	if slices.ContainsFunc(r.Claims.Flags, func(f release.Claim[release.Flag]) bool { return f.Value == release.Remux }) {
		rejects = append(rejects, "remux; remuxes are skipped")
	}
	source, _ := release.Value(r.Claims.Source)
	switch source {
	case release.DVD, release.TV:
		rejects = append(rejects, fmt.Sprintf("%s source in standard definition", sourceNames[source]))
	}
	e.rank.source = sourceRank[source]
	res, state := release.Value(r.Claims.Resolution)
	switch {
	case state == release.Conflicting:
		fallbacks = append(fallbacks, "the name states two resolutions; a last resort, checked after download")
	case state == release.Missing:
		fallbacks = append(fallbacks, "resolution not named; a last resort, checked after download")
	case res >= 2160:
		rejects = append(rejects, "2160p is reserved for favourites")
	case res < 720:
		rejects = append(rejects, fmt.Sprintf("%s is below 720p", res))
	}

	audio, why, markers := decideAudio(ctx, r)
	e.Audio = audio
	e.rank.assumed = len(audio) > 0 && len(r.IndexerLanguages) == 0 && len(r.Claims.Audio) == 0
	switch {
	case len(audio) > 0:
		info = append(info, why)
		tier := bestTier(ctx.Profile, audio, res, state == release.Unambiguous)
		if tier < 0 {
			rejects = append(rejects, fmt.Sprintf("audio is %s, not %s", languageList(audio), ctx.Profile.Wanted()))
			break
		}
		e.rank.tier = tier
		if state != release.Unambiguous {
			info = append(info, fmt.Sprintf("%s audio fits; the resolution decides the tier", ctx.Profile.Tiers[tier].Language.Name()))
		} else if res >= 720 && res < 2160 && len(rejects) == 0 {
			e.Tier = ctx.Profile.Tiers[tier].String()
			if tier == 0 {
				info = append(info, "meets "+e.Tier+", the best tier")
			} else {
				info = append(info, fmt.Sprintf("meets %s; %s would be better", e.Tier, ctx.Profile.Tiers[0]))
			}
		}
		if ctx.Original != "de" && slices.Contains(audio, "de") && slices.Contains(audio, ctx.Original) {
			info = append(info, fmt.Sprintf("dual audio: German and %s", ctx.Original.Name()))
			e.rank.dual = ctx.Profile.PreferDual
		}
	case ctx.Profile.UnnamedFallback && markers:
		fallbacks = append(fallbacks, "audio languages not named; a last resort, checked after download")
		e.rank.tier = len(ctx.Profile.Tiers)
		if res < 1080 {
			e.rank.tier++
		}
	default:
		rejects = append(rejects, fmt.Sprintf("no %s audio named", ctx.Profile.Wanted()))
	}

	switch {
	case len(rejects) > 0:
		e.Verdict, e.rank.verdict, e.Reasons, e.Decisive = Reject, 2, append(rejects, info...), len(rejects)
	case len(fallbacks) > 0:
		e.Verdict, e.rank.verdict, e.Reasons, e.Decisive = Fallback, 1, append(fallbacks, info...), len(fallbacks)
	default:
		e.Verdict, e.Reasons = Accept, info
	}
	for _, f := range r.Claims.Flags {
		if f.Value == release.Proper || f.Value == release.Repack {
			e.rank.fixes++
		}
	}
	if v, ok := release.Value(r.Claims.Version); ok == release.Unambiguous {
		e.rank.fixes += v
	}
	return e
}

// decideAudio settles the audio languages from one source: the indexer's
// when it reports any, otherwise the name's, otherwise German for German
// shows. The file check after download catches the rare wrong indexer
// report. why explains the result; markers reports dual, multi or dubbed
// marks.
func decideAudio(ctx Context, r Release) (audio []release.Language, why string, markers bool) {
	for _, m := range r.Claims.Markers {
		switch m.Value {
		case release.DualAudio, release.MultiAudio, release.Dubbed:
			markers = true
		}
	}
	if len(r.IndexerLanguages) > 0 {
		return r.IndexerLanguages, fmt.Sprintf("%s audio, as the indexer reports", languageList(r.IndexerLanguages)), markers
	}
	var named []release.Language
	for _, a := range r.Claims.Audio {
		l := a.Value
		if l == release.Original {
			l = ctx.Original
		}
		if l != "" && !slices.Contains(named, l) {
			named = append(named, l)
		}
	}
	if len(named) > 0 {
		why := fmt.Sprintf("%s audio, as the name says", languageList(named))
		if markers {
			why += ", possibly with others"
		}
		return named, why, markers
	}
	if ctx.Original == "de" {
		return []release.Language{"de"}, "German audio assumed, because the show is German and the name names no language; checked after download", markers
	}
	return nil, "", markers
}

// bestTier is the first tier whose language the audio has and whose
// resolution the release meets. Without a known resolution the language
// alone decides; the release is then a fallback anyway.
func bestTier(p Profile, audio []release.Language, res release.Resolution, known bool) int {
	// Resolutions outside 720p to 1080p are rejected elsewhere; the
	// language still decides whether the audio fits.
	if res >= 1080 {
		res = 1080
	} else {
		res = 720
	}
	for i, t := range p.Tiers {
		if slices.Contains(audio, t.Language) && (!known || t.Resolution == res) {
			return i
		}
	}
	return -1
}

// Rank orders evaluations best first and returns their indexes.
func Rank(evals []Evaluation) []int {
	order := make([]int, len(evals))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		x, y := evals[a].rank, evals[b].rank
		return cmp.Or(
			cmp.Compare(x.verdict, y.verdict),
			cmp.Compare(x.tier, y.tier),
			-compareBool(x.whole, y.whole),
			compareBool(x.assumed, y.assumed),
			-compareBool(x.dual, y.dual),
			-cmp.Compare(x.source, y.source),
			-cmp.Compare(x.fixes, y.fixes),
			-cmp.Compare(x.grabs, y.grabs),
			-x.published.Compare(y.published),
		)
	})
	return order
}

// Summary states the outcome of a search in plain words.
func Summary(p Profile, evals []Evaluation) string {
	accepted, fallback := 0, 0
	for _, e := range evals {
		switch e.Verdict {
		case Accept:
			accepted++
		case Fallback:
			fallback++
		}
	}
	switch {
	case accepted == 1:
		return "Found 1 matching release"
	case accepted > 1:
		return fmt.Sprintf("Found %d matching releases", accepted)
	case fallback > 0:
		return fmt.Sprintf("No matching %s release found; %d last-resort releases", p.Wanted(), fallback)
	}
	return fmt.Sprintf("No matching %s release found", p.Wanted())
}

// IndexerLanguages normalises languages as indexers report them ("German",
// "ja-JP"). Unknown names are kept in lower case, so they still count as a
// language the release has.
func IndexerLanguages(raw []string) []release.Language {
	var out []release.Language
	for _, s := range raw {
		l, ok := release.LanguageNamed(s)
		if !ok {
			l = release.Language(strings.ToLower(strings.TrimSpace(s)))
		}
		if l != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

var sourceRank = map[release.Source]int{
	release.BluRay: 4, release.WebDL: 3, release.WebRip: 2, release.Web: 2, release.HDTV: 1, release.HDRip: 1,
}

var sourceNames = map[release.Source]string{release.DVD: "DVD", release.TV: "TV recording"}

func languageList(ls []release.Language) string {
	names := make([]string, len(ls))
	for i, l := range ls {
		names[i] = l.Name()
	}
	switch len(names) {
	case 0:
		return "none"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}
