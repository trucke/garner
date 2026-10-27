package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/client"
	"github.com/trucke/garner/internal/profile"
)

// Profiles lists the stored profiles with their preferences and the shows
// that use them, and holds the questions about them: new, export, delete
// and use for new shows. P opens it from every screen.

type profilesOverlay int

const (
	profilesNoOverlay profilesOverlay = iota
	newPickerOverlay
	exportOverlay
	deleteProfileOverlay
	deleteRefusedOverlay
	useForOverlay
)

type (
	// profilesMsg answers one screen's reload; from tells which.
	profilesMsg struct {
		from     *profilesScreen
		profiles []api.Profile
		err      error
	}
	// profileChangedMsg reports a change the screen asked the server for.
	profileChangedMsg struct {
		err     error
		success string
		id      int64 // the profile to select, when known
	}
)

// profileRow is a stored profile with its text read.
type profileRow struct {
	api.Profile
	parsed profile.Profile
	ok     bool // the text could be read
}

type profilesScreen struct {
	sess     *session
	back     screen
	rows     []profileRow // in the table's order
	loaded   bool
	loading  bool
	dirty    bool
	err      error
	selected int64
	offset   int
	page     int
	lastKey  string
	flash    flash
	busy     string
	overlay  profilesOverlay
	choice   int
	picker   pickList // the new-profile picker
	export   exportPrompt
}

func newProfiles(sess *session, back screen) *profilesScreen {
	return &profilesScreen{sess: sess, back: back, page: 10}
}

func (s *profilesScreen) init() tea.Cmd { return s.reload() }

// returned reopens the screen after the editor or the import saved a
// profile: the saved one is selected and the message shows.
func (s *profilesScreen) returned(id int64, message string) screen {
	if id != 0 {
		s.selected = id
	}
	s.flash.info(message)
	return s
}

func (s *profilesScreen) reload() tea.Cmd {
	if s.loading {
		s.dirty = true
		return nil
	}
	s.loading = true
	cl := s.sess.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		profiles, err := cl.Profiles(ctx)
		return profilesMsg{s, profiles, err}
	}
}

// set takes the loaded profiles: those with most shows first, then by name.
func (s *profilesScreen) set(profiles []api.Profile) {
	s.rows = s.rows[:0]
	for _, p := range profiles {
		parsed, _, err := profile.Parse(p.Text)
		s.rows = append(s.rows, profileRow{Profile: p, parsed: parsed, ok: err == nil})
	}
	slices.SortStableFunc(s.rows, func(a, b profileRow) int {
		return cmp.Or(cmp.Compare(len(b.Shows), len(a.Shows)), cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)))
	})
	s.loaded = true
}

func (s *profilesScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case profilesMsg:
		if msg.from != s {
			return s, nil
		}
		s.loading = false
		switch {
		case errors.Is(msg.err, client.ErrUnauthorized):
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		case msg.err != nil:
			s.err = msg.err
		default:
			s.err = nil
			s.set(msg.profiles)
		}
		if s.dirty {
			s.dirty = false
			return s, s.reload()
		}
	case profileChangedMsg:
		s.busy = ""
		if errors.Is(msg.err, client.ErrUnauthorized) {
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		}
		if msg.err != nil {
			s.flash.stay(gFailed + " " + sRed.Render(clean(msg.err.Error())))
			return s, nil
		}
		if msg.id != 0 {
			s.selected = msg.id
		}
		s.flash.info(gDone + " " + sNormal.Render(msg.success))
		return s, s.reload()
	case eventMsg:
		switch msg.Type {
		case client.EventConnected, "profiles.updated", "series.updated", "series.deleted":
			return s, s.reload()
		}
	case tea.PasteMsg:
		if s.overlay == exportOverlay && !s.export.asking {
			var cmd tea.Cmd
			s.export.input, cmd = s.export.input.Update(msg)
			return s, cmd
		}
	case tea.KeyPressMsg:
		if s.busy != "" {
			return s, nil
		}
		s.flash.clear()
		if s.overlay != profilesNoOverlay {
			return s, s.overlayKey(msg)
		}
		return s, s.key(msg.String())
	}
	return s, nil
}

