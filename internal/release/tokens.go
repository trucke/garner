package release

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type dash uint8

const (
	noDash     dash = iota
	gluedDash       // Twelve-Thirty, Title-24
	spacedDash      // Title - 29
)

type kind uint8

const (
	kWord kind = iota
	kNumber
	kDecimal
	kNumberVersion // 01v2
	kOpen
	kClose
	kPlus
	kResolution
	kSource
	kVideo
	kAudioCodec
	kService
	kFlag
	kYear
	kVersion
	kLanguage // full name or three-letter code
	kLangCode // two-letter code, only meaningful next to other language evidence
	kSubWord
	kDubWord
	kSubLang // ESub, VOSTFR
	kMarker
	kDL
	kSeasonEpisode // S01E01, S01E01E02
	kSeason        // S01, S4
	kEpisode       // E01, E01+02
	kCross         // 1x01
	kEpWord        // Ep, Episode
	kSeasonWord    // Season, Staffel
	kOrdinal       // 2nd
	kSpecial       // NCOP, OVA, S08SP01
	kIgnore        // recognised tags that carry nothing Garner uses
)

type token struct {
	text       string
	start, end int
	dash       dash
	kind       kind

	num, frac int // numbers, years, versions, resolution
	source    Source
	flag      Flag
	lang      Language
	marker    Marker
	service   Service
}

var (
	// A decimal glued to letters, as in EAC3.2.0 or DDP5.1, is a codec with
	// channels and splits normally.
	reDecimal  = regexp.MustCompile(`(?:^|[^0-9A-Za-z])([0-9]{1,3}\.[0-9])(?:[^0-9]|$)`)
	reCodecDot = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])([hx]\.26[45])(?:[^0-9]|$)`)
	// Recap episodes such as E11.5 keep their fraction.
	reEpisodeDot = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])((?:S[0-9]{1,2})?E[0-9]{1,4}\.[0-9])(?:[^0-9]|$)`)

	reNumber          = regexp.MustCompile(`^[0-9]{1,4}$`)
	reDecimalToken    = regexp.MustCompile(`^([0-9]{1,3})\.([0-9])$`)
	reNumberVersion   = regexp.MustCompile(`^([0-9]{1,4})[vV]([0-9])$`)
	reVersion         = regexp.MustCompile(`^[vV]([0-9])$`)
	reResolution      = regexp.MustCompile(`(?i)^([0-9]{3,4})[pi]$`)
	reDimensions      = regexp.MustCompile(`(?i)^([0-9]{3,4})x([0-9]{3,4})$`)
	reSeasonEpisode   = regexp.MustCompile(`(?i)^S([0-9]{1,2})E([0-9]{1,4})(?:\.([0-9]))?((?:E[0-9]{1,4})*)(?:v([0-9]))?$`)
	reSeason          = regexp.MustCompile(`(?i)^S([0-9]{1,2})$`)
	reEpisode         = regexp.MustCompile(`(?i)^E([0-9]{1,4})(?:\.[0-9]|(?:\+E?[0-9]{1,4})*)$`)
	reCross           = regexp.MustCompile(`(?i)^([0-9]{1,2})x([0-9]{1,3})$`)
	reOrdinal         = regexp.MustCompile(`(?i)^([0-9]{1,2})(?:st|nd|rd|th)$`)
	reSpecial         = regexp.MustCompile(`(?i)^S[0-9]{1,2}(?:SP|OVA|OAD)[0-9]{1,3}$`)
	reGluedResolution = regexp.MustCompile(`(?i)^([0-9]{3,4}[pi])([a-z].*)$`)
	reAudioCodec      = regexp.MustCompile(`(?i)^(?:dd\+?|ddp|e-?ac-?3|ac3d?|aacd?|dts(?:-?hd)?|hdma|truehd|atmos|flac(?:x[0-9])?|opus|l?pcm|mp3)(?:[0-9](?:\.[0-9])?|[0-9]{2})?$`)
)

// hyphenated lists tags that contain a hyphen and must not be split.
var hyphenated = map[string]bool{
	"web-dl": true, "web-rip": true, "web-hd": true, "blu-ray": true, "dts-hd": true,
	"e-ac3": true, "eac-3": true, "e-ac-3": true, "10-bit": true, "8-bit": true,
	"multi-audio": true, "dual-audio": true, "multi-sub": true, "multi-subs": true,
}

var sources = map[string]Source{
	"bluray": BluRay, "blu-ray": BluRay, "bdrip": BluRay, "brrip": BluRay, "bd": BluRay, "bdremux": BluRay,
	"web-dl": WebDL, "webdl": WebDL, "webrip": WebRip, "web-rip": WebRip,
	"web": Web, "webhd": Web, "web-hd": Web, "netflixhd": Web, "amazonhd": Web, "ituneshd": Web,
	"bdmux": BluRay, "webmux": Web, "dlmux": Web,
	"hdtv": HDTV, "satrip": TV, "dvbrip": TV, "tvrip": TV, "dvdrip": DVD, "dvd": DVD, "hdrip": HDRip,
}

