package match

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/store"
)

func TestWords(t *testing.T) {
	for in, want := range map[string]string{
		"Glass Orchard: After Winter's End": "glass orchard after winters end",
		"Rogue!! Der Herr der Zerstörung":   "rogue der herr der zerstoerung",
		"Straße":                            "strasse",
		"Kōri no Mīnato":                    "kori no minato",
		"Copper Valley II":                  "copper valley 2",
		"LAMP x HARBOR":                     "lamp x harbor",
		"Lanterns & Tides":                  "lanterns and tides",
		"Harbor Twenty-Two":                 "harbor twenty two",
		"Night Ferryː Crossing":             "night ferry crossing",
	} {
		if got := strings.Join(Words(in), " "); got != want {
			t.Errorf("Words(%q) = %q, want %q", in, got, want)
		}
	}
}

// lanternHarbor is a made-up anime: 23 episodes in season 1 with a
// three-episode premiere on one day, 24 in season 2, six specials, and a
// Netflix order whose season 2 holds four of the specials.
func lanternHarbor() store.SeriesData {
	d := store.SeriesData{
		Series: store.Series{ID: 1, Name: "Tourou no Minato", NameEng: "Lantern Harbor", NameDeu: "Laternenhafen"},
		Orders: []store.Order{{Type: "official", Name: "Aired Order"}, {Type: "alternate", Name: "Alternate", AlternateName: "Netflix"}},
		Titles: []store.Title{
			{Title: "Tōrō no Minato", Source: "tvdb"},
			{Title: "Lanterns & Tides", Source: "tvdb"},
			{Title: "Tourou no Minato Ni no Shou", Source: "xem", SceneSeason: ptr(2), Season: ptr(2)},
			{Title: "Lantern Harbor II - Dawn", Source: "garner", Season: ptr(2)},
			// TheXEM lists the plain show title under season 1.
			{Title: "Lantern Harbor", Source: "xem", SceneSeason: ptr(1), Season: ptr(1)},
		},
		Scene: []store.SceneNumber{
			{Scene: store.Position{Season: 2, Number: 25}, TVDB: store.Position{Season: 2, Number: 25}},
			{Scene: store.Position{Season: 3, Number: 1}, TVDB: store.Position{Season: 2, Number: 13}, EpisodeID: ptr64(2013)},
		},
	}
	names := map[store.Position][2]string{
		{Season: 1, Number: 4}:  {"The Quiet Pier", "Der stille Steg"},
		{Season: 1, Number: 5}:  {"Rooftops", "Über den Dächern"},
		{Season: 1, Number: 8}:  {"Let's Go", "Gehen wir"},
		{Season: 1, Number: 11}: {"Here We Go", "Gehen wir"},
		{Season: 0, Number: 2}:  {"Recollection of the Fallen", "Erinnerung an die Gefallenen"},
	}
	premiere := time.Date(2024, 4, 5, 0, 0, 0, 0, time.UTC)
	add := func(season, number int, aired time.Time) *store.Episode {
		p := store.Position{Season: season, Number: number}
		n, ok := names[p]
		if !ok {
			n = [2]string{fmt.Sprintf("Chapter %d-%d", season, number), fmt.Sprintf("Kapitel %d-%d", season, number)}
		}
		d.Episodes = append(d.Episodes, store.Episode{
			ID: int64(season*1000 + number), Aired: aired.Format(time.DateOnly), NameEng: n[0], NameDeu: n[1],
			Numbers: map[string]store.Position{"official": p},
		})
		return &d.Episodes[len(d.Episodes)-1]
	}
	for n := 1; n <= 23; n++ {
		add(1, n, premiere.AddDate(0, 0, 7*max(0, n-3))).Numbers["alternate"] = store.Position{Season: 1, Number: n}
	}
	for n := 1; n <= 24; n++ {
		add(2, n, premiere.AddDate(1, 0, 7*n)).Numbers["alternate"] = store.Position{Season: 3, Number: n}
	}
	for n := 1; n <= 6; n++ {
		e := add(0, n, premiere.AddDate(0, 6, n))
		if n >= 3 {
			e.Numbers["alternate"] = store.Position{Season: 2, Number: n - 2}
		}
	}
	return d
}

