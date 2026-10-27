package tui

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/client"
	"github.com/trucke/garner/internal/profile"
)

// Fixtures: the profiles of a small library.
var (
	germanFallback = profile.Profile{Name: "German, English fallback", Description: "German audio; English when no German release exists",
		Audio: []string{"de", "en"}, Resolution: []int{1080, 720}, Priority: profile.ByAudio, DualAudio: profile.IgnoreDual, UnnamedAudio: profile.FallbackUnnamed, Upgrades: true}
	originalDub = profile.Profile{Name: "Original, English dub", Description: "The original audio; an English dub when no release has it",
		Audio: []string{profile.Original, "en"}, Resolution: []int{1080, 720}, Priority: profile.ByAudio, DualAudio: profile.PreferDual, UnnamedAudio: profile.RejectUnnamed, Upgrades: true}
	german = profile.Profile{Name: "German", Description: "German audio; releases with the original audio too rank first",
		Audio: []string{"de"}, Resolution: []int{1080, 720}, Priority: profile.ByAudio, DualAudio: profile.PreferDual, UnnamedAudio: profile.RejectUnnamed, Upgrades: true}
	kids = profile.Profile{Name: "Kids, German only", Description: "German audio only; 720p first to save space",
		Audio: []string{"de"}, Resolution: []int{720, 1080}, Priority: profile.ByResolution, DualAudio: profile.PreferDual, UnnamedAudio: profile.RejectUnnamed, Upgrades: false}
	// subs is not stored.
	subs = profile.Profile{Name: "Subs first", Description: "The original audio at 1080p before any dub",
		Audio: []string{profile.Original, "en"}, Resolution: []int{1080}, Priority: profile.ByResolution, DualAudio: profile.PreferDual, UnnamedAudio: profile.RejectUnnamed, Upgrades: true}
)

func storedProfiles() []api.Profile {
	return []api.Profile{
		{ID: 1, Name: germanFallback.Name, Text: profile.Encode(germanFallback), DefaultTV: true, Shows: []string{"Harbor Precinct", "Copper Valley"}},
		{ID: 2, Name: originalDub.Name, Text: profile.Encode(originalDub), DefaultAnime: true, Shows: []string{"Glass Orchard"}},
		{ID: 3, Name: german.Name, Text: profile.Encode(german), Shows: []string{"Lanterns and Tides"}},
		{ID: 4, Name: kids.Name, Text: profile.Encode(kids)},
	}
}

// profilesApp is an app on the Profiles screen with the fixtures loaded
// and the cursor on the profile with the given ID.
func profilesApp(cl *client.Client, selected int64) (*app, *profilesScreen) {
	sess := &session{client: cl}
	a := &app{sess: sess, library: newLibrary(sess)}
	p := newProfiles(sess, a.library)
	p.set(storedProfiles())
	p.selected = selected
	a.screen = p
	return a, p
}

// run sends keys, then runs the last command and feeds its message back
// to the app, as the program would, and once more for the message's own
// command, such as the switch to another screen.
func run(t *testing.T, a *app, keys ...string) tea.Msg {
	t.Helper()
	cmd := sendKeys(a, keys...)
	if cmd == nil {
		t.Fatalf("%v returned no command", keys)
	}
	msg := cmd()
	if _, next := a.update(msg); next != nil {
		if m := next(); m != nil {
			a.update(m)
		}
	}
	return msg
}

