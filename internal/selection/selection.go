// Package selection judges search results against a show's profile and
// ranks them. Every release gets plain reasons for its verdict.
package selection

import (
	"cmp"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/match"
	"github.com/trucke/garner/internal/profile"
	"github.com/trucke/garner/internal/release"
)

// Profile states which releases a show accepts: a stored profile applied
// to the show, as FromProfile makes it.
type Profile struct {
	Name string
	// Tiers are the acceptable language and resolution pairs, best first:
	// the profile's preferences in the show's languages.
	Tiers []Tier
	// PreferDual ranks the tier's language plus the show's original audio
	// above the tier's language alone.
	PreferDual bool
	// UnnamedFallback allows releases whose audio is marked dual, multi or
	// dubbed without naming languages, as a last resort.
	UnnamedFallback bool
	// Upgrades has the monitor replace a file below the first tier when a
	// release in a better one appears.
	Upgrades bool
}

// Tier is an acceptable audio language and resolution.
type Tier struct {
	Language   release.Language
	Resolution release.Resolution
}

func (t Tier) String() string { return t.Language.Name() + " " + t.Resolution.String() }

// FromProfile applies a stored profile to a show whose original language
// is original: "original" becomes that language, and a tier that repeats
// an earlier one goes. Without an original language, the profile's
// "original" preferences go too, and a profile of only those accepts
// nothing.
func FromProfile(p profile.Profile, original release.Language) Profile {
	out := Profile{
		Name:            p.Name,
		PreferDual:      p.DualAudio == profile.PreferDual,
		UnnamedFallback: p.UnnamedAudio == profile.FallbackUnnamed,
		Upgrades:        p.Upgrades,
	}
	for _, pref := range p.Preferences() {
		l := release.Language(pref.Audio)
		if pref.Audio == profile.Original {
			if original == "" {
				continue
			}
			l = original
		}
		t := Tier{Language: l, Resolution: release.Resolution(pref.Resolution)}
		if !slices.Contains(out.Tiers, t) {
			out.Tiers = append(out.Tiers, t)
		}
	}
	return out
}

// OriginalLanguage is a show's original language as TVDB names it, such as
// "jpn". A language Garner does not know keeps TVDB's code, so releases
// that name no language still count as the show's own.
func OriginalLanguage(tvdb string) release.Language {
	if l, ok := release.LanguageNamed(tvdb); ok {
		return l
	}
	return release.Language(strings.ToLower(strings.TrimSpace(tvdb)))
}

// Best names the first tier, such as "German 1080p": a file in it is not
// upgraded. It is empty for a profile that accepts nothing.
func (p Profile) Best() string {
	if len(p.Tiers) == 0 {
		return ""
	}
	return p.Tiers[0].String()
}

// Has reports whether one of the profile's tiers is in that language.
func (p Profile) Has(l release.Language) bool {
	return slices.ContainsFunc(p.Tiers, func(t Tier) bool { return t.Language == l })
}

// Describe states the profile's tiers in order, such as "German 1080p →
// German 720p · dual audio preferred".
func (p Profile) Describe() string {
	if len(p.Tiers) == 0 {
		return "nothing: the profile wants the show's original language, which TVDB does not name"
	}
	s := p.tierList()
	if p.PreferDual {
		s += " · dual audio preferred"
	}
	return s
}

// Preferences states the profile's tiers in order, such as "German 1080p
// → German 720p", without the settings Describe adds.
func (p Profile) Preferences() string {
	if len(p.Tiers) == 0 {
		return p.Describe()
	}
	return p.tierList()
}

// tierList names the tiers in order, such as "German 1080p → German 720p".
func (p Profile) tierList() string {
	tiers := make([]string, len(p.Tiers))
	for i, t := range p.Tiers {
		tiers[i] = t.String()
	}
	return strings.Join(tiers, " → ")
}

