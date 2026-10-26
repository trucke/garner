package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const minimal = `garner_profile = 1
name = "Harbor fans"
audio = ["de", "en"]
resolution = ["1080p", "720p"]
`

// TestShipped reads the profiles that come with Garner: each is valid,
// without warnings, and in the canonical form Encode writes, so it can
// serve as an example as it is.
func TestShipped(t *testing.T) {
	ps := Shipped()
	if len(ps) != len(shippedOrder) {
		t.Fatalf("%d shipped profiles, want %d", len(ps), len(shippedOrder))
	}
	for i, name := range shippedOrder {
		b, err := shippedFiles.ReadFile("shipped/" + name + ".toml")
		if err != nil {
			t.Fatal(err)
		}
		_, warnings, err := Parse(string(b))
		if err != nil || len(warnings) > 0 {
			t.Errorf("%s: %v, warnings %v", name, err, warnings)
		}
		if got := Encode(ps[i]); got != string(b) {
			t.Errorf("%s is not in canonical form; Encode writes:\n%s", name, got)
		}
	}
}

// TestDefaults reads a profile that sets only the required keys.
func TestDefaults(t *testing.T) {
	p, warnings, err := Parse(minimal)
	if err != nil || len(warnings) > 0 {
		t.Fatalf("%v, warnings %v", err, warnings)
	}
	want := Profile{Name: "Harbor fans", Audio: []string{"de", "en"}, Resolution: []int{1080, 720},
		Priority: ByAudio, DualAudio: PreferDual, UnnamedAudio: RejectUnnamed, Upgrades: true}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("got %+v, want %+v", p, want)
	}
}

// TestChoices ranks the preferences by the list that priority names first.
func TestChoices(t *testing.T) {
	p, _, err := Parse(minimal)
	if err != nil {
		t.Fatal(err)
	}
	for priority, want := range map[Priority]string{
		ByAudio:      "German 1080p, German 720p, English 1080p, English 720p",
		ByResolution: "German 1080p, English 1080p, German 720p, English 720p",
	} {
		p.Priority = priority
		var got []string
		for _, c := range p.Preferences() {
			got = append(got, c.String())
		}
		if strings.Join(got, ", ") != want {
			t.Errorf("priority %s: %s, want %s", priority, strings.Join(got, ", "), want)
		}
	}
	if got := (Preference{Original, 720}).String(); got != "Original 720p" {
		t.Errorf("original: %q", got)
	}
}

// TestProblems breaks one rule at a time. Every message names the key and
// says what would be right.
func TestProblems(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{"syntax", "garner_profile = 1\nname = \"x\n", "line 2:"},
		{"too large", minimal + "# " + strings.Repeat("x", MaxSize), "more than 64 KiB"},
		{"no version", strings.Replace(minimal, "garner_profile = 1\n", "", 1), "garner_profile is missing; set garner_profile = 1"},
		{"newer version", strings.Replace(minimal, "= 1", "= 2", 1), "newer format than this Garner reads (1); update Garner"},
		{"older version", strings.Replace(minimal, "= 1", "= 0", 1), "garner_profile must be 1"},
		{"version as text", strings.Replace(minimal, "= 1", `= "1"`, 1), "garner_profile must be the number 1"},
		{"unknown key", minimal + "tiers = [\"de 1080p\"]\n", `unknown key "tiers"; a profile has garner_profile, name,`},
		{"table", minimal + "[extra]\nx = 1\n", `unknown key "extra"`},
		{"no name", strings.Replace(minimal, "name = \"Harbor fans\"\n", "", 1), "name is missing"},
		{"empty name", strings.Replace(minimal, `"Harbor fans"`, `"  "`, 1), "name must not be empty"},
		{"long name", strings.Replace(minimal, `"Harbor fans"`, `"`+strings.Repeat("h", MaxName+1)+`"`, 1), "name has 41 characters, more than 40"},
		{"two-line name", strings.Replace(minimal, `"Harbor fans"`, `"Harbor\nfans"`, 1), "name must be one line of text"},
		{"name as number", strings.Replace(minimal, `"Harbor fans"`, `7`, 1), "name must be a text in quotes"},
		{"long description", minimal + `description = "` + strings.Repeat("d", MaxDescription+1) + "\"\n", "description has 201 characters, more than 200"},
		{"no audio", strings.Replace(minimal, "audio = [\"de\", \"en\"]\n", "", 1), `audio is missing; for example audio = ["de", "en"]`},
		{"audio as text", strings.Replace(minimal, `["de", "en"]`, `"de"`, 1), `audio must be a list, such as audio = ["de", "en"]`},
		{"empty audio", strings.Replace(minimal, `["de", "en"]`, `[]`, 1), "audio must name at least one entry"},
		{"audio name", strings.Replace(minimal, `["de", "en"]`, `["German"]`, 1), `audio: write "de" instead of "German"`},
		{"audio upper case", strings.Replace(minimal, `["de", "en"]`, `["DE"]`, 1), `audio: write "de" instead of "DE"`},
		{"unknown audio", strings.Replace(minimal, `["de", "en"]`, `["xx"]`, 1), `audio: "xx" is not a language Garner knows; use original (the show's own), de (German), en (English),`},
		{"repeated audio", strings.Replace(minimal, `["de", "en"]`, `["de", "de"]`, 1), `audio names "de" twice`},
		{"audio numbers", strings.Replace(minimal, `["de", "en"]`, `[1, 2]`, 1), "audio must list texts in quotes"},
		{"2160p", strings.Replace(minimal, `["1080p", "720p"]`, `["2160p"]`, 1), `resolution: "2160p" is not supported yet; use 1080p or 720p`},
		{"bare number", strings.Replace(minimal, `["1080p", "720p"]`, `["1080"]`, 1), `resolution: "1080" is not allowed; use 1080p or 720p`},
		{"phrase", minimal + "priority = \"audio first\"\n", `priority: "audio first" is not allowed; use audio or resolution`},
		{"dual audio", minimal + "dual_audio = \"require\"\n", `dual_audio: "require" is not allowed; use prefer or ignore`},
		{"unnamed audio", minimal + "unnamed_audio = true\n", "unnamed_audio: true is not allowed; use reject or fallback"},
		{"upgrades as text", minimal + "upgrades = \"yes\"\n", `upgrades: "yes" is not allowed; use true or false`},
	} {
		_, _, err := Parse(tc.text)
		var pe *Error
		if !errors.As(err, &pe) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// TestAllProblems reports every problem at once, so a user fixes a shared
// profile in one go.
func TestAllProblems(t *testing.T) {
	_, _, err := Parse("garner_profile = 1\naudio = [\"German\"]\nresolution = [\"4k\"]\npriority = \"both\"\n")
	var pe *Error
	if !errors.As(err, &pe) || len(pe.Problems) != 4 {
		t.Fatalf("%v, want four problems: name, audio, resolution and priority", err)
	}
}

// TestWarnings names settings changed from their default that have no
// effect. A default written out, as Encode does, never warns.
func TestWarnings(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
	}{
		{minimal, nil},
		{minimal + "priority = \"resolution\"\n", nil},
		{strings.Replace(minimal, `["de", "en"]`, `["de"]`, 1) + "priority = \"audio\"\n", nil},
		{strings.Replace(minimal, `["de", "en"]`, `["original"]`, 1) + "dual_audio = \"prefer\"\n", nil},
		{strings.Replace(minimal, `["de", "en"]`, `["de"]`, 1) + "priority = \"resolution\"\n",
			[]string{"priority has no effect while audio or resolution names a single entry"}},
		{strings.Replace(minimal, `["de", "en"]`, `["original"]`, 1) + "dual_audio = \"ignore\"\n",
			[]string{"dual_audio has no effect while audio is only original"}},
	} {
		_, got, err := Parse(tc.text)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%q: %v, %v; want %v", tc.text, got, err, tc.want)
		}
	}
}

