package importer

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/inspect"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/selection"
	"github.com/trucke/garner/internal/store"
)

// maxName keeps file names well below the 255-byte limit of most
// filesystems and SMB. The show name and the release details are capped,
// so the episode title always has room left or gives way.
const (
	maxName = 200
	maxShow = 80
	maxTags = 80
)

// target names a file in the library the way Jellyfin reads it, with only
// letters, digits, dots and hyphens, plus brackets around the TVDB ID:
//
//	Show.2013.[tvdbid-1]/Season.01/Show.2013.S01E01.Title.Bluray-1080p.DE.EN-Group.mkv
//
// tags are the release details from releaseTags.
func (c showContext) target(lib config.Library, episodes []int64, tags, ext string) (string, error) {
	folder, err := ShowFolder(lib, c.data.Series, c.profile)
	if err != nil {
		return "", err
	}
	name := showName(c.data.Series)

	first := c.episodes[episodes[0]]
	pos := first.Numbers["official"]
	number := fmt.Sprintf("S%02dE%02d", pos.Season, pos.Number)
	if len(episodes) > 1 {
		if last := c.episodes[episodes[len(episodes)-1]].Numbers["official"]; last.Season == pos.Season {
			number += fmt.Sprintf("-E%02d", last.Number)
		}
	}
	ext = strings.ToLower(ext)
	tail := cut(tags, maxTags)
	if tail != "" && !strings.HasPrefix(tail, "-") {
		tail = "." + tail
	}
	file := name + "." + number
	// The episode title is the part that gives way when the name gets long.
	if title := dotted(cmp.Or(first.NameEng, first.Name)); title != "" {
		room := maxName - len(file) - len(tail) - len(ext) - 1
		file += "." + cut(title, room)
	}
	file = strings.TrimSuffix(file, ".") + tail + ext
	return filepath.Join(folder, fmt.Sprintf("Season.%02d", pos.Season), file), nil
}

// ShowFolder is the folder new files of a show go to, as Garner reaches it:
//
//	<library root>/Show.2013.[tvdbid-1]
func ShowFolder(lib config.Library, sr store.Series, profile selection.Profile) (string, error) {
	root := lib.TV
	if profile.Name == selection.Anime.Name {
		root = lib.Anime
	}
	if root == "" {
		return "", fmt.Errorf("no library folder is set for the %s profile", profile.Name)
	}
	return filepath.Join(filepath.Clean(root), fmt.Sprintf("%s.[tvdbid-%d]", showName(sr), sr.ID)), nil
}

// showName is the show's part of folder and file names, such as
// "Harbor.Precinct.2013".
func showName(sr store.Series) string {
	// A title only in another script, such as Japanese, leaves nothing;
	// TVDB's slug is ASCII.
	name := cut(cmp.Or(dotted(cmp.Or(sr.NameEng, sr.Name)), dotted(sr.Slug), strconv.FormatInt(sr.ID, 10)), maxShow)
	if year := strconv.Itoa(sr.Year); sr.Year > 0 && !strings.HasSuffix(name, "."+year) {
		name += "." + year
	}
	return name
}

// releaseTags are the details a file name keeps because they cannot be
// read from the file later: streaming service, source, proper or repack,
// the audio languages and the release group, as in
// "NF.WEBDL-1080p.Proper.DE.EN-Group". The resolution comes from the file.
func releaseTags(d *store.Download, p inspect.Probe) string {
	c := release.Parse(d.Title)
	var parts []string
	if len(c.Services) > 0 {
		parts = append(parts, dotted(d.Title[c.Services[0].Span.Start:c.Services[0].Span.End]))
	}
	quality := []string{}
	if source, state := release.Value(c.Source); state == release.Unambiguous {
		quality = append(quality, sourceNames[source])
	}
	if len(p.Video) > 0 {
		quality = append(quality, fmt.Sprintf("%dp", inspect.Class(p.Video[0].Width, p.Video[0].Height)))
	}
	parts = append(parts, strings.Join(quality, "-"))
	for _, f := range c.Flags {
		switch f.Value {
		case release.Proper:
			parts = append(parts, "Proper")
		case release.Repack:
			parts = append(parts, "Repack")
		}
	}
	if v, state := release.Value(c.Version); state == release.Unambiguous && v > 1 {
		parts = append(parts, fmt.Sprintf("v%d", v))
	}
	parts = append(parts, audioCodes(d, p)...)
	tags := strings.Join(nonEmpty(parts), ".")
	if group, state := release.Value(c.Group); state == release.Unambiguous && dotted(group) != "" {
		tags += "-" + dotted(group)
	}
	return tags
}

// audioCodes are the file's tagged audio languages, or the language the
// release was chosen for when the file tags none.
func audioCodes(d *store.Download, p inspect.Probe) []string {
	var codes []string
	add := func(l release.Language) {
		if c := strings.ToUpper(string(l)); len(c) == 2 && !slices.Contains(codes, c) {
			codes = append(codes, c)
		}
	}
	for _, t := range p.Audio {
		if l, ok := release.LanguageNamed(t.Language); ok {
			add(l)
		}
	}
	if len(codes) == 0 {
		if f := strings.Fields(d.Tier); len(f) == 2 {
			if l, ok := release.LanguageNamed(f[0]); ok {
				add(l)
			}
		}
	}
	return codes
}

var sourceNames = map[release.Source]string{
	release.BluRay: "Bluray", release.WebDL: "WEBDL", release.WebRip: "WEBRip", release.Web: "WEB",
	release.HDTV: "HDTV", release.TV: "SDTV", release.DVD: "DVD", release.HDRip: "HDRip",
}

// letters are written the way release names write them.
var letters = strings.NewReplacer(
	"ä", "ae", "ö", "oe", "ü", "ue", "Ä", "Ae", "Ö", "Oe", "Ü", "Ue", "ß", "ss",
	"æ", "ae", "Æ", "Ae", "œ", "oe", "Œ", "Oe", "ø", "o", "Ø", "O", "ł", "l", "Ł", "L",
	"đ", "d", "Đ", "D", "ð", "d", "þ", "th", "Þ", "Th", "&", " and ",
)

// dotted turns a title into a name of ASCII letters, digits, dots and
// hyphens: "Glass Orchard: After Winter's End" becomes
// "Glass.Orchard.After.Winters.End". Plain ASCII avoids the different
// Unicode forms macOS and Linux store accented letters in. A hyphen stays
// only inside a word, as in "Twenty-Two".
func dotted(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(letters.Replace(s)) {
		switch {
		case unicode.Is(unicode.Mn, r), r == '\'', r == '’', r == 'ʼ', r == '`':
			// accents and apostrophes
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)), r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('.')
		}
	}
	var words []string
	for _, w := range strings.Split(b.String(), ".") {
		if w = strings.Trim(w, "-"); w != "" {
			words = append(words, w)
		}
	}
	return strings.Join(words, ".")
}

// cut shortens a dotted title to at most n bytes at a word boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	s = s[:n]
	if i := strings.LastIndexByte(s, '.'); i > 0 {
		s = s[:i]
	}
	return strings.Trim(s, ".-")
}

func nonEmpty(parts []string) []string {
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