var flags = map[string]Flag{
	"remux": Remux, "repack": Repack, "repack2": Repack, "proper": Proper, "internal": Internal,
	"complete": Complete, "batch": Batch, "uncensored": Uncensored,
	"hdr": HDR, "hdr10": HDR, "hdr10+": HDR, "dv": DolbyVision, "dovi": DolbyVision,
}

var video = set("x264", "x265", "h264", "h265", "h.264", "h.265", "x.264", "x.265", "hevc", "avc", "av1",
	"xvid", "divx", "vp9", "10bit", "10-bit", "8bit", "8-bit", "hi10p", "yuv444p10", "60fps", "sdr")

var services = map[string]Service{
	"nf": "netflix", "netflix": "netflix", "netflixhd": "netflix", "amzn": "amazon", "amazon": "amazon",
	"amazonhd": "amazon", "cr": "crunchyroll", "crunchyroll": "crunchyroll", "hmax": "max", "dsnp": "disney",
	"atvp": "apple", "itunes": "itunes", "ituneshd": "itunes", "hulu": "hulu", "pcok": "peacock", "all4": "all4",
	"ard": "ard", "ardp": "ard", "zdf": "zdf", "rtlp": "rtl", "joyn": "joyn", "bili": "bilibili", "yt": "youtube",
	"mubi": "mubi", "adn": "adn", "funi": "funimation",
}

var ignored = set("anime", "audio", "mkv", "ws")

// reposts are suffixes re-uploaders append to the real group, as in
// "x264-NTb-Obfuscated". The list follows Sonarr's CleanReleaseGroupRegex.
var reposts = set("rp", "1", "nzbgeek", "obfuscated", "scrambled", "sample", "pre", "postbot", "xpost",
	"whiterev", "buymore", "asrequested", "alternativetorequested", "gerov", "z0ids3n", "chamele0n", "4p",
	"4planet", "altezachen", "repackpost")

// ignoredExact are case-sensitive scene markers that would otherwise collide
// with title words such as "Winters End".
var ignoredExact = set("END", "FiNAL")

var languages = map[string]Language{
	"german": "de", "deutsch": "de", "english": "en", "japanese": "ja", "spanish": "es", "castellano": "es",
	"italian": "it", "french": "fr", "turkish": "tr", "hindi": "hi", "korean": "ko", "chinese": "zh",
	"russian": "ru", "portuguese": "pt", "polish": "pl", "dutch": "nl",
	"ger": "de", "deu": "de", "eng": "en", "jpn": "ja", "jap": "ja", "ita": "it", "spa": "es", "esp": "es",
	"fre": "fr", "fra": "fr", "tur": "tr", "hin": "hi", "kor": "ko", "chi": "zh", "rus": "ru", "por": "pt",
}

var languageCodes = map[string]Language{
	"de": "de", "en": "en", "ja": "ja", "jp": "ja", "es": "es", "it": "it", "fr": "fr", "tr": "tr",
	"hi": "hi", "ko": "ko", "zh": "zh", "ru": "ru", "pt": "pt", "pl": "pl", "nl": "nl",
}

// LanguageNamed reads a language as indexers and TVDB write it: an English
// name ("German", "Japanese (Japan)"), an ISO 639 code ("deu", "de") or a
// locale ("en-US").
func LanguageNamed(s string) (Language, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, " (-_"); i > 0 {
		s = s[:i]
	}
	if l, ok := languages[s]; ok {
		return l, true
	}
	l, ok := languageCodes[s]
	return l, ok
}

var languageNames = map[Language]string{
	"de": "German", "en": "English", "ja": "Japanese", "es": "Spanish", "it": "Italian", "fr": "French",
	"tr": "Turkish", "hi": "Hindi", "ko": "Korean", "zh": "Chinese", "ru": "Russian", "pt": "Portuguese",
	"pl": "Polish", "nl": "Dutch",
}

// Name is the language's English name, such as "German".
func (l Language) Name() string {
	if n, ok := languageNames[l]; ok {
		return n
	}
	return string(l)
}

var subtitleLanguages = map[string]Language{
	"esub": "en", "esubs": "en", "engsub": "en", "engsubs": "en", "gersub": "de", "gersubs": "de", "vostfr": "fr",
	"uksub": "en", "uksubs": "en", "nlsub": "nl", "nlsubs": "nl", "subfrench": "fr",
}