// cursor is the row under the cursor, if any.
func (s *profilesScreen) cursor() (profileRow, bool) {
	i := slices.IndexFunc(s.rows, func(r profileRow) bool { return r.ID == s.selected })
	if i < 0 {
		return profileRow{}, false
	}
	return s.rows[i], true
}

func (s *profilesScreen) move(n int) {
	if len(s.rows) == 0 {
		return
	}
	i := max(0, slices.IndexFunc(s.rows, func(r profileRow) bool { return r.ID == s.selected }))
	s.selected = s.rows[min(len(s.rows)-1, max(0, i+n))].ID
}

func (s *profilesScreen) key(key string) tea.Cmd {
	prev := s.lastKey
	s.lastKey = key
	switch key {
	case "esc":
		return func() tea.Msg { return switchMsg{s.back} }
	case "j", "down":
		s.move(1)
	case "k", "up":
		s.move(-1)
	case "g":
		if prev == "g" {
			s.move(-1 << 30)
			s.lastKey = ""
		}
	case "G":
		s.move(1 << 30)
	case "ctrl+d":
		s.move(max(1, s.page/2))
	case "ctrl+u":
		s.move(-max(1, s.page/2))
	case "i":
		next := newImport(s.sess, s)
		return func() tea.Msg { return switchMsg{next} }
	case "a":
		s.overlay, s.picker = newPickerOverlay, s.newPicker()
	}
	r, ok := s.cursor()
	if !ok {
		return nil
	}
	switch key {
	case "e", "enter":
		if !r.ok {
			s.flash.stay(gFailed + " " + sRed.Render(r.Name+" can't be read by this Garner; update it."))
			return nil
		}
		next := newEditor(s.sess, s, r.ID, r.parsed, len(r.Shows))
		return func() tea.Msg { return switchMsg{next} }
	case "x":
		s.overlay, s.export = exportOverlay, newExportPrompt(r.Name)
		return s.export.input.Focus()
	case "d":
		if len(r.Shows) > 0 || r.DefaultTV || r.DefaultAnime {
			s.overlay = deleteRefusedOverlay
		} else {
			s.overlay, s.choice = deleteProfileOverlay, 0 // the safe choice: cancel
		}
	case "u":
		s.overlay, s.choice = useForOverlay, 0
	}
	return nil
}

func (s *profilesScreen) overlayKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	r, _ := s.cursor()
	switch s.overlay {
	case deleteRefusedOverlay:
		s.overlay = profilesNoOverlay
	case newPickerOverlay:
		done, chosen := s.picker.key(key)
		if !done {
			return nil
		}
		s.overlay = profilesNoOverlay
		if chosen < 0 {
			return nil
		}
		start, _ := s.picker.items[chosen].value.(profile.Profile)
		// A copy needs a name of its own; the editor starts on the name.
		start.Name = ""
		next := newEditor(s.sess, s, 0, start, 0)
		return func() tea.Msg { return switchMsg{next} }
	case deleteProfileOverlay:
		switch key {
		case "esc":
			s.overlay = profilesNoOverlay
		case "h", "left", "shift+tab":
			s.choice = 0
		case "l", "right", "tab":
			s.choice = 1
		case "enter":
			s.overlay = profilesNoOverlay
			if s.choice != 1 {
				return nil
			}
			s.busy = "Deleting " + r.Name + "…"
			cl, id, name := s.sess.client, r.ID, r.Name
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				err := cl.DeleteProfile(ctx, id)
				return profileChangedMsg{err: err, success: "Deleted " + name}
			}
		}
	case useForOverlay:
		switch key {
		case "esc":
			s.overlay = profilesNoOverlay
		case "k", "up", "shift+tab":
			s.choice = max(0, s.choice-1)
		case "j", "down", "tab":
			s.choice = min(2, s.choice+1)
		case "enter":
			s.overlay = profilesNoOverlay
			types, label := []string{api.TypeTV}, "New TV shows"
			switch s.choice {
			case 1:
				types, label = []string{api.TypeAnime}, "New anime shows"
			case 2:
				types, label = []string{api.TypeTV, api.TypeAnime}, "New TV and anime shows"
			}
			s.busy = "Changing the profile for new shows…"
			cl, id, name := s.sess.client, r.ID, r.Name
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_, err := cl.SetDefaultProfile(ctx, id, types...)
				return profileChangedMsg{err: err, success: label + " get " + name, id: id}
			}
		}
	case exportOverlay:
		return s.exportKey(msg, r)
	}
	return nil
}