// OriginalNote says what a show's original language means for its
// releases' audio under the profile.
func OriginalNote(p Profile, original release.Language) string {
	if original != "" && p.Has(original) {
		return fmt.Sprintf("Releases that name no language are assumed %s and checked after download.", original.Name())
	}
	return p.Wanted() + " audio only comes from a dub."
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
	// Missing are the aired episodes of Want's season without a file. A
	// pack with all of them, when there are several, ranks before single
	// episodes of its tier.
	Missing []int64
	// Have is the tier of Want's file in the library, such as "German
	// 720p"; empty when it has none or the tier is unknown. Only a release
	// in a better tier is accepted then: a better source alone does not
	// replace a file.
	Have string
}

// Release is one search result.
type Release struct {
	Claims release.Claims
	Match  match.Match
	// IndexerLanguages are the audio languages the indexer reports.
	IndexerLanguages []release.Language
	// OtherShow means the indexer files the release under another show.
	OtherShow bool
	Published time.Time
	// Size is the release's size in bytes; Runtime is how long the episodes
	// it holds run. Either may be 0 when unknown.
	Size    int64
	Runtime time.Duration
}

// largeAt is the size per minute of runtime, in MB, above which a release
// ranks behind the others of its tier. Encodes run at about 40 to 100 MB
// per minute in 1080p and half that in 720p; well above it is remux
// territory or an extreme encode.
var largeAt = map[release.Resolution]int64{1080: 150, 720: 75}

// Evaluation is the verdict on a release and why.
type Evaluation struct {
	Verdict Verdict
	// Tier is the name of the profile tier the release meets, such as
	// "German 1080p"; empty when it meets none.
	Tier string
	// Audio are the audio languages Garner assumes; empty when unknown.
	Audio []release.Language
	// Reasons explain the verdict, most important first. The first
	// Decisive of them decided it: why the release is rejected, only a
	// last resort, or accepted. The rest are facts, such as caveats.
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
	large     bool // far larger than its resolution needs
	complete  bool // a pack with every missing episode of its season
	published time.Time
}

// Evaluate judges one release.
func Evaluate(ctx Context, r Release) Evaluation {
	if testing.Testing() {
		return judge(ctx, r)
	}
	// In the running server a broken invariant must not take garnerd down:
	// the monitor would search the same episode again after a restart and
	// crash in a loop. Tests and fuzzers see the panic instead, so the bug
	// is found.
	return degrade(ctx, r, func() Evaluation { return judge(ctx, r) })
}

// degrade runs build and, when an invariant breaks, logs it together with
// the name and falls back to a rejection that says Garner could not judge
// the release.
func degrade(ctx Context, r Release, build func() Evaluation) (e Evaluation) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Default().Error("selection: rejecting the release after a broken invariant",
				"invariant", rec, "name", r.Claims.Name)
			e = Evaluation{
				Verdict: Reject,
				Reasons: []string{"Garner could not judge the release: an internal check failed"},
				rank:    rankKey{verdict: 2, tier: len(ctx.Profile.Tiers) + 2, published: r.Published},
			}
		}
	}()
	return build()
}