// TestProfilesDelete checks the delete question: a profile that shows use
// or that new shows get is refused without asking the server, Cancel is
// preselected for an unused one, and only Delete asks the server.
func TestProfilesDelete(t *testing.T) {
	rec, cl := newRecorder(t)
	a, p := profilesApp(cl, 1)

	sendKeys(a, "d")
	if p.overlay != deleteRefusedOverlay {
		t.Fatalf("d on a profile in use opened overlay %d, the refusal was expected", p.overlay)
	}
	view := ansi.Strip(p.view(120, 36))
	for _, want := range []string{"German, English fallback can't be deleted", "2 shows use it:", "Harbor Precinct", "Copper Valley", "also the profile for new TV shows", "esc or any key closes"} {
		if !strings.Contains(view, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, view)
		}
	}
	sendKeys(a, "x") // any key closes
	if p.overlay != profilesNoOverlay {
		t.Error("a key did not close the refusal")
	}

	p.selected = 4
	sendKeys(a, "d")
	if p.overlay != deleteProfileOverlay || p.choice != 0 {
		t.Fatalf("after d: overlay %d, choice %d", p.overlay, p.choice)
	}
	if cmd := sendKeys(a, "enter"); cmd != nil || p.overlay != profilesNoOverlay {
		t.Error("enter on Cancel asked the server or left the question open")
	}
	sendKeys(a, "d", "esc")
	if p.overlay != profilesNoOverlay {
		t.Error("esc left the question open")
	}
	if n := len(rec.made("DELETE")); n != 0 {
		t.Fatalf("cancelling asked the server: %v", rec.made("DELETE"))
	}
	run(t, a, "d", "l", "enter")
	if made := rec.made("DELETE"); len(made) != 1 || made[0] != "DELETE /api/v1/profiles/4" {
		t.Errorf("Delete asked %v", made)
	}
}

// TestProfilesUseForNewShows checks that the choice reaches the server as
// the types it names.
func TestProfilesUseForNewShows(t *testing.T) {
	rec, cl := newRecorder(t)
	a, p := profilesApp(cl, 3)
	for _, tc := range []struct {
		keys []string
		want string
	}{
		{[]string{"u", "enter"}, `{"types":["tv"]}`},
		{[]string{"u", "j", "enter"}, `{"types":["anime"]}`},
		{[]string{"u", "j", "j", "enter"}, `{"types":["tv","anime"]}`},
	} {
		run(t, a, tc.keys...)
		if got := rec.body("PUT /api/v1/profiles/3/default"); got != tc.want {
			t.Errorf("%v sent %s, want %s", tc.keys, got, tc.want)
		}
	}
	sendKeys(a, "u", "esc")
	if p.overlay != profilesNoOverlay || len(rec.made("PUT")) != 3 {
		t.Error("esc did not close the question without asking")
	}
}

// TestNewProfilePicker checks what the editor starts from: an empty
// profile first, then a copy of a profile without its name.
func TestNewProfilePicker(t *testing.T) {
	_, cl := newRecorder(t)
	a, p := profilesApp(cl, 1)
	sendKeys(a, "a")
	if p.overlay != newPickerOverlay || p.picker.items[p.picker.choice].label != "Empty profile" {
		t.Fatalf("after a: overlay %d, choice %q", p.overlay, p.picker.items[p.picker.choice].label)
	}
	view := ansi.Strip(p.view(120, 36))
	for _, want := range []string{"New profile", "Your profiles", "Shipped with Garner", "Original language"} {
		if !strings.Contains(view, want) {
			t.Errorf("the picker lacks %q", want)
		}
	}
	a.update(sendKeys(a, "enter")())
	ed, ok := a.screen.(*editorScreen)
	if !ok || ed.id != 0 || len(ed.p.Audio) != 0 || ed.p.Name != "" || ed.p.Priority != profile.ByAudio || !ed.p.Upgrades {
		t.Fatalf("the empty profile opened as %+v", a.screen)
	}

	a.screen = p
	sendKeys(a, "a", "j", "j", "j", "j")
	if label := p.picker.items[p.picker.choice].label; label != "Kids, German only" {
		t.Fatalf("j moved to %q", label)
	}
	a.update(sendKeys(a, "enter")())
	ed, ok = a.screen.(*editorScreen)
	if !ok || ed.id != 0 || ed.p.Name != "" || !slices.Equal(ed.p.Resolution, []int{720, 1080}) || ed.p.Upgrades {
		t.Fatalf("the copy opened as %+v", a.screen)
	}
	if ed.row().kind != rowName {
		t.Error("the editor did not start on the name of a copy")
	}
}

