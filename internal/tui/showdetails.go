package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/client"
)

// Show details describes a show in the library, with the delete, profile
// and monitor overlays. Its content follows Add show's details page.

type detailsOverlay int

const (
	noOverlay detailsOverlay = iota
	deleteOverlay
	profileOverlay
	monitorOverlay
)

type (
	detailChangedMsg struct {
		detail  api.SeriesDetail
		err     error
		success string
	}
	deleteResultMsg struct {
		deleted client.Deleted
		err     error
	}
	// deletedMsg returns to the library after a show was deleted.
	deletedMsg struct {
		name    string
		deleted client.Deleted
	}
)

type showDetailsScreen struct {
	sess    *session
	show    *showScreen
	back    screen // where esc goes: the show, or the library
	overlay detailsOverlay
	choice  int
	busy    string // what runs, such as "Refreshing from TVDB…"
	flash   flash
	scroll  scroller
	files   filesPrompt
}

func newShowDetails(sess *session, show *showScreen, back screen) *showDetailsScreen {
	return &showDetailsScreen{sess: sess, show: show, back: back}
}

// init loads the show unless its screen has already, TVDB's facts included.
func (s *showDetailsScreen) init() tea.Cmd {
	if s.show.detail == nil {
		return s.show.reload()
	}
	return nil
}

func (s *showDetailsScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case detailChangedMsg:
		s.busy = ""
		if errors.Is(msg.err, client.ErrUnauthorized) {
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		}
		if msg.err != nil {
			s.flash.stay(gFailed + " " + sRed.Render(clean(msg.err.Error())))
			return s, nil
		}
		s.show.setDetail(msg.detail)
		s.flash.info(gDone + " " + sNormal.Render(msg.success))
	case deleteResultMsg:
		s.busy = ""
		if errors.Is(msg.err, client.ErrUnauthorized) {
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		}
		if msg.err != nil {
			s.flash.stay(gFailed + " " + sRed.Render("Could not delete the show: "+clean(msg.err.Error())))
			return s, nil
		}
		name, deleted := s.name(), msg.deleted
		return s, func() tea.Msg { return deletedMsg{name, deleted} }
	case eventMsg:
		if msg.Type == "series.deleted" && s.busy != "" {
			// Our own delete: its result returns to the library and reports it.
			return s, nil
		}
		// The show screen keeps the episode states current.
		_, cmd := s.show.update(msg)
		return s, cmd
	case seriesMsg:
		// Answers the show screen asked for.
		_, cmd := s.show.update(msg)
		return s, cmd
	case takenOverMsg:
		s.busy = ""
		if errors.Is(msg.err, client.ErrUnauthorized) {
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		}
		if msg.err != nil {
			s.flash.stay(gFailed + " " + sRed.Render("Could not take over files: "+clean(msg.err.Error())))
			return s, nil
		}
		s.files.show(msg.result)
		return s, s.show.reload()
	case renamedMsg:
		s.busy = ""
		reportRenamed(msg, &s.flash)
		return s, s.show.reload()
	case openedMsg:
		if msg.err != nil {
			s.flash.stay(gFailed + " " + sRed.Render("Could not open the browser: "+clean(msg.err.Error())))
		}
	case tea.KeyPressMsg:
		if s.busy != "" {
			return s, nil
		}
		s.flash.clear()
		if s.files.open {
			if s.files.key(msg.String()) {
				s.busy = "Renaming and moving " + plural(s.files.result.Renamable, "file") + "…"
				return s, renameFiles(s.sess.client, s.show.id)
			}
			return s, nil
		}
		if s.overlay != noOverlay {
			return s, s.overlayKey(msg.String())
		}
		return s, s.key(msg.String())
	}
	return s, nil
}

func (s *showDetailsScreen) name() string { return clean(s.show.name) }

