package release

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// summary renders claims compactly so a table can assert every field.
// A field the case leaves empty must also be empty in the result.
type summary struct {
	Series, Episode, Numbering, Resolution, Source, Year, Group, Version,
	Flags, Services, Audio, Subtitles, Markers, Extras string
}

func summarize(c Claims) summary {
	// Alternative readings of the same text are joined with "|".
	var numbering []string
	for i, n := range c.Numbering {
		if i > 0 && n.Span == c.Numbering[i-1].Span {
			numbering[len(numbering)-1] += "|" + n.String()
			continue
		}
		numbering = append(numbering, n.String())
	}
	return summary{
		Series:     join(c.Series, " | ", func(v string) string { return v }),
		Episode:    join(c.EpisodeTitle, " | ", func(v string) string { return v }),
		Numbering:  strings.Join(numbering, ","),
		Resolution: join(c.Resolution, ",", Resolution.String),
		Source:     join(c.Source, ",", func(v Source) string { return string(v) }),
		Year:       join(c.Year, ",", strconv.Itoa),
		Group:      join(c.Group, ",", func(v string) string { return v }),
		Version:    join(c.Version, ",", strconv.Itoa),
		Flags:      join(c.Flags, ",", func(v Flag) string { return string(v) }),
		Services:   join(c.Services, ",", func(v Service) string { return string(v) }),
		Audio:      join(c.Audio, ",", func(v Language) string { return string(v) }),
		Subtitles:  join(c.Subtitles, ",", func(v Language) string { return string(v) }),
		Markers:    join(c.Markers, ",", func(v Marker) string { return string(v) }),
		Extras:     join(c.Extras, ",", func(v string) string { return v }),
	}
}

func join[T comparable](claims []Claim[T], sep string, f func(T) string) string {
	out := make([]string, len(claims))
	for i, c := range claims {
		out[i] = f(c.Value)
	}
	return strings.Join(out, sep)
}