// editorApp opens the stored profile id in the editor.
func editorApp(cl *client.Client, id int64) (*app, *editorScreen) {
	a, p := profilesApp(cl, id)
	r, _ := p.cursor()
	ed := newEditor(p.sess, p, id, r.parsed, len(r.Shows))
	a.screen = ed
	return a, ed
}

// TestEditorLists checks the list keys: a adds after the cursor from the
// languages not listed yet, d removes, K and J move, and the preferences
// follow.
func TestEditorLists(t *testing.T) {
	_, cl := newRecorder(t)
	a, ed := editorApp(cl, 1)
	if ed.row() != (editorRow{rowAudio, 0}) {
		t.Fatalf("the editor started on %+v, the first language was expected", ed.row())
	}
	sendKeys(a, "a")
	if ed.picker == nil {
		t.Fatal("a did not open the language picker")
	}
	var offered []string
	for _, it := range ed.picker.items {
		offered = append(offered, it.label)
	}
	if slices.Contains(offered, "German") || slices.Contains(offered, "English") || offered[0] != "Original language" {
		t.Errorf("the picker offers %v", offered)
	}
	if title := ansi.Strip(ed.pickerTitle()); title != "Add a language  after German, as the 2nd" {
		t.Errorf("picker title %q", title)
	}
	sendKeys(a, "enter")
	if !slices.Equal(ed.p.Audio, []string{"de", profile.Original, "en"}) || ed.row() != (editorRow{rowAudio, 1}) {
		t.Fatalf("after adding: audio %v, cursor %+v", ed.p.Audio, ed.row())
	}
	sendKeys(a, "K")
	if !slices.Equal(ed.p.Audio, []string{profile.Original, "de", "en"}) || ed.row() != (editorRow{rowAudio, 0}) {
		t.Fatalf("after K: audio %v, cursor %+v", ed.p.Audio, ed.row())
	}
	sendKeys(a, "J", "d")
	if !slices.Equal(ed.p.Audio, []string{"de", "en"}) {
		t.Fatalf("after J d: audio %v", ed.p.Audio)
	}
	view := ansi.Strip(ed.view(120, 36))
	if !strings.Contains(view, "German 1080p → German 720p → English 1080p → English 720p") {
		t.Errorf("the preferences pane does not follow the lists:\n%s", view)
	}
	// On a resolution row, a adds a resolution; both are listed already.
	sendKeys(a, "j", "j", "a")
	if ed.picker != nil || ed.row().kind != rowResolution {
		t.Errorf("a on a resolution row with both listed opened a picker; row %+v", ed.row())
	}
	sendKeys(a, "d", "a")
	if ed.picker == nil || ed.picker.items[0].label != "720p" {
		t.Fatal("a after removing 720p did not offer it")
	}
	sendKeys(a, "enter")
	if !slices.Equal(ed.p.Resolution, []int{1080, 720}) {
		t.Errorf("after adding 1080p back: %v", ed.p.Resolution)
	}
	// Without any resolution, adding a language still names the one before.
	sendKeys(a, "d", "d", "k", "a")
	if len(ed.p.Resolution) != 0 || ed.picker == nil {
		t.Fatalf("resolutions %v, picker %v", ed.p.Resolution, ed.picker != nil)
	}
	if title := ansi.Strip(ed.pickerTitle()); title != "Add a language  after English, as the 3rd" {
		t.Errorf("picker title %q", title)
	}
	ed.view(120, 36)
}