func (s *showDetailsScreen) key(key string) tea.Cmd {
	d := s.show.detail
	if key == "esc" {
		return func() tea.Msg { return switchMsg{s.back} }
	}
	if d == nil {
		return nil // still loading
	}
	if s.scroll.key(key) {
		return nil
	}
	switch key {
	case "p":
		// The cursor starts on the show's type: enter there changes nothing.
		s.overlay, s.choice = profileOverlay, typeIndex(d.Series.Type)
	case "m":
		s.overlay, s.choice = monitorOverlay, max(0, monitorIndex(d.Series.Monitor))
	case "d":
		s.overlay, s.choice = deleteOverlay, 0 // the safe choice: cancel
	case "o":
		return openLink(d.URL)
	case "t":
		s.busy = "Looking for " + s.name() + "'s files in the library…"
		return takeOverFiles(s.sess.client, s.show.id)
	case "r":
		s.busy = "Refreshing " + s.name() + " from TVDB…"
		cl, id := s.sess.client, s.show.id
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			d, err := cl.Refresh(ctx, id)
			return detailChangedMsg{d, err, "Refreshed from TVDB"}
		}
	}
	return nil
}

func (s *showDetailsScreen) overlayKey(key string) tea.Cmd {
	d := s.show.detail
	if s.overlay == deleteOverlay {
		done, a := deleteKey(key, d.Summary, &s.choice)
		if done {
			s.overlay = noOverlay
		}
		if !a.delete {
			return nil
		}
		s.busy = a.busy(s.name())
		return deleteShow(s.sess.client, s.show.id, a.keep)
	}
	if s.overlay == monitorOverlay {
		done, mode := monitorKey(key, &s.choice)
		if done {
			s.overlay = noOverlay
		}
		if mode == "" || mode == d.Series.Monitor {
			return nil
		}
		s.busy = "Changing which episodes Garner gets…"
		cl, id := s.sess.client, s.show.id
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			d, err := cl.SetMonitor(ctx, id, mode)
			return detailChangedMsg{d, err, "Monitor: " + monitorLabel(mode)}
		}
	}
	// The profile and type overlay: the two types, then the profiles.
	options := len(showTypes) + len(d.Profiles)
	switch key {
	case "esc":
		s.overlay = noOverlay
	case "k", "up", "shift+tab":
		s.choice = max(0, s.choice-1)
	case "j", "down", "tab":
		s.choice = min(options-1, s.choice+1)
	case "G":
		s.choice = options - 1
	case "P":
		s.overlay = noOverlay
		next := newProfiles(s.sess, s)
		return func() tea.Msg { return switchMsg{next} }
	case "enter":
		s.overlay = noOverlay
		cl, id := s.sess.client, s.show.id
		switch {
		case s.choice < len(showTypes) && showTypes[s.choice].typ != d.Series.Type:
			t := showTypes[s.choice]
			s.busy = "Changing the type…"
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				d, err := cl.SetType(ctx, id, t.typ)
				return detailChangedMsg{d, err, "Type: " + t.label}
			}
		case s.choice >= len(showTypes) && s.choice-len(showTypes) < len(d.Profiles) && d.Profiles[s.choice-len(showTypes)].ID != d.Series.ProfileID:
			profile := d.Profiles[s.choice-len(showTypes)]
			s.busy = "Changing the profile…"
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				d, err := cl.SetProfile(ctx, id, profile.ID)
				return detailChangedMsg{d, err, "Profile changed to " + profile.Name}
			}
		}
	}
	return nil
}

// showType is a type a show can have, with what it means for the
// numbering: on the details page, and in the overlay's row.
type showType struct{ typ, label, numbering, means string }

var showTypes = []showType{
	{api.TypeTV, "TV", "season and episode numbers, S01E05", "season and episode numbers, S01E05"},
	{api.TypeAnime, "Anime", "a bare number such as 101 is an absolute episode first", "101 is read as absolute episode 101 first"},
}

// typeIndex is a type's row in showTypes; an unknown type counts as TV.
func typeIndex(typ string) int {
	return max(0, slices.IndexFunc(showTypes, func(t showType) bool { return t.typ == typ }))
}

