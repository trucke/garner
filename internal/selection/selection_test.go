package selection

import (
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

func TestEvaluate(t *testing.T) {
	tests := []struct {
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
		{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DL.1080p.BluRay.x264-GRP", nil, Accept, "German 1080p", "dual audio"},
		{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DL.720p.WEB-GRP", nil, Accept, "German 720p", "German 1080p would be better"},
		{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DL.WEB.x264-GRP", nil, Fallback, "", "resolution not named"},
		{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DVDRip.x264-GRP", nil, Reject, "", "DVD source"},
		{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DL.1080p.BluRay.REMUX-GRP", nil, Reject, "", "remux"},
		{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.1080p.WEB-DL-GRP", nil, Reject, "", "no German audio named"},
		{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.Subbed.1080p.WEB-GRP", nil, Reject, "", "no German audio named"},
		{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.iTALiAN.MULTi.1080p.WEB-GRP", nil, Reject, "", "audio is Italian, not German"},
		{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.2160p.WEB-GRP", nil, Reject, "", "2160p is reserved for favourites"},
		{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.480p.WEB-GRP", nil, Reject, "", "below 720p"},
		{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E02.German.1080p.WEB-GRP", nil, Reject, "", "holds S01E02, not S01E01"},
		{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.1080p.WEB-DL-GRP", []string{"German", "en-US"}, Accept, "German 1080p", "as the indexer reports"},
		// The indexer decides when it reports languages; the name only fills in.
		{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP", []string{"English"}, Reject, "", "audio is English, not German"},
		{TV, "Harbor Precinct", "en", "Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP", []string{"German"}, Accept, "German 1080p", "German audio, as the indexer reports"},
		{TV, "Hafenwache", "de", "Hafenwache.S01E01.1080p.BluRay.x264-GRP", nil, Accept, "German 1080p", "German audio assumed"},

		// Anime
		{Anime, "Lantern Harbor", "ja", "Lantern.Harbor.S01E01.German.720p.WEB-GRP", nil, Accept, "German 720p", ""},
		{Anime, "Lantern Harbor", "ja", "Lantern.Harbor.S01E01.DUBBED.1080p.WEB-GRP", []string{"English"}, Accept, "English 1080p", ""},
		{Anime, "Lantern Harbor", "ja", "Lantern.Harbor.S01E01.1080p.WEB-DL.MULTi-GRP", nil, Fallback, "", "audio languages not named"},
		{Anime, "Lantern Harbor", "ja", "Lantern.Harbor.S01E01.JPN.1080p.WEB-GRP", []string{"English"}, Accept, "English 1080p", "English audio, as the indexer reports"},
		{Anime, "Lantern Harbor", "ja", "Lantern.Harbor.S01E01.1080p.BluRay-GRP", nil, Reject, "", "no German or English audio named"},
		{Anime, "Lantern Harbor", "ja", "Lantern.Harbor.S01.German.1080p.BluRay-GRP", nil, Accept, "German 1080p", "season pack"},
		{Anime, "Lantern Harbor", "ja", "Lantern.Harbor.S01.NCOP.German.1080p.BluRay-GRP", nil, Reject, "", "extra content"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := evaluate(tc.profile, tc.show, tc.original, tc.name, tc.languages...)
			if e.Verdict != tc.verdict || e.Tier != tc.tier || !slices.ContainsFunc(e.Reasons, func(r string) bool { return strings.Contains(r, tc.reason) }) {
				t.Errorf("%s\n got: %s %q %q\nwant: %s %q with %q", tc.name, e.Verdict, e.Tier, e.Reasons, tc.verdict, tc.tier, tc.reason)
			}
		})
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
		"Harbor.Precinct.S01E01.German.DL.1080p.WEB-DL-CC", // like C, more grabs
	}
	var evals []Evaluation
	for _, n := range names {
		e := evaluate(TV, "Harbor Precinct", "en", n)
		if strings.HasSuffix(n, "-CC") {
			e.rank.grabs = 5
		}
		evals = append(evals, e)
	}
	var got []string
	for _, i := range Rank(evals) {
		got = append(got, names[i][strings.LastIndex(names[i], "-")+1:])
	}
	if want := "G CC C B F A D E"; strings.Join(got, " ") != want {
		t.Errorf("order = %s, want %s", strings.Join(got, " "), want)
	}

	// German confirmed by the indexer ranks above German assumed for a German show.
	assumed := evaluate(TV, "Hafenwache", "de", "Hafenwache.S01E01.1080p.BluRay-X")
	confirmed := evaluate(TV, "Hafenwache", "de", "Hafenwache.S01E01.1080p.WEB-DL-Y", "German")
	if order := Rank([]Evaluation{assumed, confirmed}); order[0] != 1 {
		t.Error("assumed German ranked above confirmed German")
	}
}