// TestEditorSave checks that enter refuses to save while the check finds
// problems, including a name another profile has, and otherwise sends the
// encoded profile: PUT for a stored profile, POST for a new one.
// TestLanguagePicker lists the languages by name and, in a short terminal,
// keeps the overlay whole and the cursor's row on the screen.
func TestLanguagePicker(t *testing.T) {
	_, cl := newRecorder(t)
	a, ed := editorApp(cl, 1)
	sendKeys(a, "a")
	if ed.picker == nil {
		t.Fatal("a did not open the language picker")
	}
	var names []string
	for _, it := range ed.picker.items[1:] {
		names = append(names, it.label)
	}
	if !slices.IsSorted(names) {
		t.Errorf("the languages are not by name: %v", names)
	}
	for _, tc := range []struct{ key, hidden string }{{"", "\u2193"}, {"G", "\u2191"}} {
		if tc.key != "" {
			sendKeys(a, tc.key)
		}
		view := ansi.Strip(ed.view(80, 18))
		chosen := ed.picker.items[ed.picker.choice].label
		if rows := strings.Count(view, "\n") + 1; rows > 18 || !strings.Contains(view, chosen) ||
			!strings.Contains(view, "esc cancel") || !strings.Contains(view, tc.hidden) {
			t.Errorf("after %q: %d rows, cursor on %q, view:\n%s", tc.key, rows, chosen, view)
		}
	}
}

func TestEditorSave(t *testing.T) {
	rec, cl := newRecorder(t)
	a, ed := editorApp(cl, 3)
	// Priority on a single language warns but saves; no audio is a problem.
	sendKeys(a, "j", "j", "j", "l")
	if ed.p.Priority != profile.ByResolution {
		t.Fatalf("l on Priority gave %q", ed.p.Priority)
	}
	problems, warnings := ed.check()
	if len(problems) != 0 || len(warnings) != 1 {
		t.Fatalf("check: problems %v, warnings %v", problems, warnings)
	}
	sendKeys(a, "k", "k", "k", "d")
	if problems, _ := ed.check(); len(problems) != 1 || !strings.Contains(problems[0], "audio") {
		t.Fatalf("without audio: problems %v", problems)
	}
	if cmd := sendKeys(a, "enter"); cmd != nil || !strings.Contains(ansi.Strip(ed.flash.text), "Not saved") {
		t.Fatalf("enter with a problem: command %v, message %q", cmd != nil, ansi.Strip(ed.flash.text))
	}
	sendKeys(a, "a", "enter") // Original language back in
	// A name another profile has is a problem too.
	ed.p.Name = "Kids, German only"
	if problems, _ := ed.check(); len(problems) != 1 || !strings.Contains(problems[0], "already exists") {
		t.Fatalf("with a taken name: problems %v", problems)
	}
	if sendKeys(a, "enter") != nil || len(rec.made("PUT")) != 0 {
		t.Fatal("enter with a taken name saved")
	}
	ed.p.Name = "German"
	msg := run(t, a, "enter")
	if _, ok := msg.(editorSavedMsg); !ok {
		t.Fatalf("saving returned %T", msg)
	}
	if made := rec.made("PUT"); len(made) != 1 || made[0] != "PUT /api/v1/profiles/3" {
		t.Fatalf("saving asked %v", made)
	}
	var sent struct{ Text string }
	json.Unmarshal([]byte(rec.body("PUT /api/v1/profiles/3")), &sent)
	saved, _, err := profile.Parse(sent.Text)
	if err != nil || !slices.Equal(saved.Audio, []string{profile.Original}) || saved.Priority != profile.ByResolution {
		t.Errorf("the server got %+v, %v", saved, err)
	}

	a, ed = editorApp(cl, 0)
	ed.p = emptyProfile()
	ed.p.Name, ed.p.Audio, ed.p.Resolution = "Subs first", []string{profile.Original}, []int{1080}
	run(t, a, "enter")
	if made := rec.made("POST"); len(made) != 1 || made[0] != "POST /api/v1/profiles" {
		t.Errorf("a new profile asked %v", made)
	}
}