func (s *showDetailsScreen) view(width, height int) string {
	d := s.show.detail
	var b strings.Builder
	b.WriteString(header(width, s.sess, "Garner", "Library", s.name(), "Details") + "\n\n")
	switch {
	case d == nil && s.show.err != nil:
		b.WriteString("   " + gFailed + " " + sRed.Render(clean(s.show.err.Error())))
		return screenLayout(width, height, b.String(), keyBar("esc", "back"))
	case d == nil:
		b.WriteString("   " + sMuted.Render("Loading "+s.name()+"…"))
		return screenLayout(width, height, b.String(), keyBar("esc", "back"))
	}
	// The panes use the full width, so the preferences fit on one line.
	w := max(40, min(width, designWidth)-6)
	sr := d.Series
	b.WriteString(titleBlock(sr.Name, sr.NameEng, sr.NameDeu, showTypes[typeIndex(sr.Type)].label, sr.Year, sr.Status) + "\n")
	b.WriteString(indent(frame("library", s.libraryLines(w-6), w), 3) + "\n")

	tvdb := tvdbLines(seriesFacts(d), w-6)
	if !sr.HasDetails {
		// Loaded before these facts were kept; the daily sync loads them.
		tvdb = append(tvdb, sMuted.Render("More facts come with the next daily sync from TVDB, or now with ")+sKey.Render("r")+sMuted.Render("."))
	}
	b.WriteString("\n" + indent(frame("tvdb", tvdb, w), 3) + "\n")
	if sr.Overview != "" {
		b.WriteString("\n" + indent(frame(overviewLabel(sr.OverviewLanguage), []string{sNormal.Render(clean(sr.Overview))}, w), 3) + "\n")
	}

	page := s.scroll.layout(width, height, b.String(), s.bottom())
	if s.files.open {
		return overlay(page, s.files.lines(s.name()), width, height)
	}
	switch s.overlay {
	case deleteOverlay:
		return overlay(page, s.deleteLines(), width, height)
	case profileOverlay:
		return overlay(page, s.profileLines(), width, height)
	case monitorOverlay:
		return overlay(page, monitorQuestion("Monitor "+s.name(), s.choice, s.show.detail.MonitorOff, "ok"), width, height)
	}
	return page
}

func (s *showDetailsScreen) libraryLines(inner int) []string {
	d := s.show.detail
	var lines []string
	t := showTypes[typeIndex(d.Series.Type)]
	typeLine := sNormal.Render(t.label) + sMuted.Render(" · "+t.numbering)
	if root := d.TypeFolders[t.typ]; root != "" {
		typeLine += sMuted.Render(" · files under " + clean(root))
	}
	// Wrapped under itself, as field does; the line mixes two styles.
	for i, l := range strings.Split(ansi.Wrap(typeLine, max(10, inner-labelWidth), ""), "\n") {
		head := strings.Repeat(" ", labelWidth)
		if i == 0 {
			head = sSubtle.Render(pad("Type", labelWidth))
		}
		lines = append(lines, head+l)
	}
	lines = append(lines, field("Profile", d.Profile, sNormal, inner)...)
	lines = append(lines, field("", clean(d.Rules), sMuted, inner)...)
	lines = append(lines, field("Monitor", monitorLine(d.Series), sNormal, inner)...)
	if d.MonitorOff != "" && d.Series.Monitor != api.MonitorOff {
		lines = append(lines, field("", "The monitor is "+clean(d.MonitorOff)+".", sMuted, inner)...)
	}
	lines = append(lines, field("Original", clean(d.LanguageName), sNormal, inner)...)
	lines = append(lines, field("", clean(d.LanguageNote), sMuted, inner)...)
	for i, l := range wrapPath(clean(d.Folder), inner-labelWidth) {
		head := strings.Repeat(" ", labelWidth)
		if i == 0 {
			head = sSubtle.Render(pad("Folder", labelWidth))
		}
		lines = append(lines, head+sNormal.Render(l))
	}
	episodes := fmt.Sprintf("%d of %d in the library", d.Summary.Imported, d.Summary.Aired)
	if b := size(d.Summary.Bytes); b != "" {
		episodes += " · " + b
	}
	lines = append(lines, field("Episodes", episodes, sNormal, inner)...)
	lines = append(lines, field("Added", d.Series.Added.Local().Format("2 Jan 2006"), sNormal, inner)...)
	lines = append(lines, field("Updated", "from TVDB "+ago(d.Series.Fetched), sNormal, inner)...)
	return lines
}

// monitorLine says which episodes the monitor gets, such as "new episodes,
// those airing from 17 Oct 2026 on".
func monitorLine(sr api.Series) string {
	switch sr.Monitor {
	case api.MonitorNew:
		return "new episodes, those airing from " + sr.MonitorSince.Local().Format("2 Jan 2006") + " on"
	case api.MonitorAll:
		return "all missing episodes"
	case api.MonitorOff:
		return "off; Garner gets only what you search for"
	}
	return clean(sr.Monitor)
}

func (s *showDetailsScreen) deleteLines() []string {
	return deleteQuestion(s.name(), s.show.detail.Summary, s.choice)
}

