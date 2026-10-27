package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/client"
	"github.com/trucke/garner/internal/profile"
)

// The profile editor changes a profile's name, description, lists and
// settings field by field, shows the preferences they make, and checks
// the profile as it is typed with the same check the server runs.

type editorRowKind int

const (
	rowName editorRowKind = iota
	rowDescription
	rowAudio      // index is the entry, -1 when the list is empty
	rowResolution // likewise
	rowOption     // index into profileOptions
)

type editorRow struct {
	kind  editorRowKind
	index int
}

// editorSavedMsg is the server's answer to saving.
type editorSavedMsg struct {
	from  *editorScreen
	saved api.Profile
	err   error
}

type editorScreen struct {
	sess     *session
	back     *profilesScreen
	id       int64  // the profile being replaced; 0 for a new one
	opened   string // the profile as opened, encoded, to tell changes
	p        profile.Profile
	shows    int // shows that follow the saved profile
	cursor   int
	typing   bool // the name or description is being typed
	input    textinput.Model
	picker   *pickList     // the language or resolution picker
	pickKind editorRowKind // rowAudio or rowResolution
	pickAt   int           // where the picked entry goes
	replace  bool          // the picked entry replaces the one at pickAt
	discard  bool          // the leave-without-saving question is open
	choice   int
	busy     string
	flash    flash
	server   []string // problems the server answered with
	scroll   scroller
}

// newEditor opens p: a stored profile when id is set, else a new one.
func newEditor(sess *session, back *profilesScreen, id int64, p profile.Profile, shows int) *editorScreen {
	// The lists are edited in place; the caller keeps its own.
	p.Audio, p.Resolution = slices.Clone(p.Audio), slices.Clone(p.Resolution)
	s := &editorScreen{sess: sess, back: back, id: id, p: p, shows: shows, opened: profile.Encode(p)}
	if p.Name == "" {
		s.cursor = 0
	} else {
		s.cursor = slices.IndexFunc(s.rows(), func(r editorRow) bool { return r.kind == rowAudio })
	}
	return s
}

func (s *editorScreen) init() tea.Cmd { return nil }

// rows are the lines the cursor can rest on.
func (s *editorScreen) rows() []editorRow {
	rows := []editorRow{{rowName, 0}, {rowDescription, 0}}
	if len(s.p.Audio) == 0 {
		rows = append(rows, editorRow{rowAudio, -1})
	}
	for i := range s.p.Audio {
		rows = append(rows, editorRow{rowAudio, i})
	}
	if len(s.p.Resolution) == 0 {
		rows = append(rows, editorRow{rowResolution, -1})
	}
	for i := range s.p.Resolution {
		rows = append(rows, editorRow{rowResolution, i})
	}
	for i := range profileOptions {
		rows = append(rows, editorRow{rowOption, i})
	}
	return rows
}

func (s *editorScreen) row() editorRow {
	rows := s.rows()
	s.cursor = min(max(0, s.cursor), len(rows)-1)
	return rows[s.cursor]
}

// changed reports whether there is something to save.
func (s *editorScreen) changed() bool { return profile.Encode(s.p) != s.opened }

// name is the profile's name for titles, or "New profile".
func (s *editorScreen) name() string {
	if s.p.Name == "" {
		return "New profile"
	}
	return s.p.Name
}

// check runs the shared check on the profile as it is: problems stop a
// save, warnings do not. A name another profile has is a problem too.
func (s *editorScreen) check() (problems, warnings []string) {
	_, warnings, err := profile.Parse(profile.Encode(s.p))
	var pe *profile.Error
	if errors.As(err, &pe) {
		problems = pe.Problems
	} else if err != nil {
		problems = []string{err.Error()}
	}
	for _, r := range s.back.rows {
		if r.ID != s.id && r.Name == s.p.Name && s.p.Name != "" {
			problems = append(problems, "name: a profile called "+r.Name+" already exists; choose another name")
		}
	}
	return append(problems, s.server...), warnings
}