// cases are made-up release names that follow the patterns of the real names
// reviewed for GRR-1 and GRR-5. The real names stay in the private
// garner-research repository (see TestRealReleaseCases).
var cases = []struct {
	name string
	want summary
}{
	// Scene TV: languages, quality and episode titles.
	{"Example.Show.S01E01.German.AC3D.DL.1080p.BluRay.x265-GRP", summary{
		Series: "Example Show", Numbering: "S01E01", Resolution: "1080p", Source: "bluray", Group: "GRP", Audio: "de,original"}},
	{"Twelve-Thirty.S01E01.German.DL.1080p.BluRay.x264-GRP", summary{
		Series: "Twelve-Thirty", Numbering: "S01E01", Resolution: "1080p", Source: "bluray", Group: "GRP", Audio: "de,original"}},
	{"Example.Show.S01E01.Wir.fangen.an.German.DL.1080p.BluRay.x264-GRP", summary{
		Series: "Example Show", Episode: "Wir fangen an", Numbering: "S01E01", Resolution: "1080p", Source: "bluray", Group: "GRP", Audio: "de,original"}},
	{"Example.Show.S01E01.GERMAN.DUBBED.DL.720p.BluRay.x264-GRP", summary{
		Series: "Example Show", Numbering: "S01E01", Resolution: "720p", Source: "bluray", Group: "GRP", Audio: "de,original"}},
	{"Example.Show.S01E01.Wir.fangen.an.GERMAN.DL.DUBBED.720p.WebHD.x264-GRP", summary{
		Series: "Example Show", Episode: "Wir fangen an", Numbering: "S01E01", Resolution: "720p", Source: "web", Group: "GRP", Audio: "de,original", Markers: "dubbed"}},
	{"Example.Show.S01E01.Wir.fangen.an.German.DL.WEB.h264.iNTERNAL-GRP", summary{
		Series: "Example Show", Episode: "Wir fangen an", Numbering: "S01E01", Source: "web", Group: "GRP", Flags: "internal", Audio: "de,original"}},
	{"Example.Show.S01E01.German.DVDRip.x264-GRP", summary{
		Series: "Example Show", Numbering: "S01E01", Source: "dvd", Group: "GRP", Audio: "de"}},
	{"Example.Show.S01E01.Pilot.720p.ALL4.WEB-DL.AAC2.0.H.264-GRP", summary{
		Series: "Example Show", Episode: "Pilot", Numbering: "S01E01", Resolution: "720p", Source: "webdl", Group: "GRP", Services: "all4"}},
	{"Example Show Season 8 (E01+02) BDrip HEVC 10-bit AAC5 1", summary{
		Series: "Example Show", Numbering: "S08E01-E02", Source: "bluray"}},
	{"Example.Show.2007.S01E01.Pilot.German.720p.HMAX.WEB-DL.MULTi.DDP5.1.H.264-GRP", summary{
		Series: "Example Show", Episode: "Pilot", Numbering: "S01E01", Resolution: "720p", Source: "webdl", Year: "2007", Group: "GRP", Services: "max", Audio: "de", Markers: "multi-audio"}},
	{"Example.Show.S01E15.Robot.2.0.German.DD20.DL.1080p.BD.x264-GRP", summary{
		Series: "Example Show", Episode: "Robot 2.0", Numbering: "S01E15", Resolution: "1080p", Source: "bluray", Group: "GRP", Audio: "de,original"}},
	{"Example.Show.S01.1080p.AMZN.WEB-DL.DDP2.0.H265-GRP", summary{
		Series: "Example Show", Numbering: "S01", Resolution: "1080p", Source: "webdl", Group: "GRP", Services: "amazon"}},
	{"Example.Show.S01E01.The.Pilot.Bluray.1080p.AVC.DTS-HD.MA.5.1.REMUX-GRP-AsRequested", summary{
		Series: "Example Show", Episode: "The Pilot", Numbering: "S01E01", Resolution: "1080p", Source: "bluray", Group: "GRP", Flags: "remux"}},
	{"Example Show 1x01 Piloto [HDRIP 720p x264 AC3][Spanish-English][by some-uploader][example.net]", summary{
		Series: "Example Show", Episode: "Piloto", Numbering: "S01E01", Resolution: "720p", Source: "hdrip", Audio: "es,en"}},
	{"Example.Show.S01E16.720p.AMZN.Web-DL.[TR-EN].DDP5.1.H.264-GRP", summary{
		Series: "Example Show", Numbering: "S01E16", Resolution: "720p", Source: "webdl", Group: "GRP", Services: "amazon", Audio: "tr,en"}},
	{"Example.Show.2011.S01E04.Beispiel.GERMAN.1080p.ARDP.WEB-DL.AAC2.0.H.264-GRP", summary{
		Series: "Example Show", Episode: "Beispiel", Numbering: "S01E04", Resolution: "1080p", Source: "webdl", Year: "2011", Group: "GRP", Services: "ard", Audio: "de"}},
	{"Example.Show.S01E04.Nicht.ueber.mein.Sofa.German.SATRip.XviD-GRP", summary{
		Series: "Example Show", Episode: "Nicht ueber mein Sofa", Numbering: "S01E04", Source: "tv", Group: "GRP", Audio: "de"}},
	{"Example.Show.S01.1080P.WEB.DL.DD+2.0.H.264.GRP", summary{
		Series: "Example Show", Numbering: "S01", Resolution: "1080p", Source: "webdl"}},
	{"Example.Show.S01.COMPLETE.GERMAN.AC3.720p.BDRiP.x264-GRP", summary{
		Series: "Example Show", Numbering: "S01", Resolution: "720p", Source: "bluray", Group: "GRP", Flags: "complete", Audio: "de"}},
	{"Example.Show.S01.1080p.BluRay.REMUX.AVC.DD.2.0-GRP", summary{
		Series: "Example Show", Numbering: "S01", Resolution: "1080p", Source: "bluray", Group: "GRP", Flags: "remux"}},
	// "Staffel 1.2" may mean seasons 1 and 2 or something else: unreadable.
	{"Example.Show.-.Staffel.1.2.720p", summary{
		Series: "Example Show", Resolution: "720p"}},
	{"Example.Show.S01E01.part1.0 hello.world.German.DL.1080p.BluRay.x264-GRP", summary{
		Series: "Example Show", Episode: "part1 0 hello world", Numbering: "S01E01", Resolution: "1080p", Source: "bluray", Group: "GRP", Audio: "de,original"}},
	{"Example.Show.S01E01.part1.0 hello.world.German.WS.BDRip.x264-GRP", summary{
		Series: "Example Show", Episode: "part1 0 hello world", Numbering: "S01E01", Source: "bluray", Group: "GRP", Audio: "de"}},
	{"Example.Show.2015.S01E01.GERMAN.DL.1080p.BluRay.AVC.REMUX-GRP", summary{
		Series: "Example Show", Numbering: "S01E01", Resolution: "1080p", Source: "bluray", Year: "2015", Group: "GRP", Flags: "remux", Audio: "de,original"}},
	{"Example.Show.S01E01.German.DL.BD.x264-GRP", summary{
		Series: "Example Show", Numbering: "S01E01", Source: "bluray", Group: "GRP", Audio: "de,original"}},
	{"Example.Show.S01E01.iTALiAN.MULTi.1080p.BluRay.x264-GRP", summary{
		Series: "Example Show", Numbering: "S01E01", Resolution: "1080p", Source: "bluray", Group: "GRP", Audio: "it", Markers: "multi-audio"}},
	{"Example.Show.2016.S06E10.German.DL.1080p.WEBRip.AV1-GRP", summary{
		Series: "Example Show", Numbering: "S06E10", Resolution: "1080p", Source: "webrip", Year: "2016", Group: "GRP", Audio: "de,original"}},
	{"Example.Show.S06E10.Partner.fuer.immer.GERMAN.AACD.1080p.BluRay.x265-GRP", summary{
		Series: "Example Show", Episode: "Partner fuer immer", Numbering: "S06E10", Resolution: "1080p", Source: "bluray", Group: "GRP", Audio: "de"}},
	{"Example.Show.S06.GERMAN.DUBBED.DL.1080p.WEBRiP.x264-GRP", summary{
		Series: "Example Show", Numbering: "S06", Resolution: "1080p", Source: "webrip", Group: "GRP", Audio: "de,original"}},
	{"Example.Show.[2021].S06E01.1080p.[60FPS].Bluray.x264.Dual.Audio.[Hi.Eng].DDP.5.1.Esub.[-Some Uploader-]", summary{
		Series: "Example Show", Numbering: "S06E01", Resolution: "1080p", Source: "bluray", Year: "2021", Audio: "hi,en", Subtitles: "en", Markers: "dual-audio"}},
	{"Example.Show.S06E07.2021.1080p.Netflix.WEB-DL.AVC.DDP.5.1-GRP", summary{
		Series: "Example Show", Numbering: "S06E07", Resolution: "1080p", Source: "webdl", Year: "2021", Group: "GRP", Services: "netflix"}},
	{"Example.Show.1x01.Ciao.Amico.ITA.BDMux.x264-GRP", summary{
		Series: "Example Show", Episode: "Ciao Amico", Numbering: "S01E01", Source: "bluray", Group: "GRP", Audio: "it"}},
	{"Example  Show S01E01 NL Subs", summary{
		Series: "Example Show", Numbering: "S01E01", Subtitles: "nl"}},
	{"Example Show (2013) S01e01 XviD NLSubs --Q o Q--", summary{
		Series: "Example Show", Numbering: "S01E01", Year: "2013", Subtitles: "nl"}},
	{"Example Show-S08E09-Epilogue The Final Part", summary{
		Series: "Example Show", Episode: "Epilogue The Final Part", Numbering: "S08E09"}},

	// Anime: languages and numbering.
	{"Example.Saga.S01E01.German.ML.ANiME.720p.CR.WEB.H264-GRP", summary{
		Series: "Example Saga", Numbering: "S01E01", Resolution: "720p", Source: "web", Group: "GRP", Services: "crunchyroll", Audio: "de", Markers: "multi-audio"}},
	{"Example.Saga.E01.1080p.BluRay.x264-GRP", summary{
		Series: "Example Saga", Numbering: "E01", Resolution: "1080p", Source: "bluray", Group: "GRP"}},
	{"Example.Saga.Winters.End.S01E01.The.Winters.End.1080p.CR.WEB-DL.AAC2.0.H.264-GRP", summary{
		Series: "Example Saga Winters End", Episode: "The Winters End", Numbering: "S01E01", Resolution: "1080p", Source: "webdl", Group: "GRP", Services: "crunchyroll"}},
	{"[SubGroup] Example Saga - 29 (S02E01) (WEB 1080p HEVC EAC-3)", summary{
		Series: "Example Saga", Numbering: "E29,S02E01", Resolution: "1080p", Source: "web", Group: "SubGroup"}},
	{"[Sub-Group] Example Saga 2nd Season - 01 [1080p CR WEB-DL AVC AAC][MultiSub][D38EF19D]", summary{
		Series: "Example Saga", Numbering: "S02E01", Resolution: "1080p", Source: "webdl", Group: "Sub-Group", Services: "crunchyroll", Markers: "multi-subs"}},
	{"Example.Saga.S01E01.1080p.CR.WEB-DL.MULTI.AAC2.0.H.264-GRP", summary{
		Series: "Example Saga", Numbering: "S01E01", Resolution: "1080p", Source: "webdl", Group: "GRP", Services: "crunchyroll", Markers: "multi-audio"}},
	{"Example.Saga.S01E01.2023.2160p.WEB-DL.H.265.HDR.10bit.AAC2.0-GRP", summary{
		Series: "Example Saga", Numbering: "S01E01", Resolution: "2160p", Source: "webdl", Year: "2023", Group: "GRP", Flags: "hdr"}},
	{"Example.Saga.S02.1080p.NF.WEB-DL.Multi-Audio.AAC.2.0.H.264-GRP", summary{
		Series: "Example Saga", Numbering: "S02", Resolution: "1080p", Source: "webdl", Group: "GRP", Services: "netflix", Markers: "multi-audio"}},
	{"[SubGroup].Example.Saga.Second.Chapter-24.[BD.720p.YUV444P10.E-AC3]", summary{
		Series: "Example Saga Second Chapter", Numbering: "E24", Resolution: "720p", Source: "bluray", Group: "SubGroup"}},
	{"Example.Saga.Second.Chapter-00.TBS.1280x720.x264.AAC", summary{
		Series: "Example Saga Second Chapter", Numbering: "E00", Resolution: "720p"}},
	{"Example.Saga.E111.1080p.BluRay.x264-GRP", summary{
		Series: "Example Saga", Numbering: "E111", Resolution: "1080p", Source: "bluray", Group: "GRP"}},
	{"Example.Saga.S02E001.Ger.Eng.Sub.AAC.1080p.CR.WEB.H264-GRP", summary{
		Series: "Example Saga", Numbering: "S02E01", Resolution: "1080p", Source: "web", Group: "GRP", Services: "crunchyroll", Subtitles: "de,en"}},
	{"Example.Saga.2017.S02E01.German.Subbed.ANiME.AAC.1080p.CR.WEB.H264-GRP", summary{
		Series: "Example Saga", Numbering: "S02E01", Resolution: "1080p", Source: "web", Year: "2017", Group: "GRP", Services: "crunchyroll", Subtitles: "de"}},
	{"Example.Saga.S02E01.The.Battle.Begins.1080p.CR.WEB-DL.JPN.AAC2.0.H.264.MSubs-GRP", summary{
		Series: "Example Saga", Episode: "The Battle Begins", Numbering: "S02E01", Resolution: "1080p", Source: "webdl", Group: "GRP", Services: "crunchyroll", Audio: "ja", Markers: "multi-subs"}},
	{"[Sub-Group].Example.Saga.(2017).S01E158-S01E170.(BD.1080p.OPUS).[Dual.Audio].v2", summary{
		Series: "Example Saga", Numbering: "S01E158-E170", Resolution: "1080p", Source: "bluray", Year: "2017", Group: "Sub-Group", Version: "2", Markers: "dual-audio"}},
	{"Example.Saga.S03E02.iTALiAN.1080p.WEB.H264-GRP", summary{
		Series: "Example Saga", Numbering: "S03E02", Resolution: "1080p", Source: "web", Group: "GRP", Audio: "it"}},
	{"[SubGroup] Example Saga Arc-18.5 [BD 1920x1080 x.264 Flac]", summary{
		Series: "Example Saga Arc", Numbering: "E18.5", Resolution: "1080p", Source: "bluray", Group: "SubGroup"}},
	{"Example.Saga-War.Of.Worlds.Part.2.S03.2020.2160p.WEB-DL.H.265.HDR.10bit.AAC2.0-GRP", summary{
		Series: "Example Saga-War Of Worlds Part 2", Numbering: "S03", Resolution: "2160p", Source: "webdl", Year: "2020", Group: "GRP", Flags: "hdr"}},
	{"Example.Saga.Reencarnación.desde.cero.S02E01.2021.1080p.NF.WEB-DL.AVC.DUAL.AAC.LC.2.0.SUBS", summary{
		Series: "Example Saga Reencarnación desde cero", Numbering: "S02E01", Resolution: "1080p", Source: "webdl", Year: "2021", Services: "netflix", Markers: "dual-audio,subs"}},
	{"[SubGroup] Example Saga - Second Title (2021) - S02 v2 [Bluray-1080p][Opus 2.0][Dual Audio][UNCENSORED][AV1]", summary{
		Series: "Example Saga - Second Title | Example Saga | Second Title", Numbering: "S02", Resolution: "1080p", Source: "bluray", Year: "2021", Group: "SubGroup", Version: "2", Flags: "uncensored", Markers: "dual-audio"}},
	{"Example.Saga.S08E147.More.1080p.CR.WEB-DL.DUAL.AAC2.0.H.264-GRP", summary{
		Series: "Example Saga", Episode: "More", Numbering: "S08E147", Resolution: "1080p", Source: "webdl", Group: "GRP", Services: "crunchyroll", Markers: "dual-audio"}},
	{"[SubGroup] Example Saga - S08E12 [1080p WEBRip AV1]", summary{
		Series: "Example Saga", Numbering: "S08E12", Resolution: "1080p", Source: "webrip", Group: "SubGroup"}},
	{"Example.Saga.S03E03.DUBBED.1080p.WEB.H264-GRP", summary{
		Series: "Example Saga", Numbering: "S03E03", Resolution: "1080p", Source: "web", Group: "GRP", Markers: "dubbed"}},
	{"Example.Saga.S08E11.DUBBED.1080p.AV1.10bit-GRP", summary{
		Series: "Example Saga", Numbering: "S08E11", Resolution: "1080p", Group: "GRP", Markers: "dubbed"}},
	{"[SubGroup].Example.Saga-S03E01.[English.Dub].[CR.WEB-RIP.1080p.HEVC-10.AAC].[4E26501A]", summary{
		Series: "Example Saga", Numbering: "S03E01", Resolution: "1080p", Source: "webrip", Group: "SubGroup", Services: "crunchyroll", Audio: "en"}},
	{"Example.Saga.S03E12.1080p.BluRay.REMUX.Dual-Audio.AVC.FLAC2.0-GRP", summary{
		Series: "Example Saga", Numbering: "S03E12", Resolution: "1080p", Source: "bluray", Group: "GRP", Flags: "remux", Markers: "dual-audio"}},
	{"Example.Saga.S4.-.31.(1080p).[F67A7AC0].mkv", summary{
		Series: "Example Saga", Numbering: "S04E31", Resolution: "1080p"}},
	{"[SubGroup].Example.Saga.S4.(25-37).(720p).[Batch]", summary{
		Series: "Example Saga", Numbering: "S04E25-E37", Resolution: "720p", Group: "SubGroup", Flags: "batch"}},
	{"Example.Saga.S04E37.A.Future.To.Look.Forward.To.1080p.AMZN.WEB-DL.JPN.DDP2.0.H.264.ESub-GRP", summary{
		Series: "Example Saga", Episode: "A Future To Look Forward To", Numbering: "S04E37", Resolution: "1080p", Source: "webdl", Group: "GRP", Services: "amazon", Audio: "ja", Subtitles: "en"}},
	{"Example.Saga.Ep.17.German.PCM.DL.1080p.Bluray.x264-GRP", summary{
		Series: "Example Saga", Numbering: "E17", Resolution: "1080p", Source: "bluray", Group: "GRP", Audio: "de,original"}},
	{"Example.Saga.Schwerter.der.Helden.S01E24.German.AC3.1080p.BDRip.x265-GRP", summary{
		Series: "Example Saga Schwerter der Helden", Numbering: "S01E24", Resolution: "1080p", Source: "bluray", Group: "GRP", Audio: "de"}},
	{"Example.Saga.S01E24.ANiME.SPANiSH.WEB.x264-GRP", summary{
		Series: "Example Saga", Numbering: "S01E24", Source: "web", Group: "GRP", Audio: "es"}},
	{"Example.Saga.Der.Gott.der.Helden.2022.S02E15.GERMAN.DL.ANiME.720p.WEB.H264-GRP", summary{
		Series: "Example Saga Der Gott der Helden", Numbering: "S02E15", Resolution: "720p", Source: "web", Year: "2022", Group: "GRP", Audio: "de,original"}},
	{"Example.Saga.S02E13.MULTi.1080p.WEB.x264-GRP", summary{
		Series: "Example Saga", Numbering: "S02E13", Resolution: "1080p", Source: "web", Group: "GRP", Markers: "multi-audio"}},
	{"EXAMPLE.SAGA.Heavy.Dark.S02E09.480p.x264-GRP", summary{
		Series: "EXAMPLE SAGA Heavy Dark", Numbering: "S02E09", Resolution: "480p", Group: "GRP"}},
	{"Example.Saga.S01E01.German.DL.AAC.1080p.WEB.h264.v2-GRP", summary{
		Series: "Example Saga", Numbering: "S01E01", Resolution: "1080p", Source: "web", Group: "GRP", Version: "2", Audio: "de,original"}},
	{"Example.Saga.S03E04.The.Spear.Strikes.1080P.WebDL.AAC.GRP", summary{
		Series: "Example Saga", Episode: "The Spear Strikes", Numbering: "S03E04", Resolution: "1080p", Source: "webdl"}},
	{"Example.Saga.S03E03.The.End.Mountains.EAC3.2.0.1080p.Bluray.x265-GRP", summary{
		Series: "Example Saga", Episode: "The End Mountains", Numbering: "S03E03", Resolution: "1080p", Source: "bluray", Group: "GRP"}},
	{"Example.Saga.FINAL.SEASON.S08E03.MULTi.1080p.WEBRiP.x265-GRP", summary{
		Series: "Example Saga", Numbering: "S08E03", Resolution: "1080p", Source: "webrip", Group: "GRP", Markers: "multi-audio"}},
	{"Example.Saga.E11.5.Ein.Rueckblick.German.Subbed.2021.ANiME.1080p.BluRay.x264-GRP", summary{
		Series: "Example Saga", Episode: "Ein Rueckblick", Numbering: "E11.5", Resolution: "1080p", Source: "bluray", Year: "2021", Group: "GRP", Subtitles: "de"}},
	{"Show.S01E11.5.Recap.1080p.WEB-DL-GRP", summary{
		Series: "Show", Episode: "Recap", Numbering: "S01E11.5", Resolution: "1080p", Source: "webdl", Group: "GRP"}},

	// Fansub numbering after the title.
	{"[SubGroup] Example Saga-01 [720p]", summary{
		Series: "Example Saga", Numbering: "E01", Resolution: "720p", Group: "SubGroup"}},
	{"[SubGroup] Example Saga - 24 END (BD 1920x1080 x.264 FLACx2)", summary{
		Series: "Example Saga", Numbering: "E24", Resolution: "1080p", Source: "bluray", Group: "SubGroup"}},
	{"[SubGroup].Example.Saga-S02E24-[373DCB69]", summary{
		Series: "Example Saga", Numbering: "S02E24", Group: "SubGroup"}},
	{"[SubGroup].Example.Saga-S01E01v2", summary{
		Series: "Example Saga", Numbering: "S01E01", Group: "SubGroup", Version: "2"}},
	{"[SubGroup].Example.Saga.S01E01.v3.[1080p.EAC3.AV1][dual.audio]", summary{
		Series: "Example Saga", Numbering: "S01E01", Resolution: "1080p", Group: "SubGroup", Version: "3", Markers: "dual-audio"}},
	{"[SubGroup] Example Saga 01 [BDRip 1920x1080 HEVC TrueHD]", summary{
		Series: "Example Saga | Example Saga 01", Numbering: "E01", Resolution: "1080p", Source: "bluray", Group: "SubGroup"}},
	{"[SubGroup] Example Saga (2023) - 01 (WEB 1080p H.265 AAC)", summary{
		Series: "Example Saga", Numbering: "E01", Resolution: "1080p", Source: "web", Year: "2023", Group: "SubGroup"}},
	{"[SubGroup].Example.Saga.2023-01.WEB.1080p.H.265.AAC", summary{
		Series: "Example Saga", Numbering: "E01", Resolution: "1080p", Source: "web", Year: "2023", Group: "SubGroup"}},
	{"[SubGroup] Example - Second Title 01 - 04 [WebRip][1080p][H265][Multiple Subtitles] (1)", summary{
		Series: "Example - Second Title | Example | Second Title | Example - Second Title 01", Numbering: "E01-E04", Resolution: "1080p", Source: "webrip", Group: "SubGroup", Markers: "subs"}},
	{"[SubGroup] Example Saga - 01 ~ 24 [BD 720p][Multiple Subtitle]", summary{
		Series: "Example Saga", Numbering: "E01-E24", Resolution: "720p", Source: "bluray", Group: "SubGroup", Markers: "subs"}},
	{"[SubGroup] Example Saga S3 - 12 END (BD 1920x1080 x265-10Bit Flac)", summary{
		Series: "Example Saga", Numbering: "S03E12", Resolution: "1080p", Source: "bluray", Group: "SubGroup"}},
	{"[SubGroup] Example Saga S3 01 (BDRip 1920x1080 x264 FLAC)", summary{
		Series: "Example Saga", Numbering: "S03E01", Resolution: "1080p", Source: "bluray", Group: "SubGroup"}},

	// Titles that end in a number.
	{"[SubGroup] Example 100 (2016) - S01 [Bluray-1080p][Opus 2.0][Dual Audio][AV1]", summary{
		Series: "Example 100", Numbering: "S01", Resolution: "1080p", Source: "bluray", Year: "2016", Group: "SubGroup", Markers: "dual-audio"}},
	{"Example 100 S03E01 1080p CR WEB-DL MULTI AAC2.0 H.264-GRP", summary{
		Series: "Example 100", Numbering: "S03E01", Resolution: "1080p", Source: "webdl", Group: "GRP", Services: "crunchyroll", Markers: "multi-audio"}},
	{"Example.100.S03E01.1080p.WEB-DL-GRP", summary{
		Series: "Example 100", Numbering: "S03E01", Resolution: "1080p", Source: "webdl", Group: "GRP"}},
	{"Example.No.8.S02E00.SUBFRENCH.1080p.WEB.x264-GRP", summary{
		Series: "Example No 8", Numbering: "S02E00", Resolution: "1080p", Source: "web", Group: "GRP", Subtitles: "fr"}},
	// 101 may be S01E01 or absolute episode 101; matching decides by air date.
	{"example.show.101.hdtv-grp", summary{
		Series: "example show | example show 101", Numbering: "S01E01|E101", Source: "hdtv", Group: "grp"}},
	{"Example.Show.2512.720p.HDTV.x264-GRP", summary{
		Series: "Example Show | Example Show 2512", Numbering: "S25E12|E2512", Resolution: "720p", Source: "hdtv", Group: "GRP"}},
	// No episode 0 reading; the number may also belong to the title.
	{"Example.Show.100.1080p.WEB-DL-GRP", summary{
		Series: "Example Show | Example Show 100", Numbering: "E100", Resolution: "1080p", Source: "webdl", Group: "GRP"}},

	// Extras never satisfy regular episodes.
	{"[SubGroup] Example Saga - Spin Off (Mini Anime) - 01 [92766F13]", summary{
		Series: "Example Saga - Spin Off | Example Saga | Spin Off", Numbering: "E01", Group: "SubGroup", Extras: "MINI"}},
	{"[SubGroup] Example Saga Second Chapter-TV + SP", summary{
		Series: "Example Saga Second Chapter", Group: "SubGroup", Extras: "SP"}},
	{"Example.Saga.S03.NCOP.1080p.BluRay.REMUX.Dual-Audio.AVC.FLAC2.0-GRP", summary{
		Series: "Example Saga", Numbering: "S03", Resolution: "1080p", Source: "bluray", Group: "GRP", Flags: "remux", Markers: "dual-audio", Extras: "NCOP"}},
	{"[Sub Group] Example No. 8 [Season 2] + NC [BD] [Dual Audio][1080p][HEVC 10bit x265][AAC][Eng Sub] [Batch]", summary{
		Series: "Example No 8", Numbering: "S02", Resolution: "1080p", Source: "bluray", Group: "Sub Group", Flags: "batch", Subtitles: "en", Markers: "dual-audio", Extras: "NC"}},
	{"[SubGroup] Example 8 Gou S2-Extra EP Spin Title [BD 3840x2160 x265-10Bit FLACx2]", summary{
		Series: "Example 8 Gou", Numbering: "S02", Resolution: "2160p", Source: "bluray", Group: "SubGroup", Extras: "EXTRA"}},

	// Groups: re-upload suffixes, newsgroup prefixes and hyphenated names.
	{"Example.Show.S01E01.720pWEB-DL.H264-AsRequested", summary{
		Series: "Example Show", Numbering: "S01E01", Resolution: "720p", Source: "webdl"}},
	{"[a.b.teevee] Example.Show.S01E01.1080p.WEB-DL.DD5.1.H.264-GRP", summary{
		Series: "Example Show", Numbering: "S01E01", Resolution: "1080p", Source: "webdl", Group: "GRP"}},
	{"Example.Saga.S02E04.1080p.MULTi-GRP-RakuvGEROV-Obfuscated", summary{
		Series: "Example Saga", Numbering: "S02E04", Resolution: "1080p", Group: "GRP", Markers: "multi-audio"}},
	{"Example.Show.S01E01.480p.WEB-DL.nSD.x264.1-GRP-Obfuscated", summary{
		Series: "Example Show", Numbering: "S01E01", Resolution: "480p", Source: "webdl", Group: "GRP"}},
	{"Example.Saga.S02.German.DL.TrueHD.1080p.BluRay.10bit.x265-Tofu-sama", summary{
		Series: "Example Saga", Numbering: "S02", Resolution: "1080p", Source: "bluray", Group: "Tofu-sama", Audio: "de,original"}},
}