// exportKey handles the export overlay: the path field, or the question
// about a file that already exists.
func (s *profilesScreen) exportKey(msg tea.KeyPressMsg, r profileRow) tea.Cmd {
	key := msg.String()
	e := &s.export
	if e.asking {
		switch key {
		case "esc":
			e.asking = false
		case "h", "left", "shift+tab":
			e.choice = 0
		case "l", "right", "tab":
			e.choice = 1
		case "enter":
			e.asking = false
			if e.choice == 1 {
				s.exported(r, e.path, true)
			}
		}
		return nil
	}
	switch key {
	case "esc":
		s.overlay = profilesNoOverlay
		return nil
	case "enter":
		path := expandHome(strings.TrimSpace(e.input.Value()))
		if path == "" {
			return nil
		}
		s.exported(r, path, false)
		return nil
	}
	var cmd tea.Cmd
	e.input, cmd = e.input.Update(msg)
	return cmd
}

// exported writes the profile to path and reports it, or asks before
// overwriting a file that exists.
func (s *profilesScreen) exported(r profileRow, path string, overwrite bool) {
	text := r.Text
	if r.ok {
		text = profile.Encode(r.parsed)
	}
	err := writeProfileFile(path, text, overwrite)
	switch {
	case errors.Is(err, os.ErrExist):
		s.export.asking, s.export.choice, s.export.path = true, 0, path
		return
	case err != nil:
		s.overlay = profilesNoOverlay
		s.flash.stay(gFailed + " " + sRed.Render("Could not write "+homeShort(path)+": "+clean(err.Error())))
		return
	}
	s.overlay = profilesNoOverlay
	s.flash.info(gDone + " " + sNormal.Render("Exported "+r.Name+" to "+homeShort(path)))
}

// writeProfileFile writes text to a new file at path. An existing file is
// left alone with os.ErrExist unless overwrite is set.
func writeProfileFile(path, text string, overwrite bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if overwrite {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// expandHome replaces a leading ~ with the home folder.
func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

// exportFileName makes a file name from a profile's name: lowercase, words
// joined by hyphens, as "german-english-fallback.toml".
func exportFileName(name string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if hyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			hyphen = false
			b.WriteRune(r)
		} else {
			hyphen = true
		}
	}
	return cmp.Or(b.String(), "profile") + ".toml"
}

// exportPrompt is the export overlay: the path field, and the question
// when the file exists.
type exportPrompt struct {
	input  textinput.Model
	asking bool
	choice int    // 1: overwrite
	path   string // the file asked about
}

// newExportPrompt prefills the path with the folder garner was started in
// and a file name from the profile's name.
func newExportPrompt(name string) exportPrompt {
	in := textinput.New()
	in.Prompt = ""
	st := textinput.DefaultStyles(true)
	st.Cursor.Blink = false
	st.Cursor.Color = cFg
	st.Focused.Text = sNormal
	in.SetStyles(st)
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	in.SetValue(homeShort(filepath.Join(dir, exportFileName(name))))
	in.CursorEnd()
	return exportPrompt{input: in}
}