func (s *editorScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case editorSavedMsg:
		if msg.from != s {
			return s, nil
		}
		s.busy = ""
		var serverErr *client.ServerError
		switch {
		case errors.Is(msg.err, client.ErrUnauthorized):
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		case errors.As(msg.err, &serverErr) && len(serverErr.Problems) > 0:
			s.server = serverErr.Problems
			s.flash.stay(gFailed + " " + sRed.Render("Not saved: the server found problems"))
		case errors.As(msg.err, &serverErr) && serverErr.Status == 409:
			s.server = []string{"name: " + clean(serverErr.Message)}
			s.flash.stay(gFailed + " " + sRed.Render("Not saved: "+clean(serverErr.Message)))
		case msg.err != nil:
			s.flash.stay(gFailed + " " + sRed.Render("Not saved: "+clean(msg.err.Error())))
		default:
			text := "Saved " + s.p.Name
			if s.id != 0 && s.shows > 0 {
				text += " · " + follows(s.shows)
			}
			back := s.back.returned(msg.saved.ID, gDone+" "+sNormal.Render(text))
			return s, func() tea.Msg { return switchMsg{back} }
		}
	case tea.PasteMsg:
		if s.typing {
			var cmd tea.Cmd
			s.input, cmd = s.input.Update(msg)
			return s, cmd
		}
	case tea.KeyPressMsg:
		if s.busy != "" {
			return s, nil
		}
		s.flash.clear()
		switch {
		case s.typing:
			return s, s.typingKey(msg)
		case s.picker != nil:
			return s, s.pickerKey(msg.String())
		case s.discard:
			return s, s.discardKey(msg.String())
		}
		return s, s.key(msg.String())
	}
	return s, nil
}

// follows says how many shows follow a changed profile.
func follows(n int) string {
	if n == 1 {
		return "1 show follows the new settings"
	}
	return strconv.Itoa(n) + " shows follow the new settings"
}

func (s *editorScreen) key(key string) tea.Cmd {
	row := s.row()
	switch key {
	case "esc":
		if !s.changed() {
			return func() tea.Msg { return switchMsg{s.back} }
		}
		s.discard, s.choice = true, 0
	case "j", "down":
		s.cursor = min(len(s.rows())-1, s.cursor+1)
	case "k", "up":
		s.cursor = max(0, s.cursor-1)
	case "G":
		s.cursor = len(s.rows()) - 1
	case "enter":
		return s.save()
	case "e":
		s.edit(row)
		if s.typing {
			return s.input.Focus()
		}
	case "h", "left", "l", "right":
		if row.kind == rowOption {
			o := profileOptions[row.index]
			if key == "l" || key == "right" {
				o.set(&s.p, o.values[1])
			} else {
				o.set(&s.p, o.values[0])
			}
			s.server = nil
		}
	case "a":
		if row.kind == rowResolution {
			s.openPicker(rowResolution, row.index+1, false)
		} else {
			at := len(s.p.Audio)
			if row.kind == rowAudio {
				at = row.index + 1
			}
			s.openPicker(rowAudio, at, false)
		}
	case "d":
		switch {
		case row.kind == rowAudio && row.index >= 0:
			s.p.Audio = slices.Delete(s.p.Audio, row.index, row.index+1)
		case row.kind == rowResolution && row.index >= 0:
			s.p.Resolution = slices.Delete(s.p.Resolution, row.index, row.index+1)
		default:
			return nil
		}
		// The cursor stays in the list, on the entry that took the place.
		s.cursor = slices.IndexFunc(s.rows(), func(r editorRow) bool { return r.kind == row.kind && r.index == min(row.index, s.listLen(row.kind)-1) })
		s.server = nil
	case "K":
		s.swap(row, -1)
	case "J":
		s.swap(row, 1)
	}
	return nil
}

// edit starts typing the name or description, opens the picker for a
// list entry, or flips an option.
func (s *editorScreen) edit(row editorRow) {
	switch row.kind {
	case rowName, rowDescription:
		s.input = textinput.New()
		s.input.Prompt = ""
		st := textinput.DefaultStyles(true)
		st.Cursor.Blink = false
		st.Cursor.Color = cFg
		st.Focused.Text = sNormal
		s.input.SetStyles(st)
		s.input.CharLimit = profile.MaxDescription
		if row.kind == rowName {
			s.input.CharLimit = profile.MaxName
			s.input.SetValue(s.p.Name)
		} else {
			s.input.SetValue(s.p.Description)
		}
		s.input.CursorEnd()
		s.typing = true
	case rowAudio, rowResolution:
		if row.index >= 0 {
			s.openPicker(row.kind, row.index, true)
		}
	case rowOption:
		o := profileOptions[row.index]
		o.set(&s.p, o.other(o.get(s.p)))
		s.server = nil
	}
}