func TestParse(t *testing.T) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarize(Parse(tc.name))
			if got != tc.want {
				t.Errorf("Parse(%q)\n got: %+v\nwant: %+v", tc.name, got, tc.want)
			}
		})
	}
}

func TestSpansPointAtEvidence(t *testing.T) {
	c := Parse("Example.Saga.S02E001.Ger.Eng.Sub.AAC.1080p.CR.WEB.H264-GRP")
	at := func(s Span) string { return c.Name[s.Start:s.End] }
	checks := map[string]string{
		"numbering": at(c.Numbering[0].Span),
		"subtitle":  at(c.Subtitles[1].Span),
		"group":     at(c.Group[0].Span),
	}
	want := map[string]string{"numbering": "S02E001", "subtitle": "Eng", "group": "GRP"}
	for k, v := range want {
		if checks[k] != v {
			t.Errorf("%s span covers %q, want %q", k, checks[k], v)
		}
	}
}

func TestValueReportsAgreement(t *testing.T) {
	one := []Claim[Resolution]{{Value: 1080}, {Value: 1080}}
	two := []Claim[Resolution]{{Value: 1080}, {Value: 720}}
	if v, s := Value(one); v != 1080 || s != Unambiguous {
		t.Errorf("repeated claim: got %v %v", v, s)
	}
	if _, s := Value(two); s != Conflicting {
		t.Errorf("different claims: got %v", s)
	}
	if _, s := Value[Resolution](nil); s != Missing {
		t.Errorf("no claims: got %v", s)
	}
}