func (e *exportPrompt) lines(name string) []string {
	lines := []string{sBold.Render("Export " + name), ""}
	if e.asking {
		return append(lines,
			sNormal.Render(homeShort(e.path)+" already exists."),
			"",
			"  "+choice("Keep it", e.choice == 0)+"   "+choice("Overwrite", e.choice == 1),
			"",
			keyBar("h l", "choose", "enter", "ok", "esc", "back"))
	}
	e.input.SetWidth(max(20, min(80, lipgloss.Width(e.input.Value())+12)))
	return append(lines,
		sSubtle.Render("File")+"  "+e.input.View(),
		"",
		sMuted.Render("The profile's text is written to this file."),
		sMuted.Render("A file that already exists is not overwritten without asking."),
		"",
		keyBar("enter", "export", "esc", "cancel"))
}

// newPicker lists where a new profile may start from: empty, one of the
// user's profiles, or one shipped with Garner.
func (s *profilesScreen) newPicker() pickList {
	const nameWidth, prefsWidth = 26, 61
	p := pickList{labelWidth: nameWidth + 3, width: 1 + nameWidth + 3 + prefsWidth + 1}
	p.items = append(p.items, pickItem{label: "Empty profile", note: "no audio or resolution yet", value: emptyProfile()}, pickItem{header: true})
	if len(s.rows) > 0 {
		p.items = append(p.items, pickItem{label: "Your profiles", header: true})
		for _, r := range s.rows {
			if r.ok {
				p.items = append(p.items, pickItem{label: r.Name, note: prefsLine(r.parsed), value: r.parsed})
			}
		}
		p.items = append(p.items, pickItem{header: true})
	}
	p.items = append(p.items, pickItem{label: "Shipped with Garner", header: true})
	for _, sp := range profile.Shipped() {
		p.items = append(p.items, pickItem{label: sp.Name, note: prefsLine(sp), value: sp})
	}
	return p
}

// emptyProfile is a profile with the defaults and nothing chosen yet.
func emptyProfile() profile.Profile {
	return profile.Profile{Priority: profile.ByAudio, DualAudio: profile.PreferDual, UnnamedAudio: profile.RejectUnnamed, Upgrades: true}
}

// profilesTop counts the rows above the table: header, blank, facts,
// blank, column names, rule.
const profilesTop = 6

func (s *profilesScreen) view(width, height int) string {
	r, hasCursor := s.cursor()
	if !hasCursor && len(s.rows) > 0 {
		r, hasCursor = s.rows[0], true
		s.selected = r.ID
	}
	var details []string
	if hasCursor {
		details = s.detailLines(r, min(width, designWidth)-6-6)
	}
	// The table gets what the details pane leaves.
	s.page = max(3, height-1-profilesTop-1-(len(details)+2))
	cursor := max(0, slices.IndexFunc(s.rows, func(p profileRow) bool { return p.ID == s.selected }))
	s.offset = min(max(s.offset, cursor-s.page+1), cursor)

	var b strings.Builder
	b.WriteString(header(width, s.sess, "Garner", "Profiles") + "\n\n")
	b.WriteString(factLines(width, s.facts()...) + "\n\n")
	cols := s.columns(width)
	b.WriteString(cols.head() + "\n")
	b.WriteString("   " + sBorder.Render(strings.Repeat("─", max(0, width-6))) + "\n")
	switch {
	case !s.loaded && s.err != nil:
		b.WriteString("   " + gFailed + " " + sRed.Render(clean(s.err.Error())) + "\n")
	case !s.loaded:
		b.WriteString("   " + sMuted.Render("Loading the profiles…") + "\n")
	case len(s.rows) == 0:
		b.WriteString("   " + sMuted.Render("No profiles. Press ") + sKey.Render("a") + sMuted.Render(" to add one.") + "\n")
	default:
		end := min(len(s.rows), s.offset+s.page)
		for i := s.offset; i < end; i++ {
			b.WriteString(cols.row(s.rows[i], i == cursor) + "\n")
		}
	}
	if hasCursor {
		b.WriteString("\n" + indent(frame("details", details, min(width, designWidth)-6), 3) + "\n")
	}
	page := screenLayout(width, height, b.String(), s.bottom())
	switch s.overlay {
	case newPickerOverlay:
		return overlay(page, s.picker.lines(sBold.Render("New profile")+"  "+sMuted.Render("start from"), height), width, height)
	case exportOverlay:
		return overlay(page, s.export.lines(r.Name), width, height)
	case deleteProfileOverlay:
		return overlay(page, s.deleteLines(r), width, height)
	case deleteRefusedOverlay:
		return overlay(page, s.refusedLines(r), width, height)
	case useForOverlay:
		return overlay(page, s.useForLines(r), width, height)
	}
	return page
}