// listLen is the length of the audio or resolution list.
func (s *editorScreen) listLen(kind editorRowKind) int {
	if kind == rowAudio {
		return len(s.p.Audio)
	}
	return len(s.p.Resolution)
}

// swap moves a list entry up or down.
func (s *editorScreen) swap(row editorRow, dir int) {
	i, j := row.index, row.index+dir
	switch {
	case row.kind == rowAudio && i >= 0 && j >= 0 && j < len(s.p.Audio):
		s.p.Audio[i], s.p.Audio[j] = s.p.Audio[j], s.p.Audio[i]
	case row.kind == rowResolution && i >= 0 && j >= 0 && j < len(s.p.Resolution):
		s.p.Resolution[i], s.p.Resolution[j] = s.p.Resolution[j], s.p.Resolution[i]
	default:
		return
	}
	s.cursor += dir
}

func (s *editorScreen) typingKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		value := strings.TrimSpace(s.input.Value())
		if s.row().kind == rowName {
			s.p.Name = value
		} else {
			s.p.Description = value
		}
		s.typing, s.server = false, nil
		return nil
	case "esc":
		s.typing = false
		return nil
	}
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(msg)
	return cmd
}

// openPicker lists the languages or resolutions the profile does not have
// yet, to put at index at, or in place of the entry there.
func (s *editorScreen) openPicker(kind editorRowKind, at int, replace bool) {
	p := &pickList{labelWidth: 20, width: 40}
	if kind == rowAudio {
		listed := func(code string) bool {
			return slices.Contains(s.p.Audio, code) && !(replace && s.p.Audio[at] == code)
		}
		if !listed(profile.Original) {
			p.items = append(p.items, pickItem{label: "Original language", note: "the show's own", value: profile.Original})
		}
		// By name, so German comes after French rather than after Danish (da).
		codes := profile.Languages()
		slices.SortFunc(codes, func(a, b string) int { return cmp.Compare(profile.LanguageName(a), profile.LanguageName(b)) })
		for _, code := range codes {
			if !listed(code) {
				p.items = append(p.items, pickItem{label: profile.LanguageName(code), value: code})
			}
		}
		if len(p.items) == 0 {
			s.flash.info(sMuted.Render("Every language Garner knows is listed already."))
			return
		}
	} else {
		for _, r := range profile.Resolutions {
			if !slices.Contains(s.p.Resolution, r) || replace && s.p.Resolution[at] == r {
				p.items = append(p.items, pickItem{label: fmt.Sprintf("%dp", r), value: r})
			}
		}
		if len(p.items) == 0 {
			s.flash.info(sMuted.Render("Both resolutions are listed already."))
			return
		}
	}
	s.picker, s.pickKind, s.pickAt, s.replace = p, kind, at, replace
}

func (s *editorScreen) pickerKey(key string) tea.Cmd {
	done, chosen := s.picker.key(key)
	if !done {
		return nil
	}
	picker := s.picker
	s.picker = nil
	if chosen < 0 {
		return nil
	}
	switch v := picker.items[chosen].value.(type) {
	case string:
		if s.replace {
			s.p.Audio[s.pickAt] = v
		} else {
			s.p.Audio = slices.Insert(s.p.Audio, s.pickAt, v)
			s.cursor = slices.IndexFunc(s.rows(), func(r editorRow) bool { return r.kind == rowAudio && r.index == s.pickAt })
		}
	case int:
		if s.replace {
			s.p.Resolution[s.pickAt] = v
		} else {
			s.p.Resolution = slices.Insert(s.p.Resolution, s.pickAt, v)
			s.cursor = slices.IndexFunc(s.rows(), func(r editorRow) bool { return r.kind == rowResolution && r.index == s.pickAt })
		}
	}
	s.server = nil
	return nil
}