// harborPrecinct is a made-up regular show: five seasons of 22 weekly
// episodes from 2013, so episode 101 overall is S05E13 and aired in 2017.
func harborPrecinct() store.SeriesData {
	d := store.SeriesData{Series: store.Series{ID: 2, Name: "Harbor Precinct", NameDeu: "Hafenrevier"}}
	start := time.Date(2013, 9, 17, 0, 0, 0, 0, time.UTC)
	for s := 1; s <= 5; s++ {
		for n := 1; n <= 22; n++ {
			deu := map[int]string{3: "Nicht auf meinem Sofa", 4: "Geschmackssache"}[n]
			if s > 1 {
				deu = ""
			}
			d.Episodes = append(d.Episodes, store.Episode{
				ID: int64(s*1000 + n), Aired: start.AddDate(s-1, 0, 7*(n-1)).Format(time.DateOnly), NameDeu: deu,
				Numbers: map[string]store.Position{"official": {Season: s, Number: n}},
			})
		}
	}
	return d
}

// precinct100 is a made-up show whose title ends in a number.
func precinct100() store.SeriesData {
	d := store.SeriesData{Series: store.Series{ID: 3, Name: "Precinct 100"}}
	for n := 1; n <= 10; n++ {
		d.Episodes = append(d.Episodes, store.Episode{ID: int64(1000 + n), Numbers: map[string]store.Position{"official": {Season: 1, Number: n}}})
	}
	return d
}