// TestEditorTyping checks e on the name: typing replaces it, esc keeps the
// old name, and the keys every screen has stay with the field.
func TestEditorTyping(t *testing.T) {
	_, cl := newRecorder(t)
	a, ed := editorApp(cl, 3)
	sendKeys(a, "k", "k", "e")
	if !ed.typing || !ed.captures() {
		t.Fatal("e on the name did not start typing")
	}
	for _, r := range " ?A" {
		a.update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if a.help || a.screen != screen(ed) {
		t.Fatal("? or A left the field")
	}
	sendKeys(a, "esc")
	if ed.typing || ed.p.Name != "German" {
		t.Fatalf("esc: typing %v, name %q", ed.typing, ed.p.Name)
	}
	sendKeys(a, "e")
	a.update(tea.KeyPressMsg{Code: '!', Text: "!"})
	sendKeys(a, "enter")
	if ed.p.Name != "German!" || !ed.changed() {
		t.Fatalf("enter: name %q, changed %v", ed.p.Name, ed.changed())
	}
}

// TestEditorDiscard checks esc: it leaves at once without changes, and asks
// with Keep editing preselected when there are some.
func TestEditorDiscard(t *testing.T) {
	_, cl := newRecorder(t)
	a, ed := editorApp(cl, 3)
	a.update(sendKeys(a, "esc")())
	if _, ok := a.screen.(*profilesScreen); !ok {
		t.Fatal("esc without changes did not go back")
	}
	a, ed = editorApp(cl, 3)
	sendKeys(a, "j", "j", "j", "j", "l", "esc")
	if !ed.discard || ed.choice != 0 {
		t.Fatalf("esc with changes: asking %v, choice %d", ed.discard, ed.choice)
	}
	if cmd := sendKeys(a, "enter"); cmd != nil || ed.discard || a.screen != screen(ed) {
		t.Fatal("Keep editing left the editor")
	}
	a.update(sendKeys(a, "esc", "l", "enter")())
	if _, ok := a.screen.(*profilesScreen); !ok {
		t.Fatal("Discard did not go back")
	}
}

// TestExport checks the export overlay: the path is prefilled from the
// folder garner runs in and the profile's name, the file gets the
// profile's text, and an existing file is kept unless Overwrite is chosen.
func TestExport(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(wd) })

	_, cl := newRecorder(t)
	a, p := profilesApp(cl, 1)
	sendKeys(a, "x")
	if p.overlay != exportOverlay {
		t.Fatal("x did not open the export overlay")
	}
	dir, _ = filepath.EvalSymlinks(dir) // os.Getwd resolves the temporary folder's links
	want := filepath.Join(dir, "german-english-fallback.toml")
	if got := expandHome(p.export.input.Value()); got != want {
		t.Fatalf("the path is prefilled with %q, want %q", got, want)
	}
	sendKeys(a, "enter")
	data, err := os.ReadFile(want)
	if err != nil || string(data) != profile.Encode(germanFallback) {
		t.Fatalf("the file: %v, %q", err, data)
	}
	if p.overlay != profilesNoOverlay || !strings.Contains(ansi.Strip(p.flash.text), "Exported German, English fallback to ") {
		t.Errorf("after export: overlay %d, message %q", p.overlay, ansi.Strip(p.flash.text))
	}

	// The file exists now: exporting again asks, with Keep it preselected.
	os.WriteFile(want, []byte("mine"), 0o644)
	sendKeys(a, "x", "enter")
	if !p.export.asking || p.export.choice != 0 {
		t.Fatalf("an existing file: asking %v, choice %d", p.export.asking, p.export.choice)
	}
	sendKeys(a, "enter")
	if data, _ := os.ReadFile(want); string(data) != "mine" || p.export.asking || p.overlay != exportOverlay {
		t.Fatalf("Keep it: file %q, asking %v, overlay %d", data, p.export.asking, p.overlay)
	}
	sendKeys(a, "enter", "l", "enter")
	if data, _ := os.ReadFile(want); string(data) != profile.Encode(germanFallback) {
		t.Errorf("Overwrite left %q", data)
	}

	home, _ := os.UserHomeDir()
	if got := expandHome("~/x.toml"); got != filepath.Join(home, "x.toml") {
		t.Errorf("expandHome = %q", got)
	}
	for name, want := range map[string]string{"German, English fallback": "german-english-fallback.toml", "Kids, German only": "kids-german-only.toml", "!": "profile.toml"} {
		if got := exportFileName(name); got != want {
			t.Errorf("exportFileName(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestFetchProfile checks the limits of fetching a link: https only,
// redirects only to https, and at most profile.MaxSize bytes; a github.com
// page link is read in its raw form.
func TestFetchProfile(t *testing.T) {
	text := profile.Encode(kids)
	mux := http.NewServeMux()
	mux.HandleFunc("/kids.toml", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(text)) })
	mux.HandleFunc("/big.toml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("#", profile.MaxSize+1)))
	})
	mux.HandleFunc("/exact.toml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("#", profile.MaxSize)))
	})
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/kids.toml", http.StatusFound) })
	mux.HandleFunc("/downgrade", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/kids.toml", http.StatusFound)
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // closing drops the connections the limit cut short
	srv.StartTLS()
	defer srv.Close()
	rt := srv.Client().Transport.(*http.Transport)
	ctx := context.Background()

	if got, err := fetchProfile(ctx, srv.URL+"/kids.toml", rt); err != nil || got != text {
		t.Fatalf("a plain fetch: %q, %v", got, err)
	}
	if got, err := fetchProfile(ctx, srv.URL+"/moved", rt); err != nil || got != text {
		t.Errorf("a redirect within https: %q, %v", got, err)
	}
	if _, err := fetchProfile(ctx, srv.URL+"/exact.toml", rt); err != nil {
		t.Errorf("exactly the limit: %v", err)
	}
	for name, link := range map[string]string{
		"too big":            srv.URL + "/big.toml",
		"redirect to http":   srv.URL + "/downgrade",
		"http link":          "http://" + strings.TrimPrefix(srv.URL, "https://") + "/kids.toml",
		"not found":          srv.URL + "/missing",
		"not a link":         "kids.toml",
		"file link":          "file:///etc/hosts",
		"ftp link":           "ftp://example.test/kids.toml",
		"https without host": "https:///kids.toml",
	} {
		if got, err := fetchProfile(ctx, link, rt); err == nil {
			t.Errorf("%s: fetched %d bytes, an error was expected", name, len(got))
		}
	}
	for link, want := range map[string]string{
		"https://github.com/trucke/garner/blob/main/internal/profile/shipped/german.toml": "https://raw.githubusercontent.com/trucke/garner/main/internal/profile/shipped/german.toml",
		"https://github.com/trucke/garner":                                                "https://github.com/trucke/garner",
		"https://raw.githubusercontent.com/trucke/garner/main/x":                          "https://raw.githubusercontent.com/trucke/garner/main/x",
	} {
		if got := rawLink(link); got != want {
			t.Errorf("rawLink(%q) = %q, want %q", link, got, want)
		}
	}
}

