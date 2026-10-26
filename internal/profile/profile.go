// Package profile reads, checks and writes release profiles: the audio
// languages and resolutions a show accepts, best first, and how Garner
// ranks and upgrades releases within them. A profile is a short TOML text
// that users can share; docs/profiles.md describes the format.
//
// The package holds no server code, so the terminal interface can check
// profiles without a server.
package profile

import (
	"cmp"
	"embed"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

const (
	// Version is the format version this Garner reads and writes.
	Version = 1
	// MaxSize bounds a profile's text; real ones are a few hundred bytes.
	MaxSize = 64 << 10
	// MaxName and MaxDescription bound the texts that lists show.
	MaxName        = 40
	MaxDescription = 200

	// SchemaURL is the JSON schema that editors check profiles with.
	SchemaURL = "https://raw.githubusercontent.com/trucke/garner/main/docs/profile.schema.json"
	// DocsURL describes the format.
	DocsURL = "https://github.com/trucke/garner/blob/main/docs/profiles.md"
)

// Original stands for the show's original language in Audio.
const Original = "original"

// languages are the audio languages a profile may name, by ISO 639-1 code.
// The release parser knows the same ones; a test in package selection
// keeps the two lists equal.
var languages = map[string]string{
	"de": "German", "en": "English", "es": "Spanish", "fr": "French", "hi": "Hindi", "it": "Italian",
	"ja": "Japanese", "ko": "Korean", "nl": "Dutch", "pl": "Polish", "pt": "Portuguese", "ru": "Russian",
	"tr": "Turkish", "zh": "Chinese",
}

// Languages lists the language codes a profile may name besides Original,
// in alphabetical order.
func Languages() []string { return slices.Sorted(maps.Keys(languages)) }

// LanguageName names a code of Audio, such as "German"; Original is
// "Original".
func LanguageName(code string) string {
	if code == Original {
		return "Original"
	}
	return cmp.Or(languages[code], code)
}

// Resolutions are the resolutions a profile may name, best first.
var Resolutions = []int{1080, 720}

// Priority says which list counts more when Garner ranks the preferences.
type Priority string

const (
	ByAudio      Priority = "audio"
	ByResolution Priority = "resolution"
)

// DualAudio says whether a release with the show's original audio besides
// the preference's language ranks first.
type DualAudio string

const (
	PreferDual DualAudio = "prefer"
	IgnoreDual DualAudio = "ignore"
)

// UnnamedAudio says what Garner does with a release marked dual, multi or
// dubbed that names no languages.
type UnnamedAudio string

const (
	RejectUnnamed   UnnamedAudio = "reject"
	FallbackUnnamed UnnamedAudio = "fallback"
)

// Profile is one profile, as Parse reads it.
type Profile struct {
	Name        string
	Description string
	// Audio are language codes or Original, best first.
	Audio []string
	// Resolution are vertical resolutions, such as 1080, best first.
	Resolution   []int
	Priority     Priority
	DualAudio    DualAudio
	UnnamedAudio UnnamedAudio
	// Upgrades has Garner replace a file below the first preference when a
	// release in a better preference appears.
	Upgrades bool
}

// Preference is one acceptable audio language and resolution.
type Preference struct {
	Audio      string
	Resolution int
}

func (c Preference) String() string {
	return fmt.Sprintf("%s %dp", LanguageName(c.Audio), c.Resolution)
}

// Preferences lists the profile's preferences, best first: every audio
// language with every resolution, the list that Priority names first.
func (p Profile) Preferences() []Preference {
	var out []Preference
	if p.Priority == ByResolution {
		for _, r := range p.Resolution {
			for _, a := range p.Audio {
				out = append(out, Preference{a, r})
			}
		}
		return out
	}
	for _, a := range p.Audio {
		for _, r := range p.Resolution {
			out = append(out, Preference{a, r})
		}
	}
	return out
}

// keys are a profile's keys, in the order Encode writes them.
var keys = []string{"garner_profile", "name", "description", "audio", "resolution", "priority", "dual_audio", "unnamed_audio", "upgrades"}

// Error lists everything wrong with a profile's text, each with the key it
// concerns.
type Error struct{ Problems []string }

func (e *Error) Error() string { return strings.Join(e.Problems, "; ") }

// Parse reads a profile's text strictly. It returns the profile, warnings
// about settings without effect, and an *Error that lists every problem.
func Parse(text string) (Profile, []string, error) {
	if len(text) > MaxSize {
		return Profile{}, nil, &Error{[]string{fmt.Sprintf("the profile has %d bytes, more than %d KiB", len(text), MaxSize>>10)}}
	}
	var raw map[string]any
	if _, err := toml.Decode(text, &raw); err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return Profile{}, nil, &Error{[]string{fmt.Sprintf("line %d: %s", pe.Position.Line, pe.Message)}}
		}
		return Profile{}, nil, &Error{[]string{err.Error()}}
	}
	r := reader{raw: raw}
	for _, k := range slices.Sorted(maps.Keys(raw)) {
		if !slices.Contains(keys, k) {
			r.fail("unknown key %q; a profile has %s", k, strings.Join(keys, ", "))
		}
	}
	r.version()
	p := Profile{
		Name:         r.text("name", true, MaxName),
		Description:  r.text("description", false, MaxDescription),
		Audio:        r.audio(),
		Resolution:   r.resolution(),
		Priority:     Priority(r.word("priority", string(ByAudio), ByAudio, ByResolution)),
		DualAudio:    DualAudio(r.word("dual_audio", string(PreferDual), PreferDual, IgnoreDual)),
		UnnamedAudio: UnnamedAudio(r.word("unnamed_audio", string(RejectUnnamed), RejectUnnamed, FallbackUnnamed)),
		Upgrades:     r.boolean("upgrades", true),
	}
	if len(r.problems) > 0 {
		return Profile{}, nil, &Error{r.problems}
	}
	// Warn about a setting changed from its default to no effect: whoever
	// changed it meant something. Encode writes every key, defaults too, so
	// a default alone never warns.
	var warnings []string
	if p.Priority != ByAudio && (len(p.Audio) == 1 || len(p.Resolution) == 1) {
		warnings = append(warnings, "priority has no effect while audio or resolution names a single entry")
	}
	if p.DualAudio != PreferDual && slices.Equal(p.Audio, []string{Original}) {
		warnings = append(warnings, "dual_audio has no effect while audio is only original")
	}
	return p, warnings, nil
}