// facts are the line under the breadcrumb: the count and the profiles new
// shows get.
func (s *profilesScreen) facts() []string {
	count := strconv.Itoa(len(s.rows))
	tv, anime := sMuted.Render("none"), sMuted.Render("none")
	for _, r := range s.rows {
		if r.DefaultTV {
			tv = sNormal.Render(r.Name)
		}
		if r.DefaultAnime {
			anime = sNormal.Render(r.Name)
		}
	}
	if !s.loaded {
		count, tv, anime = sMuted.Render("…"), sMuted.Render("…"), sMuted.Render("…")
	}
	return []string{
		sSubtle.Render("Profiles") + "  " + sNormal.Render(count),
		sSubtle.Render("New TV shows") + "  " + tv,
		sSubtle.Render("New anime shows") + "  " + anime,
	}
}

// profileColumns holds the table's widths: name, preferences, shows and
// default.
type profileColumns struct {
	width, name, prefs int
}

const showsWidth, defaultWidth = 7, 9 // "▾ Shows", "TV, Anime"

func (s *profilesScreen) columns(width int) profileColumns {
	c := profileColumns{width: width, name: len("Profile")}
	for _, r := range s.rows {
		c.name = max(c.name, ansi.StringWidth(r.Name))
	}
	// Margins and gaps: 3 + name + 3 + prefs + 3 + shows + 3 + default.
	fixed := 3 + 3 + 3 + showsWidth + 3 + defaultWidth
	// The name column is 26 cells, as designed, unless the terminal is narrow.
	c.name = max(12, min(max(c.name, 26), width-fixed-minName))
	c.prefs = max(0, width-fixed-c.name)
	return c
}

func (c profileColumns) head() string {
	return "   " + sSubtle.Render(pad("Profile", c.name)) + "   " + sSubtle.Render(pad("Preferences", c.prefs)) +
		"   " + sSubtle.Render(fit("▾ Shows", showsWidth, true)) + "   " + sSubtle.Render("Default")
}

func (c profileColumns) row(r profileRow, selected bool) string {
	bg := func(st lipgloss.Style) lipgloss.Style {
		if selected {
			return st.Background(cSelect)
		}
		return st
	}
	gap := bg(lipgloss.NewStyle()).Render("   ")
	prefs, prefsStyle := prefsLine(r.parsed), sMuted
	if !r.ok {
		prefs, prefsStyle = "can't be read by this Garner", sRed
	}
	line := gap + bg(sNormal).Render(fit(r.Name, c.name, false)) + gap +
		bg(prefsStyle).Render(fit(prefs, c.prefs, false)) + gap +
		bg(sNormal).Render(fit(strconv.Itoa(len(r.Shows)), showsWidth, true)) + gap +
		bg(sNormal).Render(fit(defaultLabel(r.Profile), defaultWidth, false))
	if w := lipgloss.Width(line); w < c.width {
		line += bg(lipgloss.NewStyle()).Render(strings.Repeat(" ", c.width-w))
	}
	return line
}