// TestImport checks the import screen: pasted text with problems stays
// with them listed and nothing saved, a new profile previews and saves
// with POST, and a profile whose name exists previews what changes and
// saves with PUT on the existing one.
func TestImport(t *testing.T) {
	rec, cl := newRecorder(t)
	a, p := profilesApp(cl, 1)
	a.update(sendKeys(a, "i")())
	im, ok := a.screen.(*importScreen)
	if !ok {
		t.Fatal("i did not open the import screen")
	}
	a.update(tea.PasteMsg{Content: "garner_profile = 1\nname = \"Kids\"\naudio = [\"deu\"]\nresolution = [\"4k\"]\n"})
	if cmd := sendKeys(a, "enter"); cmd != nil || len(im.problems) != 2 || a.screen != screen(im) {
		t.Fatalf("a text with problems: command %v, problems %v", cmd != nil, im.problems)
	}
	if view := ansi.Strip(im.view(120, 36)); !strings.Contains(view, "2 problems; the profile cannot be imported as it is") {
		t.Errorf("the problems are not announced:\n%s", view)
	}

	sendKeys(a, "ctrl+u")
	a.update(tea.PasteMsg{Content: profile.Encode(subs)})
	a.update(sendKeys(a, "enter")())
	pv, ok := a.screen.(*importPreviewScreen)
	if !ok {
		t.Fatalf("a new profile previewed as %T", a.screen)
	}
	if pv.existing != nil || len(pv.warnings) != 1 {
		t.Fatalf("a new profile: existing %v, warnings %v", pv.existing, pv.warnings)
	}
	if view := ansi.Strip(pv.view(120, 36)); !strings.Contains(view, "Original 1080p → English 1080p") || !strings.Contains(view, "Priority       resolution") {
		t.Errorf("the preview:\n%s", view)
	}
	run(t, a, "enter")
	if made := rec.made("POST"); len(made) != 1 || made[0] != "POST /api/v1/profiles" {
		t.Errorf("saving a new profile asked %v", made)
	}
	if _, ok := a.screen.(*profilesScreen); !ok || !strings.Contains(ansi.Strip(p.flash.text), "Imported Subs first") {
		t.Errorf("after saving: %T, message %q", a.screen, ansi.Strip(p.flash.text))
	}

	// The same name as German, with English added: what changes shows,
	// and saving replaces German in place.
	a.screen = im
	sendKeys(a, "ctrl+u")
	changed := german
	changed.Audio = []string{"de", "en"}
	changed.DualAudio = profile.IgnoreDual
	a.update(tea.PasteMsg{Content: profile.Encode(changed)})
	a.update(sendKeys(a, "enter")())
	pv, ok = a.screen.(*importPreviewScreen)
	if !ok || pv.existing == nil || pv.existing.ID != 3 {
		t.Fatalf("a known name previewed as %T", a.screen)
	}
	view := ansi.Strip(pv.view(120, 36))
	for _, want := range []string{"Saving replaces your profile German. 1 show uses it and follows the new settings:", "Lanterns and Tides",
		"WHAT CHANGES", "Audio          German                        German, then English", "Dual audio     prefer                        ignore",
		"Preferences    before  German 1080p → German 720p", "after   German 1080p → German 720p → English 1080p → English 720p"} {
		if !strings.Contains(view, want) {
			t.Errorf("the preview lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Resolution     ") {
		t.Error("the preview lists a setting that does not change")
	}
	run(t, a, "enter")
	if made := rec.made("PUT"); len(made) != 1 || made[0] != "PUT /api/v1/profiles/3" {
		t.Errorf("replacing asked %v", made)
	}
	if !strings.Contains(ansi.Strip(p.flash.text), "Replaced German · 1 show follows the new settings") {
		t.Errorf("after replacing: %q", ansi.Strip(p.flash.text))
	}

	// e opens the editor on the imported settings, for the existing ID.
	a.screen = im
	a.update(sendKeys(a, "enter")())
	a.update(sendKeys(a, "e")())
	ed, ok := a.screen.(*editorScreen)
	if !ok || ed.id != 3 || !slices.Equal(ed.p.Audio, []string{"de", "en"}) {
		t.Fatalf("e opened %T", a.screen)
	}
	// The import is not saved yet, so leaving asks.
	sendKeys(a, "esc")
	if !ed.discard {
		t.Error("esc left the editor without asking about the unsaved import")
	}
}

// TestImportLink checks that a link is fetched by the interface, never
// sent to the server, and that an http link is refused.
func TestImportLink(t *testing.T) {
	rec, cl := newRecorder(t)
	a, _ := profilesApp(cl, 1)
	a.update(sendKeys(a, "i")())
	im := a.screen.(*importScreen)
	for _, r := range "http://profiles.example/kids.toml" {
		a.update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	msg := run(t, a, "enter")
	if f, ok := msg.(fetchedMsg); !ok || f.err == nil || im.fetching != "" {
		t.Fatalf("an http link: %+v, fetching %q", msg, im.fetching)
	}
	if !strings.Contains(ansi.Strip(im.flash.text), "only https links") {
		t.Errorf("message %q", ansi.Strip(im.flash.text))
	}
	if n := len(rec.requests); n != 0 {
		t.Errorf("the server saw %v", rec.requests)
	}
}

// TestProfileAndType checks the overlay on the details page: the cursor
// starts on the show's type, enter on the other type patches the type,
// enter on another profile patches the profile, and P opens Profiles.
func TestProfileAndType(t *testing.T) {
	rec, cl := newRecorder(t)
	sess := &session{client: cl}
	show := newShow(sess, nil, 7, "Glass Orchard", "")
	// The recording server answers every change with an empty show, so the
	// detail is set again before each step.
	reset := func() {
		show.setDetail(api.SeriesDetail{
			SeriesData:  api.SeriesData{Series: api.Series{ID: 7, Name: "Glass Orchard", DefaultOrder: "official", Type: api.TypeAnime, ProfileID: 1}},
			Folder:      "/mnt/media/anime/Glass.Orchard.[tvdbid-7]",
			TypeFolders: map[string]string{api.TypeTV: "/mnt/media/tv", api.TypeAnime: "/mnt/media/anime"},
			Profiles: []api.ProfileInfo{{ID: 1, Name: "German, English fallback", Rules: "German 1080p → German 720p · dual audio preferred", Preferences: "German 1080p → German 720p"},
				{ID: 2, Name: "Original, English dub", Rules: "Japanese 1080p → Japanese 720p · dual audio preferred", Preferences: "Japanese 1080p → Japanese 720p"}},
		})
	}
	reset()
	a := &app{sess: sess, library: newLibrary(sess)}
	det := newShowDetails(sess, show, a.library)
	a.screen = det

	sendKeys(a, "p")
	if det.overlay != profileOverlay || det.choice != 1 {
		t.Fatalf("after p: overlay %d, choice %d", det.overlay, det.choice)
	}
	view := ansi.Strip(det.view(120, 36))
	for _, want := range []string{"Profile and type of Glass Orchard", "S01E05 · new files under /mnt/media/tv", "101 first · new files under /mnt/media/anime",
		"files already in the library stay where they are", "P manage profiles"} {
		if !strings.Contains(view, want) {
			t.Errorf("the overlay lacks %q:\n%s", want, view)
		}
	}
	// The profile rows show the preferences alone, not the whole rules.
	if !strings.Contains(view, "Japanese 1080p → Japanese 720p ") || strings.Contains(view, "dual audio preferred") {
		t.Errorf("the profile rows:\n%s", view)
	}
	if cmd := sendKeys(a, "enter"); cmd != nil || det.overlay != noOverlay {
		t.Fatal("enter on the show's own type asked the server")
	}
	run(t, a, "p", "k", "enter")
	if got := rec.body("PATCH /api/v1/series/7"); got != `{"type":"tv"}` {
		t.Errorf("TV sent %q", got)
	}
	reset()
	if cmd := sendKeys(a, "p", "j", "enter"); cmd != nil {
		t.Fatal("enter on the show's own profile asked the server")
	}
	run(t, a, "p", "j", "j", "enter")
	if got := rec.body("PATCH /api/v1/series/7"); got != `{"profile_id":2}` {
		t.Errorf("the other profile sent %q", got)
	}
	reset()
	a.update(sendKeys(a, "p", "P")())
	if pr, ok := a.screen.(*profilesScreen); !ok || pr.back != screen(det) {
		t.Errorf("P opened %T", a.screen)
	}
	if view := ansi.Strip(det.view(120, 36)); !strings.Contains(view, "Type        Anime · a bare number such as 101 is an absolute episode first · files under /mnt/media/anime") {
		t.Errorf("the details page lacks the type line:\n%s", view)
	}
}

// TestProfilesFromEverywhere checks that P opens Profiles from the library
// and esc returns, but not while a filter is typed.
func TestProfilesFromEverywhere(t *testing.T) {
	_, cl := newRecorder(t)
	a, lib := libraryApp(cl, false)
	sendKeys(a, "P")
	p, ok := a.screen.(*profilesScreen)
	if !ok || p.back != screen(lib) {
		t.Fatalf("P opened %T", a.screen)
	}
	a.update(sendKeys(a, "esc")())
	if a.screen != screen(lib) {
		t.Fatal("esc did not return to the library")
	}
	lib.typing = true
	sendKeys(a, "P")
	if a.screen != screen(lib) || lib.filter != "P" {
		t.Error("P left a filter being typed")
	}
}