// reader takes the keys of a decoded profile apart and collects problems.
type reader struct {
	raw      map[string]any
	problems []string
}

func (r *reader) fail(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

func (r *reader) version() {
	v, ok := r.raw["garner_profile"]
	if !ok {
		r.fail("garner_profile is missing; set garner_profile = %d", Version)
		return
	}
	n, ok := v.(int64)
	switch {
	case !ok:
		r.fail("garner_profile must be the number %d", Version)
	case n > Version:
		r.fail("garner_profile = %d is a newer format than this Garner reads (%d); update Garner", n, Version)
	case n != Version:
		r.fail("garner_profile must be %d", Version)
	}
}

func (r *reader) text(key string, required bool, limit int) string {
	v, ok := r.raw[key]
	if !ok {
		if required {
			r.fail("%s is missing", key)
		}
		return ""
	}
	s, ok := v.(string)
	if !ok {
		r.fail("%s must be a text in quotes", key)
		return ""
	}
	s = strings.TrimSpace(s)
	switch {
	case required && s == "":
		r.fail("%s must not be empty", key)
	case strings.ContainsFunc(s, unicode.IsControl):
		r.fail("%s must be one line of text", key)
	case utf8.RuneCountInString(s) > limit:
		r.fail("%s has %d characters, more than %d", key, utf8.RuneCountInString(s), limit)
	}
	return s
}

// list reads a required list of texts without repeats.
func (r *reader) list(key, example string) []string {
	v, ok := r.raw[key]
	if !ok {
		r.fail("%s is missing; for example %s = %s", key, key, example)
		return nil
	}
	items, ok := v.([]any)
	if !ok {
		r.fail("%s must be a list, such as %s = %s", key, key, example)
		return nil
	}
	if len(items) == 0 {
		r.fail("%s must name at least one entry", key)
		return nil
	}
	var out []string
	for _, item := range items {
		s, ok := item.(string)
		switch {
		case !ok:
			r.fail("%s must list texts in quotes, such as %s = %s", key, key, example)
			return nil
		case slices.Contains(out, s):
			r.fail("%s names %q twice", key, s)
		default:
			out = append(out, s)
		}
	}
	return out
}

func (r *reader) audio() []string {
	codes := r.list("audio", `["de", "en"]`)
	allowed := append([]string{Original}, Languages()...)
	for _, c := range codes {
		if slices.Contains(allowed, c) {
			continue
		}
		if code, ok := codeFor(c); ok {
			r.fail("audio: write %q instead of %q", code, c)
			continue
		}
		named := []string{Original + " (the show's own)"}
		for _, l := range Languages() {
			named = append(named, fmt.Sprintf("%s (%s)", l, languages[l]))
		}
		r.fail("audio: %q is not a language Garner knows; use %s", c, strings.Join(named, ", "))
	}
	return codes
}

// codeFor finds the code of a language written another way, such as
// "German" or "DE".
func codeFor(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if _, ok := languages[s]; ok {
		return s, true
	}
	for code, name := range languages {
		if strings.ToLower(name) == s {
			return code, true
		}
	}
	return "", false
}

func (r *reader) resolution() []int {
	var out []int
	for _, s := range r.list("resolution", `["1080p", "720p"]`) {
		i := slices.IndexFunc(Resolutions, func(res int) bool { return fmt.Sprintf("%dp", res) == s })
		switch {
		case i >= 0:
			out = append(out, Resolutions[i])
		case strings.EqualFold(s, "2160p") || strings.EqualFold(s, "4k"):
			r.fail("resolution: %q is not supported yet; use 1080p or 720p", s)
		default:
			r.fail("resolution: %q is not allowed; use 1080p or 720p", s)
		}
	}
	return out
}

// word reads one of a few allowed words, or the default when the key is
// missing.
func (r *reader) word(key, def string, allowed ...any) string {
	v, ok := r.raw[key]
	if !ok {
		return def
	}
	words := make([]string, len(allowed))
	for i, a := range allowed {
		words[i] = fmt.Sprint(a)
	}
	s, ok := v.(string)
	if !ok || !slices.Contains(words, s) {
		r.fail("%s: %s is not allowed; use %s", key, show(v), strings.Join(words, " or "))
		return def
	}
	return s
}

func (r *reader) boolean(key string, def bool) bool {
	v, ok := r.raw[key]
	if !ok {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		r.fail("%s: %s is not allowed; use true or false", key, show(v))
		return def
	}
	return b
}

// show writes a decoded value as the profile had it.
func show(v any) string {
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprint(v)
}

// file is a profile as Encode writes it, in the order of keys.
type file struct {
	Version      int      `toml:"garner_profile"`
	Name         string   `toml:"name"`
	Description  string   `toml:"description,omitempty"`
	Audio        []string `toml:"audio"`
	Resolution   []string `toml:"resolution"`
	Priority     string   `toml:"priority"`
	DualAudio    string   `toml:"dual_audio"`
	UnnamedAudio string   `toml:"unnamed_audio"`
	Upgrades     bool     `toml:"upgrades"`
}

// Encode writes the profile in its canonical form, with every key and a
// header that points editors at the schema. Parse reads it back as the
// same profile.
func Encode(p Profile) string {
	res := make([]string, len(p.Resolution))
	for i, r := range p.Resolution {
		res[i] = fmt.Sprintf("%dp", r)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#:schema %s\n# A Garner profile; %s describes the format.\n", SchemaURL, DocsURL)
	err := toml.NewEncoder(&b).Encode(file{
		Version: Version, Name: p.Name, Description: p.Description, Audio: p.Audio, Resolution: res,
		Priority: string(p.Priority), DualAudio: string(p.DualAudio), UnnamedAudio: string(p.UnnamedAudio), Upgrades: p.Upgrades,
	})
	if err != nil {
		panic(fmt.Sprintf("profile: encoding %q cannot fail, but did: %v", p.Name, err))
	}
	return b.String()
}

//go:embed shipped/*.toml
var shippedFiles embed.FS

// shippedOrder is the order in which lists offer the shipped profiles.
var shippedOrder = []string{"original-language", "german", "german-english-fallback", "english", "original-english-dub"}

// Shipped returns the profiles that come with Garner, in the order lists
// offer them. Each is a file in shipped/ in its canonical form.
func Shipped() []Profile {
	out := make([]Profile, len(shippedOrder))
	for i, name := range shippedOrder {
		b, err := shippedFiles.ReadFile("shipped/" + name + ".toml")
		if err != nil {
			panic(fmt.Sprintf("profile: shipped/%s.toml is missing: %v", name, err))
		}
		p, _, err := Parse(string(b))
		if err != nil {
			panic(fmt.Sprintf("profile: shipped/%s.toml is invalid: %v", name, err))
		}
		out[i] = p
	}
	return out
}