func judge(ctx Context, r Release) Evaluation {
	// info supports the verdict; notes are caveats and other facts.
	var rejects, fallbacks, info, notes []string
	e := Evaluation{rank: rankKey{tier: len(ctx.Profile.Tiers) + 2, published: r.Published}}

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
			notes = append(notes, fmt.Sprintf("season pack of %d episodes; it may not hold every one", len(r.Match.Episodes)))
		case len(r.Match.Episodes) > 1:
			notes = append(notes, "holds "+ctx.Show.Describe(r.Match.Episodes))
		default:
			e.rank.whole = !r.Match.Uncertain
		}
		notes = append(notes, r.Match.Notes...)
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
	case len(ctx.Profile.Tiers) == 0:
		rejects = append(rejects, "the profile wants the show's original language, which TVDB does not name")
	case len(audio) > 0:
		info = append(info, why)
		tier := bestTier(ctx.Profile, audio, res, state == release.Unambiguous)
		switch {
		case tier < 0 && slices.ContainsFunc(audio, ctx.Profile.Has):
			// The language fits, but no tier in it has this resolution.
			rejects = append(rejects, fmt.Sprintf("%s is not among the profile's tiers: %s", res, ctx.Profile.tierList()))
		case tier < 0 && e.rank.assumed:
			rejects = append(rejects, fmt.Sprintf("no %s audio named", ctx.Profile.Wanted()))
		case tier < 0:
			rejects = append(rejects, fmt.Sprintf("audio is %s, not %s", languageList(audio), ctx.Profile.Wanted()))
		}
		if tier < 0 {
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
		if l := ctx.Profile.Tiers[tier].Language; l != ctx.Original && slices.Contains(audio, ctx.Original) {
			info = append(info, fmt.Sprintf("dual audio: %s and %s", l.Name(), ctx.Original.Name()))
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

	if limit, ok := largeAt[res]; ok && state == release.Unambiguous && len(rejects) == 0 && r.Size > 0 && r.Runtime >= time.Minute {
		if perMinute := r.Size / 1e6 / int64(r.Runtime/time.Minute); perMinute > limit {
			e.rank.large = true
			notes = append(notes, fmt.Sprintf("large for %s: %d MB per minute, more than %d; listed after smaller releases of its tier", res, perMinute, limit))
		}
	}

	if len(ctx.Missing) > 1 && len(r.Match.Episodes) > 1 && len(rejects) == 0 &&
		!slices.ContainsFunc(ctx.Missing, func(id int64) bool { return !slices.Contains(r.Match.Episodes, id) }) {
		e.rank.complete = true
		notes = append(notes, "holds every missing episode of the season")
	}

	// A last resort's tier is the best its audio allows, so it is rejected
	// only when even that is not better.
	if ctx.Have != "" && len(rejects) == 0 && e.rank.tier >= TierRank(ctx.Profile, ctx.Have) {
		rejects = append(rejects, fmt.Sprintf("the library has %s already; only a better tier replaces it", ctx.Have))
	}

	switch {
	case len(rejects) > 0:
		e.Verdict, e.rank.verdict, e.Reasons, e.Decisive = Reject, 2, slices.Concat(rejects, info, notes), len(rejects)
	case len(fallbacks) > 0:
		e.Verdict, e.rank.verdict, e.Reasons, e.Decisive = Fallback, 1, slices.Concat(fallbacks, info, notes), len(fallbacks)
	default:
		e.Verdict, e.Reasons, e.Decisive = Accept, slices.Concat(info, notes), len(info)
	}
	for _, f := range r.Claims.Flags {
		if f.Value == release.Proper || f.Value == release.Repack {
			e.rank.fixes++
		}
	}
	if v, ok := release.Value(r.Claims.Version); ok == release.Unambiguous {
		e.rank.fixes += v
	}
	e.verify(ctx)
	return e
}

// verify panics when an evaluation breaks an invariant: it has at least
// one reason, Decisive ends the leading decisive reasons, so it lies
// between 1 and the number of reasons, and an accepted release meets a
// tier of the profile.
func (e *Evaluation) verify(ctx Context) {
	if len(e.Reasons) == 0 {
		panic("invariant broken: an evaluation has at least one reason, but this one has none")
	}
	if e.Decisive <= 0 || e.Decisive > len(e.Reasons) {
		panic(fmt.Sprintf("invariant broken: Decisive ends the decisive reasons, but %d lies outside 1 to %d", e.Decisive, len(e.Reasons)))
	}
	if e.Verdict == Accept && (e.Tier == "" || TierRank(ctx.Profile, e.Tier) >= len(ctx.Profile.Tiers)) {
		panic(fmt.Sprintf("invariant broken: an accepted release meets a tier of the profile, but %q meets none", e.Tier))
	}
}

// decideAudio settles the audio languages from one source: the indexer's
// when it reports any, otherwise the name's, otherwise the show's original
// language, as names leave it unmarked. The file check after download
// catches a wrong guess. why explains the result; markers reports dual,
// multi or dubbed marks.
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
	// Marks without languages say there is more than the original audio;
	// when the profile wants another language, the audio stays unknown and
	// the release can only be a last resort.
	if ctx.Original == "" || markers && !ctx.Profile.Has(ctx.Original) {
		return nil, "", markers
	}
	name := ctx.Original.Name()
	return []release.Language{ctx.Original}, fmt.Sprintf("%s audio assumed, because the show is %s and the name names no language; checked after download", name, name), markers
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
	compare := func(a, b int) int {
		x, y := evals[a].rank, evals[b].rank
		return cmp.Or(
			cmp.Compare(x.verdict, y.verdict),
			cmp.Compare(x.tier, y.tier),
			compareBool(x.large, y.large),
			-compareBool(x.complete, y.complete),
			-compareBool(x.whole, y.whole),
			compareBool(x.assumed, y.assumed),
			-compareBool(x.dual, y.dual),
			-cmp.Compare(x.source, y.source),
			-cmp.Compare(x.fixes, y.fixes),
			-x.published.Compare(y.published),
		)
	}
	slices.SortStableFunc(order, compare)
	verifyRank(evals, order, compare)
	return order
}