// defaultLabel says which new shows get the profile: TV, Anime or both.
func defaultLabel(p api.Profile) string {
	switch {
	case p.DefaultTV && p.DefaultAnime:
		return "TV, Anime"
	case p.DefaultTV:
		return "TV"
	case p.DefaultAnime:
		return "Anime"
	}
	return ""
}

// settingWidth is the label column of the settings lines.
const settingWidth = 15

// labelled renders a subtle label of settingWidth cells and a value.
func labelled(label, value string) string {
	return sSubtle.Render(pad(label, settingWidth)) + value
}

// wrapLabelled is labelled with the value, which may mix styles, wrapped
// under itself within inner cells.
func wrapLabelled(label, value string, inner int) []string {
	var lines []string
	for i, l := range strings.Split(ansi.Wrap(value, max(10, inner-settingWidth), ""), "\n") {
		if i > 0 {
			label = ""
		}
		lines = append(lines, labelled(label, l))
	}
	return lines
}

// detailLines describe the profile under the cursor: its description,
// settings and the shows that use it.
func (s *profilesScreen) detailLines(r profileRow, inner int) []string {
	if !r.ok {
		return []string{gFailed + " " + sRed.Render("The profile's text can't be read by this Garner; update it.")}
	}
	var lines []string
	if r.parsed.Description != "" {
		lines = append(lines, wrapLabelled("Description", sNormal.Render(r.parsed.Description), inner)...)
	}
	lines = append(lines, settingLines(r.parsed, inner)...)
	shows := sMuted.Render("none")
	if len(r.Shows) > 0 {
		var names []string
		for _, sh := range r.Shows {
			names = append(names, clean(sh))
		}
		shows = sNormal.Render(strings.Join(names, ", "))
	}
	return append(lines, wrapLabelled("Shows", shows, inner)...)
}

// settingLines are a profile's settings, each with what it means, within
// inner cells.
func settingLines(p profile.Profile, inner int) []string {
	lines := wrapLabelled("Audio", sNormal.Render(then(languageNames(p.Audio))), inner)
	lines = append(lines, wrapLabelled("Resolution", sNormal.Render(then(resolutionNames(p.Resolution))), inner)...)
	for _, o := range profileOptions {
		v := o.get(p)
		lines = append(lines, wrapLabelled(o.label, sNormal.Render(v)+sMuted.Render(" · "+o.explain(v)), inner)...)
	}
	return lines
}

// then joins a list best first: "German, then English".
func then(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", then ")
}

func languageNames(codes []string) []string {
	out := make([]string, len(codes))
	for i, c := range codes {
		out[i] = profile.LanguageName(c)
	}
	return out
}

func resolutionNames(res []int) []string {
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = fmt.Sprintf("%dp", r)
	}
	return out
}

// prefsLine lists a profile's preferences best first, as "German 1080p →
// German 720p".
func prefsLine(p profile.Profile) string {
	prefs := p.Preferences()
	parts := make([]string, len(prefs))
	for i, c := range prefs {
		parts[i] = c.String()
	}
	return strings.Join(parts, " → ")
}

// profileOption is one of a profile's settings with two values, each
// explained in a line.
type profileOption struct {
	label  string
	values [2]string
	means  [2]string
	get    func(profile.Profile) string
	set    func(*profile.Profile, string)
}

func (o profileOption) explain(value string) string {
	if value == o.values[1] {
		return o.means[1]
	}
	return o.means[0]
}

// other is the value that is not value.
func (o profileOption) other(value string) string {
	if value == o.values[0] {
		return o.values[1]
	}
	return o.values[0]
}