func (s *editorScreen) discardKey(key string) tea.Cmd {
	switch key {
	case "esc":
		s.discard = false
	case "h", "left", "shift+tab":
		s.choice = 0
	case "l", "right", "tab":
		s.choice = 1
	case "enter":
		s.discard = false
		if s.choice == 1 {
			return func() tea.Msg { return switchMsg{s.back} }
		}
	}
	return nil
}

// save sends the profile to the server, unless the check finds problems.
func (s *editorScreen) save() tea.Cmd {
	if problems, _ := s.check(); len(problems) > 0 {
		word := "problem"
		if len(problems) > 1 {
			word = "problems"
		}
		s.flash.stay(gFailed + " " + sNormal.Render("Not saved: fix the "+word+" above first"))
		return nil
	}
	s.busy = "Saving " + s.p.Name + "…"
	cl, id, text := s.sess.client, s.id, profile.Encode(s.p)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var saved api.Profile
		var err error
		if id == 0 {
			saved, err = cl.CreateProfile(ctx, text)
		} else {
			saved, err = cl.ReplaceProfile(ctx, id, text)
		}
		return editorSavedMsg{s, saved, err}
	}
}

func (s *editorScreen) view(width, height int) string {
	row := s.row()
	var b strings.Builder
	b.WriteString(header(width, s.sess, "Garner", "Profiles", s.name()) + "\n\n")
	// A row is built from styled parts, each with the select background on
	// the cursor row, which then runs across the full width.
	type styler = func(lipgloss.Style) lipgloss.Style
	line := func(r editorRow, build func(bg styler) string) {
		bg := func(st lipgloss.Style) lipgloss.Style { return st }
		if r == row {
			bg = func(st lipgloss.Style) lipgloss.Style { return st.Background(cSelect) }
		}
		text := build(bg)
		b.WriteString(text + bg(lipgloss.NewStyle()).Render(strings.Repeat(" ", max(0, width-lipgloss.Width(text)))) + "\n")
	}
	label := func(bg styler, l string) string {
		return bg(lipgloss.NewStyle()).Render("   ") + bg(sSubtle).Render(pad(l, settingWidth))
	}
	text := func(r editorRow, l, v string) func(bg styler) string {
		return func(bg styler) string {
			if s.typing && r == row {
				// The field draws its own cursor.
				s.input.SetWidth(max(10, width-3-settingWidth-2))
				return label(bg, l) + s.input.View()
			}
			return label(bg, l) + bg(sNormal).Render(v)
		}
	}
	line(editorRow{rowName, 0}, text(editorRow{rowName, 0}, "Name", s.p.Name))
	line(editorRow{rowDescription, 0}, text(editorRow{rowDescription, 0}, "Description", s.p.Description))
	b.WriteString("\n")
	entry := func(l string, i int, v, note string) func(bg styler) string {
		return func(bg styler) string {
			if i > 0 {
				l = ""
			}
			out := label(bg, l) + bg(sSubtle).Render(strconv.Itoa(i+1)) + bg(lipgloss.NewStyle()).Render("  ") + bg(sNormal).Render(v)
			if note != "" {
				out += bg(lipgloss.NewStyle()).Render("  ") + bg(sMuted).Render(note)
			}
			return out
		}
	}
	none := func(l, hint string) func(bg styler) string {
		return func(bg styler) string { return label(bg, l) + bg(sMuted).Render(hint) }
	}
	if len(s.p.Audio) == 0 {
		line(editorRow{rowAudio, -1}, none("Audio", "none; press a to add a language"))
	}
	for i, code := range s.p.Audio {
		note := ""
		if code == profile.Original {
			note = "the show's own language"
		}
		line(editorRow{rowAudio, i}, entry("Audio", i, profile.LanguageName(code), note))
	}
	if len(s.p.Resolution) == 0 {
		line(editorRow{rowResolution, -1}, none("Resolution", "none; press a to add a resolution"))
	}
	for i, r := range s.p.Resolution {
		line(editorRow{rowResolution, i}, entry("Resolution", i, fmt.Sprintf("%dp", r), ""))
	}
	b.WriteString("\n")
	for i, o := range profileOptions {
		v := o.get(s.p)
		line(editorRow{rowOption, i}, func(bg styler) string {
			out := label(bg, o.label)
			cells := 0
			for j, c := range o.values {
				if j > 0 {
					out += bg(lipgloss.NewStyle()).Render("   ")
					cells += 3
				}
				st := sSubtle
				if c == v {
					st = sNormal
				}
				out += bg(st).Render(c)
				cells += lipgloss.Width(c)
			}
			// The explanations line up after the widest pair of values.
			const optionsWidth = 24
			return out + bg(lipgloss.NewStyle()).Render(strings.Repeat(" ", max(1, optionsWidth-cells))) + bg(sMuted).Render("· "+o.explain(v))
		})
	}
	b.WriteString("\n")
	prefs := sMuted.Render("none yet")
	if len(s.p.Audio) > 0 && len(s.p.Resolution) > 0 {
		prefs = sNormal.Render(prefsLine(s.p))
	}
	b.WriteString(indent(frame("preferences", []string{prefs}, min(width, designWidth)-6), 3) + "\n")
	problems, warnings := s.check()
	if len(problems)+len(warnings) > 0 {
		b.WriteString("\n")
	}
	for _, p := range problems {
		b.WriteString("   " + gFailed + " " + sRed.Render(clean(p)) + "\n")
	}
	for _, w := range warnings {
		b.WriteString("   " + sAmber.Render("!") + " " + sNormal.Render(clean(w)) + "\n")
	}
	page := s.scroll.layout(width, height, b.String(), s.bottom(row))
	switch {
	case s.picker != nil:
		return overlay(page, s.picker.lines(s.pickerTitle(), height), width, height)
	case s.discard:
		return overlay(page, s.discardLines(), width, height)
	}
	return page
}