func TestMatch(t *testing.T) {
	anime := Options{Anime: true}
	netflixRule := Options{Anime: true, ServiceOrders: map[release.Service]string{"netflix": "alternate"}}
	posted := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }
	tests := []struct {
		show      func() store.SeriesData
		opts      Options
		name      string
		published time.Time
		want      string
		// detail must appear in the reason or a note.
		detail string
	}{
		// Show titles
		{lanternHarbor, anime, "Lantern.Harbor.S01E04.1080p.WEB-DL-GRP", time.Time{}, "S01E04", ""},
		{lanternHarbor, anime, "Toro.no.Minato.S01E04.1080p.WEB-DL-GRP", time.Time{}, "S01E04", ""},
		{lanternHarbor, anime, "Laternenhafen.S01E06.German.DL.1080p.WEB-GRP", time.Time{}, "S01E06", ""},
		{lanternHarbor, anime, "Lanterns.and.Tides.S01E02.1080p.WEB-GRP", time.Time{}, "S01E02", ""},
		{lanternHarbor, anime, "The.Lantern.Harbor.S01E02.1080p.WEB-GRP", time.Time{}, "S01E02", ""},
		{lanternHarbor, anime, "Lantern.Harbor.Night.Shift.Diaries.S01E02.1080p.WEB-GRP", time.Time{}, "unknown", "does not match"},
		{lanternHarbor, anime, "[Grp] Lantern Harbor - Night Shift - 03 [1080p]", time.Time{}, "unknown", "only part of the title"},
		{lanternHarbor, anime, "[Grp] Tourou no Minato - Lantern Harbor - 03 [1080p]", time.Time{}, "S01E03", ""},
		{lanternHarbor, anime, "Tide.Runners.S01E01.1080p.WEB-GRP", time.Time{}, "unknown", "does not match"},

		// Episode titles
		{lanternHarbor, anime, "Lantern.Harbor.S01E09.The.Quiet.Pier.1080p.WEB-GRP", time.Time{}, "S01E04", "episode title"},
		{lanternHarbor, anime, "Laternenhafen.S01E09.Ueber.den.Daechern.German.1080p.WEB-GRP", time.Time{}, "S01E05", "episode title"},
		{lanternHarbor, anime, "Lantern.Harbor.S01E08.Gehen.wir.German.1080p.WEB-GRP", time.Time{}, "S01E08", "fits 2 episodes"},
		{lanternHarbor, anime, "Lantern.Harbor.E11.5.Erinnerung.an.die.Gefallenen.German.1080p.WEB-GRP", time.Time{}, "S00E02", "half episodes"},
		{lanternHarbor, anime, "[Grp] Tourou no Minato - 12.5 [1080p]", time.Time{}, "unknown", "half episodes"},

		// Absolute numbers count regular episodes; seasons in titles
		{lanternHarbor, anime, "[Grp] Tourou no Minato - 26 (S02E03) [1080p]", time.Time{}, "S02E03", ""},
		{lanternHarbor, anime, "[Grp] Tourou no Minato - 27 (S02E03) [1080p]", time.Time{}, "unknown", "disagree"},
		{lanternHarbor, anime, "[Grp] Tourou no Minato Ni no Shou - 24 [1080p]", time.Time{}, "S02E24", "season 2 comes from the title"},
		{lanternHarbor, anime, "[Grp] Lantern Harbor II - Dawn - 24 [1080p]", time.Time{}, "S02E24", "season 2 comes from the title"},
		{lanternHarbor, anime, "[Grp] Lantern Harbor II - 05 [1080p]", time.Time{}, "S02E05", "season 2 comes from the title"},
		{lanternHarbor, anime, "[Grp] Tourou no Minato II - 05 [1080p]", time.Time{}, "S02E05", "season 2 comes from the title"},
		{lanternHarbor, anime, "[Grp] Lantern Harbor II - Dawn - 30 [1080p]", time.Time{}, "unknown", "TVDB has no S02E30"},
		{lanternHarbor, anime, "[Grp] Lantern Harbor - 30 [1080p]", time.Time{}, "S02E07", ""},

		// Streaming-service orders
		{lanternHarbor, anime, "Lantern.Harbor.S02.1080p.NF.WEB-DL-GRP", time.Time{}, "unknown", "no reviewed rule"},
		{lanternHarbor, netflixRule, "Lantern.Harbor.S02.1080p.NF.WEB-DL-GRP", time.Time{}, "season S00E03-S00E06 (4)", "reviewed rule"},
		{lanternHarbor, anime, "Lantern.Harbor.S01E05.1080p.NF.WEB-DL-GRP", time.Time{}, "S01E05", ""},

		// TheXEM scene numbering
		{lanternHarbor, anime, "Lantern.Harbor.S03E01.1080p.WEB-GRP", time.Time{}, "S02E13", "TheXEM maps S03E01 to TVDB S02E13"},
		{lanternHarbor, anime, "Lantern.Harbor.S02E25.1080p.WEB-GRP", time.Time{}, "unknown", "which TVDB does not list"},
		{lanternHarbor, anime, "Lantern.Harbor.S04E01.1080p.WEB-GRP", time.Time{}, "unknown", "TVDB has no S04E01"},

		// Contents
		{lanternHarbor, anime, "Lantern.Harbor.S01.1080p.BluRay.x264-GRP", time.Time{}, "season S01E01-S01E23 (23)", ""},
		{lanternHarbor, anime, "Lantern.Harbor.S01.NCOP.1080p.BluRay.x264-GRP", time.Time{}, "extra", "NCOP"},
		{lanternHarbor, anime, "[Grp] Tourou no Minato - 01 (720p) [2 Hours Special]", time.Time{}, "S01E01 uncertain", "3 episodes aired on 2024-04-05"},
		{lanternHarbor, anime, "[Grp] Tourou no Minato - 04 (720p) [Special]", time.Time{}, "S01E04", ""},

		// Ambiguous bare numbers
		{harborPrecinct, Options{}, "harbor.precinct.101.hdtv-grp", posted("2013-10-02"), "S01E01", "had not aired"},
		{harborPrecinct, Options{}, "harbor.precinct.101.hdtv-grp", time.Time{}, "S01E01", "regular TV reads season and episode first"},
		{harborPrecinct, anime, "harbor.precinct.101.hdtv-grp", time.Time{}, "S05E13", "anime reads an absolute number first"},
		{harborPrecinct, Options{}, "harbor.precinct.101.hdtv-grp", posted("2013-09-01"), "unknown", "had aired"},

		// Regular TV
		{harborPrecinct, Options{}, "Harbor.Precinct.S01E04.Nicht.auf.meinem.Sofa.German.720p.WEB-GRP", time.Time{}, "S01E03", "the name says S01E04"},
		{harborPrecinct, Options{}, "Hafenrevier.S01E04.Geschmackssache.German.720p.WEB-GRP", time.Time{}, "S01E04", ""},
		{harborPrecinct, Options{}, "Harbor Precinct Season 2 (E01+02) 1080p WEB", time.Time{}, "S02E01-S02E02 (2)", ""},

		// Numbers that belong to the title
		{precinct100, Options{}, "Precinct.100.1080p.WEB-DL-GRP", time.Time{}, "unknown", `"100" belongs to the title`},
		{precinct100, Options{}, "Precinct.100.S01E03.1080p.WEB-DL-GRP", time.Time{}, "S01E03", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			show := NewShow(tc.show(), tc.opts)
			m := show.Match(release.Parse(tc.name), tc.published)
			got := summarize(show, m)
			details := append([]string{m.Reason}, m.Notes...)
			if got != tc.want || !slices.ContainsFunc(details, func(s string) bool { return strings.Contains(s, tc.detail) }) {
				t.Errorf("%s\n got: %s, reason %q, notes %q\nwant: %s with %q", tc.name, got, m.Reason, m.Notes, tc.want, tc.detail)
			}
		})
	}
}

func ptr(n int) *int       { return &n }
func ptr64(n int64) *int64 { return &n }