// deleteAnswer is a choice in the delete question.
type deleteAnswer struct {
	label  string
	delete bool // false: cancel
	keep   bool // leave the files in the library
}

// deleteAnswers are the choices in the delete question, from the library
// or the details page. Cancel, the safe one, comes first and is
// preselected; keeping the files is offered only when there are files.
func deleteAnswers(sh api.Show) []deleteAnswer {
	if sh.Bytes > 0 {
		return []deleteAnswer{{label: "Cancel"}, {"Keep files", true, true}, {"Delete files", true, false}}
	}
	return []deleteAnswer{{label: "Cancel"}, {label: "Delete", delete: true}}
}

func (a deleteAnswer) busy(name string) string {
	if a.keep {
		return "Removing " + name + "…"
	}
	return "Deleting " + name + "…"
}

// deleteKey moves the choice in the delete question about sh or answers
// it. done closes the question; the answer says whether and how to delete.
func deleteKey(key string, sh api.Show, chosen *int) (done bool, answer deleteAnswer) {
	answers := deleteAnswers(sh)
	switch key {
	case "esc":
		return true, deleteAnswer{}
	case "h", "left", "shift+tab":
		*chosen = max(0, *chosen-1)
	case "l", "right", "tab":
		*chosen = min(len(answers)-1, *chosen+1)
	case "enter":
		if *chosen < len(answers) {
			answer = answers[*chosen]
		}
		return true, answer
	}
	return false, deleteAnswer{}
}

// deleteQuestion is the delete overlay.
func deleteQuestion(name string, sh api.Show, chosen int) []string {
	var body []string
	if sh.Bytes > 0 {
		body = append(body, sNormal.Render("Files: "+size(sh.Bytes)))
	}
	if sh.Counts[api.EpisodeDownloading]+sh.Counts[api.EpisodeProcessing] > 0 {
		body = append(body, sNormal.Render("Running downloads are cancelled."))
	}
	lines := []string{gFailed + " " + sBold.Render("Delete "+name+"?")}
	if len(body) > 0 {
		lines = append(append(lines, ""), body...)
	}
	var row []string
	for i, a := range deleteAnswers(sh) {
		row = append(row, choice(a.label, i == chosen))
	}
	return append(lines, "",
		"  "+strings.Join(row, "   "),
		"",
		keyBar("h l", "choose", "enter", "ok", "esc", "cancel"))
}

// deleteShow deletes a show, with its files unless keepFiles is set.
func deleteShow(cl *client.Client, id int64, keepFiles bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		deleted, err := cl.DeleteSeries(ctx, id, keepFiles)
		return deleteResultMsg{deleted, err}
	}
}

// profileLines is the profile and type overlay: the types with what each
// means, then the profiles with their preferences for this show. The
// show's own type and profile are in the text colour, the others subtle;
// the cursor row has the select background.
func (s *showDetailsScreen) profileLines() []string {
	d := s.show.detail
	const nameWidth, typeWidth = 29, 8
	width := 92
	typeNote := func(t showType) string {
		if root := d.TypeFolders[t.typ]; root != "" {
			return t.means + " · new files under " + clean(root)
		}
		return t.means
	}
	for _, p := range d.Profiles {
		width = max(width, 1+nameWidth+lipgloss.Width(clean(p.Preferences))+1)
	}
	for _, t := range showTypes {
		width = max(width, 1+typeWidth+lipgloss.Width(typeNote(t))+1)
	}
	width = min(width, designWidth-8)
	row := func(i int, current bool, label string, labelWidth int, note string) string {
		st, noteSt := sSubtle, sMuted
		if current {
			st = sNormal
		}
		if i == s.choice {
			st, noteSt = st.Background(cSelect), noteSt.Background(cSelect)
		}
		return st.Render(" "+pad(label, labelWidth)) + noteSt.Render(fit(note, width-1-labelWidth, false))
	}
	lines := []string{sBold.Render("Profile and type of " + s.name()), "", sSubtle.Render("Type")}
	for i, t := range showTypes {
		lines = append(lines, row(i, t.typ == d.Series.Type, t.label, typeWidth, typeNote(t)))
	}
	lines = append(lines, "", sSubtle.Render("Profile"))
	for i, p := range d.Profiles {
		lines = append(lines, row(len(showTypes)+i, p.ID == d.Series.ProfileID, clean(p.Name), nameWidth, clean(p.Preferences)))
	}
	return append(lines, "",
		sMuted.Render("The type sets the numbering and the folder for new files;"),
		sMuted.Render("files already in the library stay where they are."),
		sMuted.Render("Changing the profile clears earlier search results;"),
		sMuted.Render("the next search judges releases by the new rules."),
		"",
		keyBar("j k", "choose", "enter", "ok", "P", "manage profiles", "esc", "cancel"))
}