// pickerTitle says what the picker adds and where: "Add a language  after
// English, as the 3rd".
func (s *editorScreen) pickerTitle() string {
	kind := "language"
	if s.pickKind == rowResolution {
		kind = "resolution"
	}
	if s.replace {
		return sBold.Render("Change the "+kind) + "  " + sMuted.Render("in place of the "+ordinal(s.pickAt+1))
	}
	where := "as the " + ordinal(s.pickAt+1)
	if s.pickAt > 0 {
		var before string
		if s.pickKind == rowResolution {
			before = fmt.Sprintf("%dp", s.p.Resolution[s.pickAt-1])
		} else {
			before = profile.LanguageName(s.p.Audio[s.pickAt-1])
		}
		where = "after " + before + ", " + where
	}
	return sBold.Render("Add a "+kind) + "  " + sMuted.Render(where)
}

func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return strconv.Itoa(n) + suffix
}

func (s *editorScreen) discardLines() []string {
	subject := s.p.Name
	if subject == "" {
		subject = "The new profile"
	}
	return []string{
		sBold.Render("Leave without saving?"),
		"",
		sNormal.Render(subject + " has unsaved changes."),
		"",
		"  " + choice("Keep editing", s.choice == 0) + "   " + choice("Discard", s.choice == 1),
		"",
		keyBar("h l", "choose", "enter", "ok", "esc", "cancel"),
	}
}

func (s *editorScreen) bottom(row editorRow) string {
	switch {
	case s.busy != "":
		return sMuted.Render(s.busy)
	case s.typing:
		return keyBar("enter", "ok", "esc", "cancel")
	case s.flash.text != "":
		return s.flash.text
	}
	add := "add language"
	if row.kind == rowResolution {
		add = "add resolution"
	}
	return keyBar("e", "edit", "h l", "choose", "a", add, "d", "remove", "K", "move up", "J", "move down", "enter", "save", "esc", "cancel")
}

func (s *editorScreen) flashed() *flash { return &s.flash }

// captures: the editor keeps every key while typing or a question is open.
func (s *editorScreen) captures() bool {
	return s.typing || s.picker != nil || s.discard || s.busy != ""
}

func (s *editorScreen) help() (string, []string) {
	return "Profile editor", []string{
		"e", "edit the field under the cursor", "h l", "choose a setting's value",
		"a", "add a language, or a resolution on those rows", "d", "remove the entry",
		"K J", "move the entry up or down", "enter", "save the profile",
		"esc", "leave; asks when there are changes",
	}
}