var markers = map[string]Marker{
	"dual": DualAudio, "dual-audio": DualAudio, "dualaudio": DualAudio,
	"multi": MultiAudio, "ml": MultiAudio, "multi-audio": MultiAudio, "multiaudio": MultiAudio,
	"multisub": MultiSubs, "multisubs": MultiSubs, "multi-sub": MultiSubs, "multi-subs": MultiSubs,
	"msub": MultiSubs, "msubs": MultiSubs,
}

var (
	subWords     = set("sub", "subs", "subbed", "subtitle", "subtitles", "subtitled")
	dubWords     = set("dub", "dubbed")
	episodeWords = set("ep", "episode", "folge")
	seasonWords  = set("season", "staffel", "saison")
	specials     = set("ncop", "nced", "nc", "ova", "oad", "sp")
)

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

func isSeparator(c byte) bool {
	return c == ' ' || c == '.' || c == '_' || c == ',' || c < 0x20 || c == 0x7f
}

func isBracket(c byte) bool {
	return strings.IndexByte("[](){}", c) >= 0
}

// tokenize splits a name on separators and brackets. Dots inside decimals
// such as 18.5 or codecs such as H.264 do not split. Hyphenated words are split
// unless they are known tags, and the hyphen is recorded on the following part.
func tokenize(s string) []token {
	keep := make([]bool, len(s))
	for _, re := range []*regexp.Regexp{reDecimal, reCodecDot, reEpisodeDot} {
		for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
			for i := m[2]; i < m[3]; i++ {
				keep[i] = true
			}
		}
	}

	var raw []token
	start := -1
	flush := func(end int) {
		if start >= 0 {
			raw = append(raw, token{text: s[start:end], start: start, end: end})
			start = -1
		}
	}
	for i := 0; i < len(s); {
		c, size := s[i], 1
		unsafe := false
		if c >= utf8.RuneSelf {
			var r rune
			r, size = utf8.DecodeRuneInString(s[i:])
			// Control and format characters (such as direction overrides) and
			// invalid UTF-8 must never reach titles shown in a terminal.
			unsafe = unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (r == utf8.RuneError && size == 1)
		}
		switch {
		case c == '.' && keep[i]:
			if start < 0 {
				start = i
			}
		case unsafe || isSeparator(c):
			flush(i)
		case isBracket(c):
			flush(i)
			raw = append(raw, token{text: s[i : i+1], start: i, end: i + 1})
		default:
			if start < 0 {
				start = i
			}
		}
		i += size
	}
	flush(len(s))
	raw = splitGluedResolutions(raw)

	var out []token
	pending := noDash
	for _, t := range raw {
		if strings.Trim(t.text, "-~") == "" {
			pending = spacedDash
			continue
		}
		if !strings.Contains(t.text, "-") || hyphenated[strings.ToLower(t.text)] {
			t.dash = pending
			pending = noDash
			out = append(out, t)
			continue
		}
		if t.text[0] == '-' {
			pending = spacedDash
		}
		var parts []token
		offset := t.start
		for _, part := range strings.Split(t.text, "-") {
			if part != "" {
				parts = append(parts, token{text: part, start: offset, end: offset + len(part), dash: gluedDash})
			}
			offset += len(part) + 1
		}
		// Keep known tags whole inside longer names, as in WEB-DL-GROUP.
		for i := 0; i < len(parts); i++ {
			for j := min(len(parts), i+3); j > i+1; j-- {
				if text := t.text[parts[i].start-t.start : parts[j-1].end-t.start]; hyphenated[strings.ToLower(text)] {
					parts[i].text, parts[i].end = text, parts[j-1].end
					parts = append(parts[:i+1], parts[j:]...)
					break
				}
			}
		}
		if len(parts) > 0 {
			parts[0].dash = pending
		}
		out = append(out, parts...)
		pending = noDash
	}

	for i := range out {
		classify(&out[i])
	}
	resolvePairs(out)
	return out
}