// profileOptions are the settings below the lists, in the editor's order.
var profileOptions = []profileOption{
	{
		label: "Priority", values: [2]string{string(profile.ByAudio), string(profile.ByResolution)},
		means: [2]string{"the language counts more than the resolution", "the resolution counts more than the language"},
		get:   func(p profile.Profile) string { return string(p.Priority) },
		set:   func(p *profile.Profile, v string) { p.Priority = profile.Priority(v) },
	},
	{
		label: "Dual audio", values: [2]string{string(profile.PreferDual), string(profile.IgnoreDual)},
		means: [2]string{"a release that also has the original audio ranks first", "a release that also has the original audio ranks no higher"},
		get:   func(p profile.Profile) string { return string(p.DualAudio) },
		set:   func(p *profile.Profile, v string) { p.DualAudio = profile.DualAudio(v) },
	},
	{
		label: "Unnamed audio", values: [2]string{string(profile.RejectUnnamed), string(profile.FallbackUnnamed)},
		means: [2]string{"a release marked dual or multi without named languages is rejected", "a release marked dual or multi without named languages is a last resort"},
		get:   func(p profile.Profile) string { return string(p.UnnamedAudio) },
		set:   func(p *profile.Profile, v string) { p.UnnamedAudio = profile.UnnamedAudio(v) },
	},
	{
		label: "Upgrades", values: [2]string{"on", "off"},
		means: [2]string{"a file below the 1st preference is replaced when a better release appears", "a file below the 1st preference stays when a better release appears"},
		get: func(p profile.Profile) string {
			if p.Upgrades {
				return "on"
			}
			return "off"
		},
		set: func(p *profile.Profile, v string) { p.Upgrades = v == "on" },
	},
}

func (s *profilesScreen) deleteLines(r profileRow) []string {
	return []string{
		gFailed + " " + sBold.Render("Delete "+r.Name+"?"),
		"",
		sNormal.Render("No show uses it."),
		"",
		"  " + choice("Cancel", s.choice == 0) + "   " + choice("Delete", s.choice == 1),
		"",
		keyBar("h l", "choose", "enter", "ok", "esc", "cancel"),
	}
}

// refusedLines say why the profile cannot be deleted, from what the
// screen knows; the server has the last word.
func (s *profilesScreen) refusedLines(r profileRow) []string {
	lines := []string{gFailed + " " + sBold.Render(r.Name+" can't be deleted"), ""}
	if n := len(r.Shows); n > 0 {
		if n == 1 {
			lines = append(lines, sNormal.Render("1 show uses it:"))
		} else {
			lines = append(lines, sNormal.Render(strconv.Itoa(n)+" shows use it:"))
		}
		for _, sh := range r.Shows {
			lines = append(lines, sNormal.Render("  "+clean(sh)))
		}
		lines = append(lines, "")
	}
	also := "It is "
	if len(r.Shows) > 0 {
		also = "It is also "
	}
	switch {
	case r.DefaultTV && r.DefaultAnime:
		lines = append(lines, sNormal.Render(also+"the profile for new TV and anime shows."), "")
	case r.DefaultTV:
		lines = append(lines, sNormal.Render(also+"the profile for new TV shows."), "")
	case r.DefaultAnime:
		lines = append(lines, sNormal.Render(also+"the profile for new anime shows."), "")
	}
	if len(r.Shows) > 0 {
		lines = append(lines, sNormal.Render("Give these shows another profile first."))
	} else {
		lines = append(lines, sNormal.Render("Choose another profile for new shows first."))
	}
	return append(lines, "", sSubtle.Render("esc or any key closes"))
}

func (s *profilesScreen) useForLines(r profileRow) []string {
	now := func(def func(api.Profile) bool) string {
		for _, o := range s.rows {
			if def(o.Profile) {
				if o.ID == r.ID {
					return "already"
				}
				return "now " + o.Name
			}
		}
		return ""
	}
	rows := []struct{ label, note string }{
		{"New TV shows", now(func(p api.Profile) bool { return p.DefaultTV })},
		{"New anime shows", now(func(p api.Profile) bool { return p.DefaultAnime })},
		{"Both", ""},
	}
	lines := []string{sBold.Render("Use " + r.Name + " for new shows"), ""}
	for i, row := range rows {
		label := " " + pad(row.label, 18)
		if i == s.choice {
			label = sNormal.Background(cSelect).Render(label)
		} else {
			label = sNormal.Render(label)
		}
		lines = append(lines, label+"  "+sMuted.Render(row.note))
	}
	return append(lines, "", keyBar("j k", "choose", "enter", "ok", "esc", "cancel"))
}