func (s *showDetailsScreen) captures() bool {
	return s.overlay != noOverlay || s.files.open || s.busy != ""
}

func (s *showDetailsScreen) help() (string, []string) {
	return "Show details", []string{
		"p", "change the profile or the type", "m", "choose the episodes Garner gets",
		"r", "refresh from TVDB", "o", "open on TVDB",
		"t", "take over the show's files in the library", "d", "delete the show",
	}
}

func (s *showDetailsScreen) bottom() string {
	switch {
	case s.busy != "":
		return sMuted.Render(s.busy)
	case s.flash.text != "":
		return s.flash.text
	}
	return keyBar("p", "profile and type", "m", "monitor", "r", "refresh", "o", "open on TVDB", "t", "take over", "d", "delete", "?", "help", "esc", "back")
}

// titleBlock is the heading of a details page: the English name, the
// German name when it differs, the original title when it differs from
// both, then type, year and status.
func titleBlock(name, nameEng, nameDeu, typ string, year int, st string) string {
	title := sBold.Render(clean(cmp.Or(nameEng, name)))
	if nameDeu != "" && nameDeu != cmp.Or(nameEng, name) {
		title += "  " + sMuted.Render(clean(nameDeu))
	}
	lines := []string{"   " + title}
	if nameEng != "" && name != nameEng && name != nameDeu {
		// TVDB's main name is the original title, such as Japanese.
		lines = append(lines, "   "+sMuted.Render(clean(name)))
	}
	facts := []string{typ}
	if year > 0 {
		facts = append(facts, fmt.Sprint(year))
	}
	if st := status(st); st != "" {
		facts = append(facts, st)
	}
	lines = append(lines, "   "+sMuted.Render(strings.Join(facts, " · ")))
	return strings.Join(lines, "\n") + "\n"
}

func overviewLabel(language string) string {
	if language != "" && language != "eng" {
		return "overview (" + langCode(language) + ")"
	}
	return "overview"
}

// seriesFacts are TVDB's facts as Garner stored them: episodes and seasons
// of the default order without specials, the default order first.
func seriesFacts(d *api.SeriesDetail) tvdbFacts {
	sr := d.Series
	f := tvdbFacts{network: sr.Network, status: sr.Status, firstAired: sr.FirstAired, lastAired: sr.LastAired,
		runtime: sr.AverageRuntime, url: d.URL}
	seasons := map[int]bool{}
	for _, e := range d.Episodes {
		if pos, ok := e.Numbers[sr.DefaultOrder]; ok && pos.Season > 0 {
			f.episodes++
			seasons[pos.Season] = true
		}
	}
	f.seasons = len(seasons)
	for _, o := range d.Orders {
		name := o.Name
		if o.AlternateName != "" {
			name += " (" + o.AlternateName + ")"
		}
		if o.Type == sr.DefaultOrder {
			f.orders = append([]string{name}, f.orders...)
		} else {
			f.orders = append(f.orders, name)
		}
	}
	return f
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < 2*time.Minute:
		return "a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 2*time.Hour:
		return "an hour ago"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	}
	return "on " + t.Local().Format("2 Jan 2006")
}

// wrapPath breaks a path after its slashes, so no line ends inside a
// folder name; paths are never cut. A single name longer than the line
// breaks where it must.
func wrapPath(path string, width int) []string { return wrapAfter(path, "/", width) }

// wrapAfter breaks s after each sep where the line is full, such as a file
// name after its dots.
func wrapAfter(s, sep string, width int) []string {
	width = max(10, width)
	var lines []string
	line := ""
	for _, part := range strings.SplitAfter(s, sep) {
		if line != "" && ansi.StringWidth(line+part) > width {
			lines = append(lines, line)
			line = ""
		}
		line += part
		for ansi.StringWidth(line) > width {
			lines = append(lines, ansi.Truncate(line, width, ""))
			line = ansi.TruncateLeft(line, width, "")
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

func (s *showDetailsScreen) flashed() *flash { return &s.flash }