// splitGluedResolutions separates "720pWEB-DL" into "720p" and "WEB-DL".
func splitGluedResolutions(raw []token) []token {
	out := make([]token, 0, len(raw))
	for _, t := range raw {
		if m := reGluedResolution.FindStringSubmatchIndex(t.text); m != nil {
			if _, ok := resolution(t.text[m[2]:m[3]-1], ""); ok {
				cut := t.start + m[3]
				out = append(out, token{text: t.text[:m[3]], start: t.start, end: cut},
					token{text: t.text[m[3]:], start: cut, end: t.end})
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

func classify(t *token) {
	lower := strings.ToLower(t.text)
	if t.text == "[" || t.text == "(" || t.text == "{" {
		t.kind = kOpen
		return
	}
	if t.text == "]" || t.text == ")" || t.text == "}" {
		t.kind = kClose
		return
	}
	if t.text == "+" {
		t.kind = kPlus
		return
	}
	if reNumber.MatchString(t.text) {
		t.num, _ = strconv.Atoi(t.text)
		t.kind = kNumber
		if len(t.text) == 4 && t.num >= 1900 && t.num <= 2099 {
			t.kind = kYear
		}
		return
	}
	if m := reDecimalToken.FindStringSubmatch(t.text); m != nil {
		t.num, _ = strconv.Atoi(m[1])
		t.frac, _ = strconv.Atoi(m[2])
		t.kind = kDecimal
		return
	}
	if m := reNumberVersion.FindStringSubmatch(t.text); m != nil {
		t.num, _ = strconv.Atoi(m[1])
		t.frac, _ = strconv.Atoi(m[2])
		t.kind = kNumberVersion
		return
	}
	if m := reVersion.FindStringSubmatch(t.text); m != nil {
		t.num, _ = strconv.Atoi(m[1])
		t.kind = kVersion
		return
	}
	if m := reResolution.FindStringSubmatch(t.text); m != nil {
		if r, ok := resolution(m[1], ""); ok {
			t.num, t.kind = r, kResolution
			return
		}
	}
	if m := reDimensions.FindStringSubmatch(t.text); m != nil {
		if r, ok := resolution(m[2], m[1]); ok {
			t.num, t.kind = r, kResolution
			return
		}
	}
	switch lower {
	case "4k", "uhd":
		t.num, t.kind = 2160, kResolution
		return
	}
	if s, ok := sources[lower]; ok {
		t.source, t.kind = s, kSource
		t.service = services[lower] // NetflixHD
		if lower == "bdremux" {
			t.flag = Remux
		}
		return
	}
	if f, ok := flags[lower]; ok {
		t.flag, t.kind = f, kFlag
		return
	}
	switch {
	case reSeasonEpisode.MatchString(t.text):
		t.kind = kSeasonEpisode
	case reSeason.MatchString(t.text):
		t.kind = kSeason
	case reEpisode.MatchString(t.text):
		t.kind = kEpisode
	case reCross.MatchString(t.text):
		t.kind = kCross
	case reOrdinal.MatchString(t.text):
		t.kind = kOrdinal
	case reSpecial.MatchString(t.text), specials[lower]:
		t.kind = kSpecial
	case video[lower]:
		t.kind = kVideo
	case reAudioCodec.MatchString(t.text):
		t.kind = kAudioCodec
	case services[lower] != "":
		t.kind, t.service = kService, services[lower]
	case ignored[lower], ignoredExact[t.text]:
		t.kind = kIgnore
	case lower == "dl":
		t.kind = kDL
	case subWords[lower]:
		t.kind = kSubWord
	case dubWords[lower]:
		t.kind = kDubWord
	case episodeWords[lower]:
		t.kind = kEpWord
	case seasonWords[lower]:
		t.kind = kSeasonWord
	default:
		if l, ok := languages[lower]; ok {
			t.lang, t.kind = l, kLanguage
		} else if l, ok := languageCodes[lower]; ok {
			t.lang, t.kind = l, kLangCode
		} else if l, ok := subtitleLanguages[lower]; ok {
			t.lang, t.kind = l, kSubLang
		} else if m, ok := markers[lower]; ok {
			t.marker, t.kind = m, kMarker
		} else {
			t.kind = kWord
		}
	}
}

// resolvePairs merges two-token tags such as WEB.DL or Dual.Audio.
func resolvePairs(toks []token) {
	for i := 0; i+1 < len(toks); i++ {
		t, next := &toks[i], &toks[i+1]
		lower, nextLower := strings.ToLower(t.text), strings.ToLower(next.text)
		switch {
		case t.kind == kSource && t.source == Web && nextLower == "dl":
			t.source, next.kind = WebDL, kIgnore
		case t.kind == kSource && t.source == Web && nextLower == "rip":
			t.source, next.kind = WebRip, kIgnore
		case t.kind == kMarker && lower == "multi" && next.kind == kSubWord:
			t.marker, next.kind = MultiSubs, kIgnore
		// "(Mini Anime)" marks short spin-off episodes, specials on TVDB.
		case lower == "mini" && nextLower == "anime":
			t.kind, next.kind = kSpecial, kIgnore
		// "S2-Extra EP" is a special, not the season.
		case lower == "extra" && (nextLower == "ep" || i > 0 && toks[i-1].kind == kSeason && t.dash == gluedDash):
			t.kind = kSpecial
			if nextLower == "ep" {
				next.kind = kIgnore
			}
		}
	}
}

func resolution(height, width string) (int, bool) {
	h, _ := strconv.Atoi(height)
	switch h {
	case 360, 480, 540, 576, 720, 1080, 2160:
		return h, true
	}
	if width != "" {
		switch w, _ := strconv.Atoi(width); w {
		case 1280:
			return 720, true
		case 1920:
			return 1080, true
		case 3840:
			return 2160, true
		}
	}
	return 0, false
}