// verifyRank panics when the ranking breaks an invariant: the order is a
// permutation of the indexes and sorted by the comparator, so it is a
// consistent order. The comparator ranks by fields in lexicographic order,
// which cannot disagree for any input; the check keeps future edits to it
// honest.
func verifyRank(evals []Evaluation, order []int, compare func(a, b int) int) {
	seen := make([]bool, len(evals))
	for k, i := range order {
		if i < 0 || i >= len(evals) || seen[i] {
			panic("invariant broken: Rank returns every index exactly once")
		}
		seen[i] = true
		if k > 0 && compare(order[k-1], i) > 0 {
			panic("invariant broken: Rank's order is consistent with its comparator")
		}
	}
}

// Summary states the outcome of a search in plain words. have is the tier
// of the file in the library, as in Context.
func Summary(p Profile, have string, evals []Evaluation) string {
	return summary(p, have, evals, "release")
}

// PackSummary states the outcome of a season search, which keeps packs.
func PackSummary(p Profile, evals []Evaluation) string { return summary(p, "", evals, "season pack") }

func summary(p Profile, have string, evals []Evaluation, noun string) string {
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
		return "Found 1 matching " + noun
	case accepted > 1:
		return fmt.Sprintf("Found %d matching %ss", accepted, noun)
	case have != "" && fallback > 0:
		return fmt.Sprintf("No %s better than %s found; %d last-resort %ss", noun, have, fallback, noun)
	case have != "":
		return fmt.Sprintf("No %s better than %s found", noun, have)
	case fallback > 0:
		return fmt.Sprintf("No matching %s %s found; %d last-resort %ss", p.Wanted(), noun, fallback, noun)
	}
	return fmt.Sprintf("No matching %s %s found", p.Wanted(), noun)
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

// sourceRank orders releases of a tier by source. Web releases go before
// Blu-ray encodes: Jellyfin usually streams them without converting.
var sourceRank = map[release.Source]int{
	release.WebDL: 4, release.WebRip: 3, release.Web: 3, release.BluRay: 2, release.HDTV: 1, release.HDRip: 1,
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

// FileTier names the tier of a file with these audio languages and this
// resolution, such as "German 720p": the best profile tier it meets or,
// when it meets none, its own language and resolution, which rank below
// every tier, such as "English 1080p" under a German profile. A 2160p file
// meets the 1080p tier of its language, so no upgrade replaces it with
// less. It is empty when the audio languages are unknown.
func FileTier(p Profile, audio []release.Language, res release.Resolution) string {
	if len(audio) == 0 {
		return ""
	}
	if res >= 720 {
		if i := bestTier(p, audio, res, true); i >= 0 {
			return p.Tiers[i].String()
		}
	}
	// The profile's languages first, so a German file in 480p reads as such.
	lang := audio[0]
	for _, t := range p.Tiers {
		if slices.Contains(audio, t.Language) {
			lang = t.Language
			break
		}
	}
	return Tier{lang, res}.String()
}

// TierRank places a tier name such as "German 1080p" in the profile: 0 is
// the best tier; an unknown or empty one ranks after every tier.
func TierRank(p Profile, tier string) int {
	if i := slices.IndexFunc(p.Tiers, func(t Tier) bool { return t.String() == tier }); i >= 0 {
		return i
	}
	return len(p.Tiers)
}
