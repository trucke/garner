package selection

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/match"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/store"
)

// fixture is a made-up show with three episodes in season 1.
func fixture(name, original string) (*match.Show, int64) {
	d := store.SeriesData{Series: store.Series{ID: 1, Name: name, OriginalLanguage: original}}
	for n := 1; n <= 3; n++ {
		d.Episodes = append(d.Episodes, store.Episode{ID: int64(100 + n), Numbers: map[string]store.Position{"official": {Season: 1, Number: n}}})
	}
	return match.NewShow(d, match.Options{}), 101
}

func evaluate(p Profile, show string, original release.Language, name string, languages ...string) Evaluation {
	s, want := fixture(show, string(original))
	claims := release.Parse(name)
	return Evaluate(Context{Profile: p, Show: s, Want: want, Original: original},
		Release{Claims: claims, Match: s.Match(claims, time.Time{}), IndexerLanguages: IndexerLanguages(languages)})
}

// german1080 accepts German 1080p only.
var german1080 = fromText(`audio = ["de"]
resolution = ["1080p"]`, "")

// english accepts English audio.
var english = fromText(`audio = ["en"]
resolution = ["1080p", "720p"]`, "")

// evaluateTests are made-up release names and the verdicts they must get.
// FuzzEvaluate seeds its corpus from them.
var evaluateTests = []struct {
	profile   Profile
	show      string
	original  release.Language
	name      string
	languages []string
	verdict   Verdict
	tier      string
	reason    string
}{
	// Regular TV
	{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DL.1080p.BluRay.x264-GRP", nil, Accept, "German 1080p", "also has the show's original English audio, which the profile prefers"},
	{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DL.720p.WEB-GRP", nil, Accept, "German 720p", "German 1080p would be better"},
	{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DL.WEB.x264-GRP", nil, Fallback, "", "resolution not named"},
	{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DVDRip.x264-GRP", nil, Reject, "", "DVD source"},
	{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DL.1080p.BluRay.REMUX-GRP", nil, Reject, "", "remux"},
	{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.1080p.WEB-DL-GRP", nil, Reject, "", "no German audio named"},
	{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.Subbed.1080p.WEB-GRP", nil, Reject, "", "no German audio named"},
	{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.iTALiAN.MULTi.1080p.WEB-GRP", nil, Reject, "", "audio is Italian, not German"},
	{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.2160p.WEB-GRP", nil, Reject, "", "Garner does not take 2160p yet"},
	{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.480p.WEB-GRP", nil, Reject, "", "below 720p"},
	{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E02.German.1080p.WEB-GRP", nil, Reject, "", "holds S01E02, not S01E01"},
	{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.1080p.WEB-DL-GRP", []string{"German", "en-US"}, Accept, "German 1080p", "as the indexer reports"},
	// The indexer decides when it reports languages; the name only fills in.
	{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP", []string{"English"}, Reject, "", "audio is English, not German"},
	{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP", []string{"German"}, Accept, "German 1080p", "German audio, as the indexer reports"},
	{TV, "Hafenwache", "de", "Hafenwache.S01E01.1080p.BluRay.x264-GRP", nil, Accept, "German 1080p", "German audio assumed"},

	// A name without languages is in the show's original language.
	{english, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.1080p.WEB-DL-GRP", nil, Accept, "English 1080p", "English audio assumed"},
	{english, "Lantern Harbor", "ja", "Lantern.Harbor.S01E01.1080p.WEB-DL-GRP", nil, Reject, "", "no English audio named"},
	{german1080, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.720p.WEB-GRP", nil, Reject, "", "720p is not among the profile's preferences: German 1080p"},
	{english, "Lantern Harbor", "ja", "Lantern.Harbor.S01E01.1080p.WEB-DL-GRP", []string{"English", "Japanese"}, Accept, "English 1080p", "also has the show's original Japanese audio"},

	// Anime
	{Anime, "Lantern Harbor", "ja", "Lantern.Harbor.S01E01.German.720p.WEB-GRP", nil, Accept, "German 720p", ""},
	{Anime, "Lantern Harbor", "ja", "Lantern.Harbor.S01E01.DUBBED.1080p.WEB-GRP", []string{"English"}, Accept, "English 1080p", ""},
	{Anime, "Lantern Harbor", "ja", "Lantern.Harbor.S01E01.1080p.WEB-DL.MULTi-GRP", nil, Fallback, "", "audio languages not named"},
	{Anime, "Lantern Harbor", "ja", "Lantern.Harbor.S01E01.JPN.1080p.WEB-GRP", []string{"English"}, Accept, "English 1080p", "English audio, as the indexer reports"},
	{Anime, "Lantern Harbor", "ja", "Lantern.Harbor.S01E01.1080p.BluRay-GRP", nil, Reject, "", "no German or English audio named"},
	{Anime, "Lantern Harbor", "ja", "Lantern.Harbor.S01.German.1080p.BluRay-GRP", nil, Accept, "German 1080p", "season pack"},
	{Anime, "Lantern Harbor", "ja", "Lantern.Harbor.S01.NCOP.German.1080p.BluRay-GRP", nil, Reject, "", "extra content"},
}

func TestEvaluate(t *testing.T) {
	for _, tc := range evaluateTests {
		t.Run(tc.name, func(t *testing.T) {
			e := evaluate(tc.profile, tc.show, tc.original, tc.name, tc.languages...)
			if e.Verdict != tc.verdict || e.Tier != tc.tier || !slices.ContainsFunc(e.Reasons, func(r string) bool { return strings.Contains(r, tc.reason) }) {
				t.Errorf("%s\n got: %s %q %q\nwant: %s %q with %q", tc.name, e.Verdict, e.Tier, e.Reasons, tc.verdict, tc.tier, tc.reason)
			}
		})
	}
}

// TestDualAudioReason names the show's original audio of a dual-audio
// release in the audio reason, and once more only when the profile prefers
// such releases (GRR-48).
func TestDualAudioReason(t *testing.T) {
	for _, tc := range []struct {
		dual string
		want int
	}{{"prefer", 2}, {"ignore", 1}} {
		p := fromText(`audio = ["de"]
resolution = ["1080p"]
dual_audio = "`+tc.dual+`"`, "en")
		e := evaluate(p, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP")
		if n := strings.Count(strings.Join(e.Reasons, "\n"), "English"); e.Verdict != Accept || n != tc.want {
			t.Errorf("dual_audio %s: %s, English named %d times in %q, want %d", tc.dual, e.Verdict, n, e.Reasons, tc.want)
		}
	}
}

// TestEvaluateDegrades checks the production path, which tests and
// fuzzers never take: when an invariant breaks outside go test, Evaluate
// rejects the release with a reason that says Garner could not judge it
// and logs the invariant and the name instead of crashing the server. The
// rejected release must also rank like any other rejection, not first.
func TestEvaluateDegrades(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	s, want := fixture("Harbor Precinct", "en")
	ctx := Context{Profile: TV, Show: s, Want: want, Original: "en"}
	r := Release{Claims: release.Parse("Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP")}
	got := degrade(ctx, r, func() Evaluation {
		panic("invariant broken: a test panic")
	})
	if got.Verdict != Reject || len(got.Reasons) != 1 || !strings.Contains(got.Reasons[0], "could not judge") || got.Decisive != 0 {
		t.Errorf("degrade yielded %s %q decisive %d, want a rejection that says Garner could not judge", got.Verdict, got.Reasons, got.Decisive)
	}
	if got.rank.verdict != 2 {
		t.Errorf("a degraded rejection ranks like a verdict of %d, want a rejection's rank", got.rank.verdict)
	}
	if log := buf.String(); !strings.Contains(log, "a test panic") || !strings.Contains(log, r.Claims.Name) {
		t.Errorf("the log does not name the invariant and the input: %s", log)
	}
}

func TestRank(t *testing.T) {
	names := []string{
		"Harbor.Precinct.S01E01.German.DL.720p.BluRay-A",   // lower tier
		"Harbor.Precinct.S01E01.German.1080p.BluRay-B",     // German only
		"Harbor.Precinct.S01E01.German.DL.1080p.WEB-DL-C",  // dual, WEB
		"Harbor.Precinct.S01E01.German.DL.WEB-D",           // fallback
		"Harbor.Precinct.S01E01.1080p.WEB-E",               // rejected
		"Harbor.Precinct.S01.German.DL.1080p.BluRay-F",     // season pack
		"Harbor.Precinct.S01E01.German.DL.1080p.BluRay-G",  // dual, Blu-ray
		"Harbor.Precinct.S01E01.German.DL.1080p.WEB-DL-CC", // like C, newer
		"Harbor.Precinct.S01E01.German.DL.1080p.BluRay-L",  // like G, far too large
	}
	s, want := fixture("Harbor Precinct", "en")
	ctx := Context{Profile: TV, Show: s, Want: want, Original: "en"}
	var evals []Evaluation
	for _, n := range names {
		claims := release.Parse(n)
		r := Release{Claims: claims, Match: s.Match(claims, time.Time{})}
		switch {
		case strings.HasSuffix(n, "-CC"):
			r.Published = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		case strings.HasSuffix(n, "-C"):
			r.Published = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		case strings.HasSuffix(n, "-L"):
			// 7.6 GB for 24 minutes: over 300 MB per minute.
			r.Size, r.Runtime = 7.6e9, 24*time.Minute
		case strings.HasSuffix(n, "-G"):
			r.Size, r.Runtime = 2.2e9, 24*time.Minute
		}
		evals = append(evals, Evaluate(ctx, r))
	}
	var got []string
	for _, i := range Rank(evals) {
		got = append(got, names[i][strings.LastIndex(names[i], "-")+1:])
	}
	// Web before Blu-ray: dual audio WEB-DL (CC, C) before dual Blu-ray (G).
	if want := "CC C G B F L A D E"; strings.Join(got, " ") != want {
		t.Errorf("order = %s, want %s", strings.Join(got, " "), want)
	}
	large := evals[len(evals)-1]
	if large.Verdict != Accept || !slices.ContainsFunc(large.Reasons, func(r string) bool { return strings.HasPrefix(r, "large for 1080p: 316 MB per minute") }) {
		t.Errorf("large release: %s %q", large.Verdict, large.Reasons)
	}

	// German confirmed by the indexer ranks above German assumed for a German show.
	assumed := evaluate(TV, "Hafenwache", "de", "Hafenwache.S01E01.1080p.BluRay-X")
	confirmed := evaluate(TV, "Hafenwache", "de", "Hafenwache.S01E01.1080p.WEB-DL-Y", "German")
	if order := Rank([]Evaluation{assumed, confirmed}); order[0] != 1 {
		t.Error("assumed German ranked above confirmed German")
	}
}

// TestRankPack puts a season pack with every missing episode before single
// episodes of its tier, and after them when it would bring one episode.
func TestRankPack(t *testing.T) {
	s, want := fixture("Harbor Precinct", "en")
	pack := release.Parse("Harbor.Precinct.S01.German.DL.1080p.BluRay-P")
	single := release.Parse("Harbor.Precinct.S01E01.German.DL.1080p.BluRay-S")
	for _, tc := range []struct {
		missing []int64
		first   string
	}{
		{[]int64{101, 102, 103}, "P"},
		{[]int64{101}, "S"},
	} {
		ctx := Context{Profile: TV, Show: s, Want: want, Original: "en", Missing: tc.missing}
		evals := []Evaluation{
			Evaluate(ctx, Release{Claims: single, Match: s.Match(single, time.Time{})}),
			Evaluate(ctx, Release{Claims: pack, Match: s.Match(pack, time.Time{})}),
		}
		if got := []string{"S", "P"}[Rank(evals)[0]]; got != tc.first {
			t.Errorf("%d missing: %s first, want %s (%q)", len(tc.missing), got, tc.first, evals[1].Reasons)
		}
	}
}

// TestUpgrade accepts, for an episode with a file, only releases in a
// better tier than the file's: the same tier from a better source does
// not replace it.
func TestUpgrade(t *testing.T) {
	s, want := fixture("Harbor Precinct", "en")
	judge := func(have, name string) Evaluation {
		claims := release.Parse(name)
		return Evaluate(Context{Profile: TV, Show: s, Want: want, Original: "en", Have: have},
			Release{Claims: claims, Match: s.Match(claims, time.Time{})})
	}
	for _, tc := range []struct {
		have, name string
		verdict    Verdict
	}{
		{"German 720p", "Harbor.Precinct.S01E01.German.DL.1080p.WEB-DL-A", Accept},
		{"German 720p", "Harbor.Precinct.S01E01.German.DL.720p.BluRay-B", Reject},
		{"German 1080p", "Harbor.Precinct.S01E01.German.DL.1080p.WEB-DL-C", Reject},
		{"German 720p", "Harbor.Precinct.S01E01.German.DL.WEB-D", Fallback}, // resolution unknown: it may be 1080p
		{"German 1080p", "Harbor.Precinct.S01E01.German.DL.WEB-D", Reject},  // nothing beats the best tier
		{"", "Harbor.Precinct.S01E01.German.DL.720p.BluRay-E", Accept},      // no file, or one of unknown tier
	} {
		e := judge(tc.have, tc.name)
		if e.Verdict != tc.verdict {
			t.Errorf("%s over %q: %s %q, want %s", tc.name, tc.have, e.Verdict, e.Reasons, tc.verdict)
		}
		if e.Verdict == Reject && e.Reasons[0] != "the library has "+tc.have+" already; only a release that meets a better preference replaces it" {
			t.Errorf("%s: reasons %q", tc.name, e.Reasons)
		}
	}
	if got := Summary(TV, "German 1080p", []Evaluation{judge("German 1080p", "Harbor.Precinct.S01E01.German.DL.1080p.WEB-DL-C")}); got != "No release better than German 1080p found" {
		t.Errorf("summary %q", got)
	}
}

func TestFileTier(t *testing.T) {
	for _, tc := range []struct {
		profile Profile
		audio   []release.Language
		res     release.Resolution
		want    string
	}{
		{TV, []release.Language{"en", "de"}, 1080, "German 1080p"},
		{TV, []release.Language{"de"}, 720, "German 720p"},
		{TV, []release.Language{"de"}, 2160, "German 1080p"}, // more than the best tier
		{TV, []release.Language{"de"}, 480, "German 480p"},   // below every tier
		{TV, []release.Language{"en"}, 1080, "English 1080p"},
		{Anime, []release.Language{"ja", "en"}, 1080, "English 1080p"},
		{TV, nil, 1080, ""},
	} {
		got := FileTier(tc.profile, tc.audio, tc.res)
		if got != tc.want {
			t.Errorf("%s %v %dp: %q, want %q", tc.profile.Name, tc.audio, tc.res, got, tc.want)
		}
		if got != "" && got != tc.profile.Tiers[0].String() && TierRank(tc.profile, got) == 0 {
			t.Errorf("%q ranks as the best tier", got)
		}
	}
}

// TestFromProfile applies stored profiles to shows: "original" becomes the
// show's language, a repeated tier goes, and the settings carry over.
func TestFromProfile(t *testing.T) {
	for _, tc := range []struct {
		name, text, original string
		want                 string
	}{
		{"original", `audio = ["original", "en"]
resolution = ["1080p", "720p"]`, "jpn", "Japanese 1080p → Japanese 720p → English 1080p → English 720p · dual audio preferred"},
		{"original repeats a language", `audio = ["original", "en"]
resolution = ["1080p", "720p"]`, "eng", "English 1080p → English 720p · dual audio preferred"},
		{"by resolution", `audio = ["de", "en"]
resolution = ["1080p", "720p"]
priority = "resolution"
dual_audio = "ignore"`, "deu", "German 1080p → English 1080p → German 720p → English 720p"},
		{"language Garner does not know", `audio = ["original"]
resolution = ["1080p"]`, "yue", "yue 1080p · dual audio preferred"},
		{"no original language", `audio = ["original", "de"]
resolution = ["1080p"]`, "", "German 1080p · dual audio preferred"},
		{"nothing left", `audio = ["original"]
resolution = ["1080p"]`, "", "nothing: the profile wants the show's original language, which TVDB does not name"},
	} {
		p := fromText(tc.text, tc.original)
		if got := p.Describe(); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		// Preferences is the same without the settings.
		if want, _, _ := strings.Cut(tc.want, " · "); p.Preferences() != want {
			t.Errorf("%s: preferences %q, want %q", tc.name, p.Preferences(), want)
		}
	}
	p := fromText(`audio = ["de", "en"]
resolution = ["720p"]
dual_audio = "ignore"
unnamed_audio = "fallback"
upgrades = false`, "")
	if p.PreferDual || !p.UnnamedFallback || p.Upgrades || p.Best() != "German 720p" {
		t.Errorf("settings: %+v, best %q", p, p.Best())
	}
	if got := fromText(`audio = ["original"]
resolution = ["1080p"]`, "").Best(); got != "" {
		t.Errorf("a profile that accepts nothing has the best tier %q", got)
	}
}

// TestNothingAccepted judges a release for a show whose profile accepts
// nothing: it is rejected with the reason, not by an empty language list.
func TestNothingAccepted(t *testing.T) {
	p := fromText(`audio = ["original"]
resolution = ["1080p"]`, "")
	e := evaluate(p, "Harbor Precinct", "", "Harbor.Precinct.S01E01.1080p.WEB-DL-GRP")
	if e.Verdict != Reject || !slices.ContainsFunc(e.Reasons, func(r string) bool { return strings.Contains(r, "TVDB does not name") }) {
		t.Errorf("%s %q", e.Verdict, e.Reasons)
	}
}

func TestOriginalNote(t *testing.T) {
	for _, tc := range []struct {
		p        Profile
		original release.Language
		want     string
	}{
		{TV, "de", "Releases that name no language are assumed German and checked after download."},
		{TV, "en", "German audio only comes from a dub."},
		{english, "en", "Releases that name no language are assumed English and checked after download."},
		{Anime, "ja", "German or English audio only comes from a dub."},
	} {
		if got := OriginalNote(tc.p, tc.original); got != tc.want {
			t.Errorf("%s, %s: %q", tc.p.Describe(), tc.original, got)
		}
	}
}