// TestSchema keeps docs/profile.schema.json, which editors check profiles
// with, in line with what Parse accepts.
func TestSchema(t *testing.T) {
	b, err := os.ReadFile("../../docs/profile.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		ID         string   `json:"$id"`
		Required   []string `json:"required"`
		Additional bool     `json:"additionalProperties"`
		Properties map[string]struct {
			Const     *int  `json:"const"`
			Enum      []any `json:"enum"`
			MaxLength int   `json:"maxLength"`
			Default   any   `json:"default"`
			Items     struct {
				Enum []string `json:"enum"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	props := s.Properties
	check := func(what string, got, want any) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("schema %s: %v, want %v", what, got, want)
		}
	}
	check("$id", s.ID, SchemaURL)
	check("additionalProperties", s.Additional, false)
	check("keys", slices.Sorted(func(yield func(string) bool) {
		for k := range props {
			if !yield(k) {
				return
			}
		}
	}), slices.Sorted(slices.Values(keys)))
	check("required", s.Required, []string{"garner_profile", "name", "audio", "resolution"})
	if c := props["garner_profile"].Const; c == nil || *c != Version {
		t.Errorf("schema garner_profile: const %v, want %d", c, Version)
	}
	check("name maxLength", props["name"].MaxLength, MaxName)
	check("description maxLength", props["description"].MaxLength, MaxDescription)
	check("audio", props["audio"].Items.Enum, append([]string{Original}, Languages()...))
	check("resolution", props["resolution"].Items.Enum, []string{"1080p", "720p"})
	check("priority", props["priority"].Enum, []any{ByAudio, ByResolution})
	check("dual_audio", props["dual_audio"].Enum, []any{PreferDual, IgnoreDual})
	check("unnamed_audio", props["unnamed_audio"].Enum, []any{RejectUnnamed, FallbackUnnamed})
	d, _, _ := Parse(minimal)
	check("priority default", props["priority"].Default, d.Priority)
	check("dual_audio default", props["dual_audio"].Default, d.DualAudio)
	check("unnamed_audio default", props["unnamed_audio"].Default, d.UnnamedAudio)
	check("upgrades default", props["upgrades"].Default, d.Upgrades)
}

// FuzzParse feeds Parse arbitrary text. It never panics, and a profile it
// accepts reads back as the same profile from its canonical form.
func FuzzParse(f *testing.F) {
	f.Add(minimal)
	for _, name := range shippedOrder {
		b, _ := shippedFiles.ReadFile("shipped/" + name + ".toml")
		f.Add(string(b))
	}
	f.Add(minimal + "priority = \"resolution\"\nupgrades = false\ndescription = \"x\"\n")
	f.Fuzz(func(t *testing.T, text string) {
		p, _, err := Parse(text)
		if err != nil {
			return
		}
		if n := len(p.Preferences()); n == 0 || n != len(p.Audio)*len(p.Resolution) {
			t.Fatalf("%d preferences from %d languages and %d resolutions", n, len(p.Audio), len(p.Resolution))
		}
		out := Encode(p)
		again, _, err := Parse(out)
		if err != nil {
			t.Fatalf("the canonical form does not parse: %v\n%s", err, out)
		}
		if !reflect.DeepEqual(again, p) {
			t.Fatalf("read back as %+v, want %+v", again, p)
		}
		if Encode(again) != out {
			t.Fatalf("the canonical form is not stable")
		}
	})
}
