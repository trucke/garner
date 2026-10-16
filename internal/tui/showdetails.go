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
	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/client"
	"github.com/trucke/garner/internal/server"
	"github.com/trucke/garner/internal/store"
)

// Show details describes a show in the library, with the delete and
// profile overlays. Boards "Show · details …" in the Paper file. Its
// content follows what was agreed for Add show's details page.

type detailsOverlay int

const (
	noOverlay detailsOverlay = iota
	deleteOverlay
	profileOverlay
)

type (
	detailChangedMsg struct {
		detail  server.SeriesDetail
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
	case openedMsg:
		if msg.err != nil {
			s.flash.stay(gFailed + " " + sRed.Render("Could not open the browser: "+clean(msg.err.Error())))
		}
	case tea.KeyPressMsg:
		if s.busy != "" {
			return s, nil
		}
		s.flash.clear()
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
		s.overlay, s.choice = profileOverlay, max(0, slices.IndexFunc(d.Profiles, func(p server.ProfileInfo) bool { return p.Name == d.Series.Profile }))
	case "d":
		s.overlay, s.choice = deleteOverlay, 0 // the safe choice: cancel
	case "o":
		return openLink(d.URL)
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
	options := len(d.Profiles)
	switch key {
	case "esc":
		s.overlay = noOverlay
	case "h", "left", "k", "up", "shift+tab":
		s.choice = max(0, s.choice-1)
	case "l", "right", "j", "down", "tab":
		s.choice = min(options-1, s.choice+1)
	case "enter":
		overlay := s.overlay
		s.overlay = noOverlay
		cl, id := s.sess.client, s.show.id
		switch {
		case overlay == profileOverlay && s.choice < len(d.Profiles) && d.Profiles[s.choice].Name != d.Series.Profile:
			profile := d.Profiles[s.choice].Name
			s.busy = "Changing the profile…"
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				d, err := cl.SetProfile(ctx, id, profile)
				return detailChangedMsg{d, err, "Profile changed to " + profile}
			}
		}
	}
	return nil
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
	w := max(40, min(detailsWidth, width-6))
	sr := d.Series
	typ := "TV"
	if sr.Profile == "anime" {
		typ = "Anime"
	}
	b.WriteString(titleBlock(sr.Name, sr.NameEng, sr.NameDeu, typ, sr.Year, sr.Status) + "\n")
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
	switch s.overlay {
	case deleteOverlay:
		return overlay(page, s.deleteLines(), width, height)
	case profileOverlay:
		return overlay(page, s.profileLines(), width, height)
	}
	return page
}

func (s *showDetailsScreen) libraryLines(inner int) []string {
	d := s.show.detail
	var lines []string
	lines = append(lines, field("Profile", d.Series.Profile, sNormal, inner)...)
	lines = append(lines, field("", clean(d.Rules), sMuted, inner)...)
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
func deleteAnswers(sh store.Show) []deleteAnswer {
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
func deleteKey(key string, sh store.Show, chosen *int) (done bool, answer deleteAnswer) {
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
func deleteQuestion(name string, sh store.Show, chosen int) []string {
	var body []string
	if sh.Bytes > 0 {
		body = append(body, sNormal.Render("Files: "+size(sh.Bytes)))
	}
	if sh.Counts[store.EpisodeDownloading]+sh.Counts[store.EpisodeProcessing] > 0 {
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

func (s *showDetailsScreen) profileLines() []string {
	d := s.show.detail
	lines := []string{sBold.Render("Profile for " + s.name()), ""}
	nameWidth := 0
	for _, p := range d.Profiles {
		nameWidth = max(nameWidth, len(p.Name))
	}
	for i, p := range d.Profiles {
		row := pad(p.Name, nameWidth) + "   " + clean(p.Rules)
		if i == s.choice {
			lines = append(lines, sNormal.Background(cSelect).Render(" "+row+" "))
		} else {
			lines = append(lines, sNormal.Render(" "+row+" "))
		}
	}
	return append(lines, "",
		sMuted.Render("Changing the profile clears earlier search results;"),
		sMuted.Render("the next search judges releases by the new rules."),
		"",
		keyBar("j k", "choose", "enter", "ok", "esc", "cancel"))
}

func (s *showDetailsScreen) captures() bool { return s.overlay != noOverlay || s.busy != "" }

func (s *showDetailsScreen) help() (string, []string) {
	return "Show details", []string{
		"p", "change the profile", "r", "refresh from TVDB",
		"o", "open on TVDB", "d", "delete the show",
	}
}

func (s *showDetailsScreen) bottom() string {
	switch {
	case s.busy != "":
		return sMuted.Render(s.busy)
	case s.flash.text != "":
		return s.flash.text
	}
	return keyBar("p", "change profile", "r", "refresh from TVDB", "o", "open on TVDB", "d", "delete show", "?", "help", "esc", "back")
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
func seriesFacts(d *server.SeriesDetail) tvdbFacts {
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