func (s *profilesScreen) bottom() string {
	switch {
	case s.busy != "":
		return sMuted.Render(s.busy)
	case s.flash.text != "":
		return s.flash.text
	}
	return keyBar("e", "edit", "a", "new", "d", "delete", "i", "import", "x", "export", "u", "use for new shows", "?", "help", "esc", "back")
}

func (s *profilesScreen) flashed() *flash { return &s.flash }

func (s *profilesScreen) captures() bool { return s.overlay != profilesNoOverlay || s.busy != "" }

func (s *profilesScreen) help() (string, []string) {
	return "Profiles", []string{
		"enter e", "edit the profile", "i", "import a profile from text or a link",
		"a", "new profile, empty or from another", "x", "export the profile to a file",
		"d", "delete the profile", "u", "use for new TV or anime shows",
	}
}

// pickItem is one row of a pickList: a choice, or a header the cursor
// skips. An empty header is a blank line.
type pickItem struct {
	label, note string
	header      bool
	value       any
}

// pickList is an overlay's list of choices with section headers, such as
// the new-profile picker and the language picker.
type pickList struct {
	items      []pickItem
	choice     int // index into items, never a header
	labelWidth int // the label column, gap included
	width      int // of a row, borders excluded
}

// key moves the cursor or answers: done closes the list, chosen is the
// index of the choice, or -1 when cancelled.
func (p *pickList) key(key string) (done bool, chosen int) {
	switch key {
	case "esc":
		return true, -1
	case "k", "up", "shift+tab":
		p.step(-1)
	case "j", "down", "tab":
		p.step(1)
	case "G", "end":
		p.choice = len(p.items)
		p.step(-1)
	case "enter":
		if p.choice >= 0 && p.choice < len(p.items) && !p.items[p.choice].header {
			return true, p.choice
		}
	}
	return false, -1
}

// step moves the cursor past headers in direction dir.
func (p *pickList) step(dir int) {
	for i := p.choice + dir; i >= 0 && i < len(p.items); i += dir {
		if !p.items[i].header {
			p.choice = i
			return
		}
	}
}

// lines draws the list for a screen height rows high. A list longer than
// fits shows a window around the cursor, with ↑ and ↓ where rows are
// hidden.
func (p *pickList) lines(title string, height int) []string {
	lines := []string{title, ""}
	first, last := 0, len(p.items)
	// The overlay's borders, the title, the key bar and their gaps take 6
	// rows; a window also takes one row for each mark.
	if rows := height - 6; len(p.items) > rows {
		shown := max(1, rows-2)
		first = min(max(0, p.choice-shown/2), len(p.items)-shown)
		last = first + shown
		lines = append(lines, sMuted.Render(mark(first > 0, " ↑")))
	}
	for i := first; i < last; i++ {
		it := p.items[i]
		switch {
		case it.header && it.label == "":
			lines = append(lines, "")
		case it.header:
			lines = append(lines, sSubtle.Render(it.label))
		default:
			label, note := sNormal, sMuted
			if i == p.choice {
				label, note = label.Background(cSelect), note.Background(cSelect)
			}
			noteWidth := max(0, p.width-1-p.labelWidth)
			lines = append(lines, label.Render(ansi.Truncate(" "+pad(it.label, p.labelWidth), p.width, ""))+
				note.Render(fit(it.note, noteWidth, false)))
		}
	}
	if first > 0 || last < len(p.items) {
		lines = append(lines, sMuted.Render(mark(last < len(p.items), " ↓")))
	}
	return append(lines, "", keyBar("j k", "choose", "enter", "ok", "esc", "cancel"))
}

// mark is s when shown, else an empty row of the same place.
func mark(shown bool, s string) string {
	if shown {
		return s
	}
	return ""
}