func TestUnreadableInputYieldsNothing(t *testing.T) {
	for _, name := range []string{
		strings.Repeat("Show.S01E01.", MaxNameLength/12+1),
		"Show.S01E05-E03.1080p",   // reversed range
		"Show.S01E001-E999.1080p", // range beyond the expansion limit
	} {
		if n := Parse(name).Numbering; len(n) != 0 {
			t.Errorf("Parse(%.40q…) claimed %v", name, n)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, tc := range cases {
		f.Add(tc.name)
	}
	f.Add("Show\x1b[31m.S01E01\x00.1080p")
	f.Add("Show\u202e.S01E01\xff.1080p")
	f.Fuzz(func(t *testing.T, name string) {
		c := Parse(name)
		check := func(what string, s Span) {
			if s.Start < 0 || s.End > len(name) || s.Start >= s.End {
				t.Fatalf("%s span %v outside %d-byte name", what, s, len(name))
			}
		}
		for _, n := range c.Numbering {
			check("numbering", n.Span)
			if len(n.Episodes) > maxEpisodes {
				t.Fatalf("numbering expanded to %d episodes", len(n.Episodes))
			}
			if !n.Seasoned && len(n.Episodes) == 0 {
				t.Fatalf("claim without season or episodes: %+v", n)
			}
		}
		for _, text := range append(append(append([]Claim[string]{}, c.Series...), c.EpisodeTitle...), c.Group...) {
			check("text", text.Span)
			unsafe := func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }
			if !utf8.ValidString(text.Value) || strings.IndexFunc(text.Value, unsafe) >= 0 {
				t.Fatalf("text %q contains invalid or control characters", text.Value)
			}
		}
		_ = fmt.Sprint(c)
	})
}
