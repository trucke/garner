package tui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/client"
)

// Library lists the shows with what Garner has of each.

type category int

const (
	catAll category = iota
	catTV
	catAnime
	catMovies
)

type sortKey int

const (
	byState sortKey = iota
	byName
	byEpisodes
	bySize
	byStatus
	sortKeys
)

// libraryMsg answers one library's reload; from tells the app which.
type libraryMsg struct {
	from  *libraryScreen
	shows []api.Show
	err   error
}

type libraryScreen struct {
	sess     *session
	loaded   bool
	loading  bool
	dirty    bool // an event arrived while loading; load again
	cat      category
	sort     sortKey
	reverse  bool // the sort column's order flipped
	filter   string
	typing   bool // the filter prompt has the keys
	selected int64
	offset   int
	page     int // table rows on screen, from the last view
	flash    flash
	lastKey  string
	// The delete question for the show under the cursor, and the show
	// being deleted.
	asking   bool
	choice   int
	deleting string // its name
	busy     string // what runs, such as "Deleting Harbor Precinct…"
}

func newLibrary(sess *session) *libraryScreen {
	return &libraryScreen{sess: sess, page: 20}
}

// init loads the library when returning from another screen. On the
// first start it waits for the event stream: connecting loads it.
func (s *libraryScreen) init() tea.Cmd {
	if !s.loaded {
		return nil
	}
	return s.reload()
}

// showAdded selects a show just added, wherever the filter and tabs were,
// for when the user returns from the show.
func (s *libraryScreen) showAdded(msg addedMsg) tea.Cmd {
	s.cat, s.filter, s.typing, s.selected = catAll, "", false, msg.id
	return s.reload()
}

// showDeleted reports a deleted show.
func (s *libraryScreen) showDeleted(msg deletedMsg) tea.Cmd {
	text := "Deleted " + msg.name
	switch {
	case msg.deleted.Kept > 0:
		text = fmt.Sprintf("Removed %s; its %s, %s, stay in the library", msg.name, plural(msg.deleted.Kept, "file"), size(msg.deleted.Bytes))
	case msg.deleted.Files > 0:
		text += fmt.Sprintf(" and %s, %s", plural(msg.deleted.Files, "file"), size(msg.deleted.Bytes))
	}
	s.flash.info(gDone + " " + sNormal.Render(text))
	if n := len(msg.deleted.Problems); n > 0 {
		s.flash.stay(gFailed + " " + sNormal.Render(text+fmt.Sprintf("; %d files could not be removed: ", n)) +
			sRed.Render(clean(msg.deleted.Problems[0])))
	}
	return s.reload()
}

func (s *libraryScreen) reload() tea.Cmd {
	if s.loading {
		s.dirty = true
		return nil
	}
	s.loading = true
	cl := s.sess.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		shows, err := cl.Library(ctx)
		return libraryMsg{s, shows, err}
	}
}

func (s *libraryScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case libraryMsg:
		s.loading = false
		switch {
		case errors.Is(msg.err, client.ErrUnauthorized):
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		case msg.err != nil:
			if !s.sess.offline {
				s.flash.stay(sRed.Render(clean(msg.err.Error())))
			}
		default:
			s.sess.shows, s.loaded = msg.shows, true
		}
		if s.dirty {
			s.dirty = false
			return s, s.reload()
		}
		var waiting []*api.Download
		for _, sh := range s.sess.shows {
			if sh.Focus != nil {
				waiting = append(waiting, sh.Focus.Download)
			}
		}
		return s, redrawWhenSlow(waiting...)
	case eventMsg:
		switch msg.Type {
		case client.EventConnected, "series.updated", "series.deleted", "search.finished":
			return s, s.reload()
		case "download.updated":
			var d api.Download
			if json.Unmarshal(msg.Data, &d) != nil || !s.patch(d) {
				return s, s.reload()
			}
		}
	case deleteResultMsg:
		name := s.deleting
		s.deleting, s.busy = "", ""
		switch {
		case errors.Is(msg.err, client.ErrUnauthorized):
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		case msg.err != nil:
			s.flash.stay(gFailed + " " + sRed.Render("Could not delete "+name+": "+clean(msg.err.Error())))
			return s, nil
		}
		return s, s.showDeleted(deletedMsg{name, msg.deleted})
	case tea.PasteMsg:
		if s.typing {
			s.filter += clean(msg.Content)
		}
	case tea.KeyPressMsg:
		return s, s.key(msg)
	}
	return s, nil
}

// patch updates a show's running download in place when only its progress
// changed. It reports false when the library needs loading again.
func (s *libraryScreen) patch(d api.Download) bool {
	for _, sh := range s.sess.shows {
		if f := sh.Focus; f != nil && f.Download != nil && f.Download.ID == d.ID {
			if f.Download.State != d.State || f.Download.Import.State != d.Import.State {
				return false
			}
			f.Download = &d
			return true
		}
	}
	// A download the summary does not show yet, such as a new one.
	return false
}

func (s *libraryScreen) key(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	if s.deleting != "" {
		return nil
	}
	s.flash.clear()
	if s.asking {
		return s.answer(key)
	}
	if s.typing {
		switch key {
		case "enter":
			s.typing = false
		case "esc":
			s.typing, s.filter = false, ""
		case "backspace":
			if r := []rune(s.filter); len(r) > 0 {
				s.filter = string(r[:len(r)-1])
			}
		case "ctrl+u":
			s.filter = ""
		case "up":
			s.move(-1)
		case "down":
			s.move(1)
		default:
			if msg.Text != "" && msg.Mod == 0 || msg.Text != "" && msg.Mod == tea.ModShift {
				s.filter += clean(msg.Text)
			}
		}
		return nil
	}

	prev := s.lastKey
	s.lastKey = key
	switch key {
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
	case "tab":
		s.cat = (s.cat + 1) % 4
	case "shift+tab":
		s.cat = (s.cat + 3) % 4
	case "1", "2", "3", "4":
		s.cat = category(key[0] - '1')
	case "s":
		s.sort, s.reverse = (s.sort+1)%sortKeys, false
	case "I":
		s.reverse = !s.reverse
	case "/":
		s.typing = true
	case "esc":
		s.filter = ""
	case "enter":
		if sh, ok := s.cursorShow(); ok {
			next := newShow(s.sess, s, sh.ID, showName(sh), "")
			return func() tea.Msg { return switchMsg{next} }
		}
	case "a":
		next := newAddShow(s.sess, s)
		return func() tea.Msg { return switchMsg{next} }
	case "i":
		if sh, ok := s.cursorShow(); ok {
			next := newShowDetails(s.sess, newShow(s.sess, s, sh.ID, showName(sh), ""), s)
			return func() tea.Msg { return switchMsg{next} }
		}
	case "d":
		if _, ok := s.cursorShow(); ok {
			s.asking, s.choice = true, 0 // the safe choice: cancel
		}
	case "q":
		return tea.Quit
	}
	return nil
}

// answer handles the delete question.
func (s *libraryScreen) answer(key string) tea.Cmd {
	sh, ok := s.cursorShow()
	if !ok {
		s.asking = false
		return nil
	}
	done, a := deleteKey(key, sh, &s.choice)
	if done {
		s.asking = false
	}
	if !a.delete {
		return nil
	}
	s.deleting, s.busy = showName(sh), a.busy(showName(sh))
	return deleteShow(s.sess.client, sh.ID, a.keep)
}

// cursorShow is the show under the cursor, if any is shown.
func (s *libraryScreen) cursorShow() (api.Show, bool) {
	rows, _ := s.rows()
	i := slices.IndexFunc(rows, func(sh api.Show) bool { return sh.ID == s.selected })
	if i < 0 {
		return api.Show{}, false
	}
	return rows[i], true
}

// move shifts the cursor by n rows within the visible rows.
func (s *libraryScreen) move(n int) {
	rows, _ := s.rows()
	if len(rows) == 0 {
		return
	}
	i := max(0, slices.IndexFunc(rows, func(sh api.Show) bool { return sh.ID == s.selected }))
	s.selected = rows[min(len(rows)-1, max(0, i+n))].ID
}

// rows are the shows of the current category that match the filter, in
// the chosen order; total counts the category.
func (s *libraryScreen) rows() (rows []api.Show, total int) {
	needle := []rune(strings.ToLower(s.filter))
	for _, sh := range s.sess.shows {
		if !s.cat.holds(sh) {
			continue
		}
		total++
		if len(needle) == 0 || fuzzy(needle, sh.Name) || fuzzy(needle, sh.NameEng) || fuzzy(needle, sh.NameDeu) {
			rows = append(rows, sh)
		}
	}
	slices.SortStableFunc(rows, func(a, b api.Show) int {
		byNames := cmp.Compare(strings.ToLower(showName(a)), strings.ToLower(showName(b)))
		var c int
		switch s.sort {
		case byName:
			c = byNames
		case byState:
			c = cmp.Compare(rank(a), rank(b))
		case byEpisodes:
			c = cmp.Compare(b.Aired-b.Imported, a.Aired-a.Imported)
		case bySize:
			c = cmp.Compare(b.Bytes, a.Bytes)
		case byStatus:
			c = cmp.Compare(status(a.Status), status(b.Status))
		}
		if s.reverse {
			c = -c
		}
		return cmp.Or(c, byNames)
	})
	return rows, total
}

func (c category) holds(sh api.Show) bool {
	switch c {
	case catTV:
		return sh.Profile != "anime"
	case catAnime:
		return sh.Profile == "anime"
	case catMovies:
		return false
	}
	return true
}

// fuzzy reports whether needle's letters appear in s in order.
func fuzzy(needle []rune, s string) bool {
	if s == "" {
		return false
	}
	i := 0
	for _, r := range strings.ToLower(s) {
		if i < len(needle) && r == needle[i] {
			i++
		}
	}
	return i == len(needle)
}

var stateRank = map[string]int{
	api.EpisodeFailed: 0, api.EpisodeAttention: 1, api.EpisodeDownloading: 2, api.EpisodeProcessing: 3,
	api.EpisodeImporting: 4, api.EpisodeWaiting: 5, api.EpisodeNotSearched: 5, api.EpisodeUpcoming: 6,
}

// rank orders shows for the state sort: needs you first, complete last. A
// state this client does not know, say from a newer server, comes just
// before complete.
func rank(sh api.Show) int {
	if sh.Focus == nil {
		return 9
	}
	if r, ok := stateRank[sh.Focus.State]; ok {
		return r
	}
	return 8
}

func showName(sh api.Show) string { return clean(cmp.Or(sh.NameEng, sh.Name)) }

func status(s string) string {
	switch s {
	case "Continuing":
		return "Airing"
	case "":
		return ""
	}
	return clean(s)
}

const libraryTop = 6 // rows above the table: header, blank, tabs, blank, column names, rule

func (s *libraryScreen) view(width, height int) string {
	rows, total := s.rows()
	s.page = max(1, height-libraryTop-1)
	cursor := max(0, slices.IndexFunc(rows, func(sh api.Show) bool { return sh.ID == s.selected }))
	if len(rows) > 0 {
		s.selected = rows[cursor].ID
	}
	s.offset = min(max(s.offset, cursor-s.page+1), cursor)

	var b strings.Builder
	b.WriteString(header(width, s.sess, "Garner", "Library") + "\n\n")
	b.WriteString("   " + s.tabs() + "\n\n")
	cols := s.columns(width)
	b.WriteString(cols.head(s.sort, s.reverse) + "\n")
	b.WriteString("   " + sBorder.Render(strings.Repeat("─", max(0, width-6))) + "\n")

	switch {
	case !s.loaded && s.sess.offline:
		b.WriteString("   " + sMuted.Render("Garner at "+clean(s.sess.client.Addr())+" does not answer. Trying again."))
	case !s.loaded:
		b.WriteString("   " + sMuted.Render("Loading the library…"))
	case s.cat == catMovies:
		b.WriteString("   " + sMuted.Render("Movies are not available yet."))
	case len(s.sess.shows) == 0:
		b.WriteString("   " + sMuted.Render("No shows yet. Press ") + sKey.Render("a") + sMuted.Render(" to add one."))
	case len(rows) == 0 && s.filter != "":
		b.WriteString("   " + sMuted.Render("No show matches "+strconv.Quote(s.filter)+"."))
	case len(rows) == 0:
		b.WriteString("   " + sMuted.Render("No shows in this category."))
	default:
		end := min(len(rows), s.offset+s.page)
		for i := s.offset; i < end; i++ {
			b.WriteString(cols.row(rows[i], i == cursor) + "\n")
		}
	}
	page := screenLayout(width, height, b.String(), s.bottom(width, len(rows), total))
	if sh, ok := s.cursorShow(); ok && s.asking {
		return overlay(page, deleteQuestion(showName(sh), sh, s.choice), width, height)
	}
	return page
}

func (s *libraryScreen) tabs() string {
	counts := [4]int{}
	for _, sh := range s.sess.shows {
		for c := catAll; c <= catAnime; c++ {
			if c.holds(sh) {
				counts[c]++
			}
		}
	}
	names := [4]string{"All", "TV", "Anime", "Movies"}
	parts := make([]string, 4)
	for c := range parts {
		label := sMuted.Render(names[c])
		if category(c) == s.cat {
			label = sBold.Render(names[c])
		}
		if category(c) != catMovies {
			label += " " + sMuted.Render(strconv.Itoa(counts[c]))
		}
		parts[c] = label
	}
	return strings.Join(parts, "      ")
}

func (s *libraryScreen) bottom(width, shown, total int) string {
	switch {
	case s.typing:
		left := sNormal.Render("/"+s.filter) + lipgloss.NewStyle().Background(cFg).Render(" ")
		right := sMuted.Render(fmt.Sprintf("%d of %d", shown, total))
		return left + strings.Repeat(" ", max(1, width-2-lipgloss.Width(left)-lipgloss.Width(right))) + right
	case s.busy != "":
		return sMuted.Render(s.busy)
	case s.flash.text != "":
		return s.flash.text
	case s.loaded && len(s.sess.shows) == 0:
		return keyBar("a", "add show", "?", "help", "q", "quit")
	case s.filter != "":
		return keyBar("j k", "move", "enter", "open", "i", "details", "a", "add", "d", "delete", "/", "filter",
			"esc", "clear filter", "?", "help", "q", "quit")
	}
	// tab category is in Help; the tabs show at the top.
	return keyBar("j k", "move", "enter", "open", "i", "details", "a", "add", "d", "delete", "/", "filter",
		"s", "sort", "A", "activity", "?", "help", "q", "quit")
}

// columns holds the table's column widths: glyph, show, episodes, size,
// status, then state takes the rest.
type columns struct {
	width                         int
	name, episodes, size, statusW int
	state                         int
}

func (s *libraryScreen) columns(width int) columns {
	c := columns{width: width, name: len("Show") + 2, episodes: len("Episodes") + 2, size: len("Size") + 2, statusW: len("Status") + 2}
	for _, sh := range s.sess.shows {
		c.name = max(c.name, ansi.StringWidth(showName(sh)))
		c.episodes = max(c.episodes, len(episodes(sh)))
		c.size = max(c.size, len(size(sh.Bytes)))
		c.statusW = max(c.statusW, len(status(sh.Status)))
	}
	// Margins and gaps: 3 + glyph 1 + 3 + name + 3 + episodes + 3 + size
	// + 3 + status + 3 + state + 3.
	fixed := 3 + 1 + 3 + 3 + c.episodes + 3 + c.size + 3 + c.statusW + 3 + 3
	// Narrow: the names keep room, State gives way, then Status goes.
	state := 24
	if width-fixed-state < min(c.name, minName) {
		fixed, c.statusW, state = fixed-3-c.statusW, 0, 16
	}
	c.name = max(12, min(c.name, width-fixed-state))
	c.state = max(0, width-fixed-c.name)
	return c
}

func (c columns) head(sort sortKey, reverse bool) string {
	arrow := "▾"
	if reverse {
		arrow = "▴"
	}
	label := func(name string, k sortKey) (string, lipgloss.Style) {
		if k == sort {
			return name + " " + arrow, sNormal
		}
		return name, sSubtle
	}
	glyph := " "
	if sort == byState {
		glyph = sNormal.Render(arrow)
	}
	field := func(name string, k sortKey, w int, right bool) string {
		t, st := label(name, k)
		return st.Render(fit(t, w, right))
	}
	status := ""
	if c.statusW > 0 {
		status = field("Status", byStatus, c.statusW, false) + "   "
	}
	return "   " + glyph + "   " + field("Show", byName, c.name, false) + "   " + field("Episodes", byEpisodes, c.episodes, true) +
		"   " + field("Size", bySize, c.size, true) + "   " + status + sSubtle.Render("State")
}

func (c columns) row(sh api.Show, selected bool) string {
	bg := func(st lipgloss.Style) lipgloss.Style {
		if selected {
			return st.Background(cSelect)
		}
		return st
	}
	gap := bg(lipgloss.NewStyle()).Render("   ")
	main, numbers, statusStyle := sNormal, sNormal, sMuted
	if sh.Focus == nil {
		main, numbers, statusStyle = sSubtle, sSubtle, sSubtle
	}
	glyph, glyphStyle := showGlyph(sh)
	line := gap + bg(glyphStyle).Render(glyph) + gap +
		bg(main).Render(fit(showName(sh), c.name, false)) + gap +
		bg(numbers).Render(fit(episodes(sh), c.episodes, true)) + gap +
		bg(numbers).Render(fit(size(sh.Bytes), c.size, true)) + gap
	if c.statusW > 0 {
		line += bg(statusStyle).Render(fit(status(sh.Status), c.statusW, false)) + gap
	}
	line += renderParts(stateParts(sh, time.Now()), c.state, bg)
	if w := lipgloss.Width(line); w < c.width {
		line += bg(lipgloss.NewStyle()).Render(strings.Repeat(" ", c.width-w))
	}
	return line
}

// fit cuts plain text to w cells with an ellipsis, then pads it.
func fit(s string, w int, right bool) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	padding := strings.Repeat(" ", w-ansi.StringWidth(s))
	if right {
		return padding + s
	}
	return s + padding
}

type part struct {
	text  string
	style lipgloss.Style
}

// renderParts draws styled parts within w cells, cutting the last that
// does not fit.
func renderParts(parts []part, w int, bg func(lipgloss.Style) lipgloss.Style) string {
	var b strings.Builder
	for _, p := range parts {
		if w <= 0 {
			break
		}
		t := ansi.Truncate(p.text, w, "…")
		w -= ansi.StringWidth(t)
		b.WriteString(bg(p.style).Render(t))
	}
	return b.String()
}

func showGlyph(sh api.Show) (string, lipgloss.Style) {
	if sh.Focus == nil {
		return "✓", sMint
	}
	switch sh.Focus.State {
	case api.EpisodeFailed:
		return "✗", sRed
	case api.EpisodeAttention:
		return "▲", sAmber
	case api.EpisodeDownloading:
		return "◐", sAmber
	case api.EpisodeProcessing, api.EpisodeImporting:
		return "●", sAmber
	case api.EpisodeUpcoming:
		return "·", sSubtle
	}
	return "○", sSubtle
}

// stateParts describes what the show's most urgent episode is doing.
func stateParts(sh api.Show, now time.Time) []part {
	f := sh.Focus
	if f == nil {
		return nil
	}
	switch f.State {
	case api.EpisodeWaiting, api.EpisodeNotSearched:
		// One missing episode tells what its search found, several their count.
		if n := sh.Counts[api.EpisodeNotSearched] + sh.Counts[api.EpisodeWaiting]; n > 1 {
			return []part{{strconv.Itoa(n) + " missing", sMuted}}
		}
	case api.EpisodeUpcoming:
		if sh.NextAired != "" {
			return []part{{"next episode " + day(sh.NextAired, now), sMuted}}
		}
		return nil
	}
	return statusParts(f.State, f.Download, f.Summary, f.NextRetry, 0, now)
}

// importSlowAfter is when an import that waits for Jellyfin shows since
// when it waits.
const importSlowAfter = 3 * time.Minute

// redrawMsg only makes the screen draw again.
type redrawMsg struct{}

// redrawWhenSlow draws the screen again when a waiting import passes
// importSlowAfter, so its start time appears without a key press.
func redrawWhenSlow(downloads ...*api.Download) tea.Cmd {
	var next time.Time
	for _, d := range downloads {
		if d == nil || d.Import.State != api.ImportScanning {
			continue
		}
		if at := d.Updated.Add(importSlowAfter); at.After(time.Now()) && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	if next.IsZero() {
		return nil
	}
	return tea.Tick(time.Until(next), func(time.Time) tea.Msg { return redrawMsg{} })
}

// statusParts describes what an episode is doing, from its state, latest
// download and latest search. The sentences come from the server verbatim.
// A download that replaces a file in the library names the file's tier
// first, so a narrow State column keeps it; its percentage stands in for
// the progress bar.
func statusParts(state string, d *api.Download, summary string, nextRetry *time.Time, attempts int, now time.Time) []part {
	if d == nil || d.Replaces == "" {
		return pipelineParts(state, d, summary, nextRetry, attempts, now)
	}
	replaces := []part{{"replaces " + clean(d.Replaces), sMuted}, {" · ", sMuted}}
	switch state {
	case api.EpisodeDownloading:
		return append(replaces, downloadParts(d, attempts, false)...)
	case api.EpisodeProcessing, api.EpisodeImporting:
		return append(replaces, pipelineParts(state, d, summary, nextRetry, attempts, now)...)
	case api.EpisodeFailed:
		// The library keeps its file; said before the failure, which is long.
		parts := pipelineParts(state, d, summary, nextRetry, attempts, now)
		return slices.Insert(parts, 1, part{" · keeps " + clean(d.Replaces), sMuted})
	}
	return pipelineParts(state, d, summary, nextRetry, attempts, now)
}

// pipelineParts is statusParts without the file a download replaces, as
// the Episode screen's steps show it.
func pipelineParts(state string, d *api.Download, summary string, nextRetry *time.Time, attempts int, now time.Time) []part {
	sep := part{" · ", sMuted}
	switch state {
	case api.EpisodeFailed:
		if d == nil {
			return []part{{"failed", sRed}}
		}
		var parts []part
		switch d.Import.State {
		case api.ImportFailed:
			parts = []part{{"import failed", sRed}, sep, {clean(d.Import.Detail), sMuted}}
		case api.ImportRejected:
			parts = []part{{"rejected", sRed}, sep, {clean(d.Import.Detail), sMuted}}
		default:
			parts = []part{{"download failed", sRed}, sep, {clean(d.Failure), sMuted}}
		}
		if nextRetry != nil {
			parts = append(parts, sep, part{nextSearch(*nextRetry, now), sMuted})
		}
		return parts
	case api.EpisodeAttention:
		detail := ""
		if d != nil {
			detail = clean(d.Import.Detail)
		}
		return []part{{"needs you", sAmber}, sep, {detail, sMuted}}
	case api.EpisodeDownloading:
		return downloadParts(d, attempts, true)
	case api.EpisodeProcessing:
		if d != nil && d.Detail != "" {
			return []part{{clean(d.Detail), sMuted}}
		}
		return []part{{"processing", sMuted}}
	case api.EpisodeImporting:
		if d == nil {
			return []part{{"importing", sMuted}}
		}
		switch d.Import.State {
		case api.ImportMoving:
			return []part{{"moving into the library", sMuted}}
		case api.ImportScanning:
			// Jellyfin usually confirms within 1 to 2 minutes; only a longer
			// wait gets its start time.
			if started := d.Updated; now.Sub(started) >= importSlowAfter {
				return []part{{"importing into the media library… · since " + when(started, now), sMuted}}
			}
			return []part{{"importing into the media library…", sMuted}}
		}
		return []part{{"checking the file", sMuted}}
	case api.EpisodeWaiting, api.EpisodeNotSearched:
		if d != nil && d.State == api.Cancelled {
			// The monitor leaves it to you until you download it.
			return []part{{"missing · download cancelled", sMuted}}
		}
		if state == api.EpisodeNotSearched {
			return []part{{"missing", sMuted}}
		}
		text := "missing"
		if summary != "" {
			text += " · " + lowerFirst(clean(summary))
		}
		if nextRetry != nil {
			text += " · " + nextSearch(*nextRetry, now)
		}
		return []part{{text, sMuted}}
	}
	return nil
}

// downloadParts shows a download's progress, with a bar or without.
func downloadParts(d *api.Download, attempts int, bar bool) []part {
	var parts []part
	switch {
	case d == nil || d.Progress <= 0:
		parts = []part{{"queued", sMuted}}
	case bar:
		const width = 16
		done := min(width, int(d.Progress/100*width+0.5))
		parts = []part{
			{strings.Repeat("█", done), sAmber},
			{strings.Repeat("█", width-done), sBorder},
			{fmt.Sprintf("  %3.0f%%", d.Progress), sNormal},
		}
	default:
		parts = []part{{fmt.Sprintf("%.0f%%", d.Progress), sNormal}}
	}
	if d != nil && d.Progress > 0 {
		if left := timeLeft(d.TimeLeft); left != "" {
			parts = append(parts, part{"   " + left + " left", sMuted})
		}
	}
	if attempts > 1 {
		parts = append(parts, part{" · attempt " + strconv.Itoa(attempts), sMuted})
	}
	return parts
}

// nextSearch says when the monitor searches again; "soon" once the time
// has come, while it works through the episodes before.
func nextSearch(t, now time.Time) string {
	if !t.After(now) {
		return "next search soon"
	}
	return "next search " + when(t, now)
}

// lowerFirst lets a server sentence continue a line: "Found 21 matching
// releases" becomes "found 21 matching releases".
func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}

func episodes(sh api.Show) string { return fmt.Sprintf("%d/%d", sh.Imported, sh.Aired) }

// size uses decimal units.
func size(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n >= 100e9:
		return fmt.Sprintf("%.0f GB", float64(n)/1e9)
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.0f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%.0f kB", float64(n)/1e3)
}

// timeLeft turns SABnzbd's "0:01:20" into "1m20s".
func timeLeft(s string) string {
	parts := strings.Split(s, ":")
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return ""
		}
		total = total*60 + n
	}
	if len(parts) == 4 { // days:hours:minutes:seconds
		return ""
	}
	if total <= 0 {
		return ""
	}
	return elapsed(time.Duration(total) * time.Second)
}

// elapsed is a short duration: 40s, 1m20s, 2h5m.
func elapsed(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// day writes a date as "15 Oct", with the year when it is not this year.
func day(date string, now time.Time) string {
	t, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return clean(date)
	}
	if t.Year() != now.Year() {
		return t.Format("2 Jan 2006")
	}
	return t.Format("2 Jan")
}

// when writes a time as "13:30" today, otherwise with the day.
func when(t, now time.Time) string {
	t = t.Local()
	if t.Format(time.DateOnly) == now.Local().Format(time.DateOnly) {
		return t.Format("15:04")
	}
	return day(t.Format(time.DateOnly), now) + " " + t.Format("15:04")
}

func (s *libraryScreen) flashed() *flash { return &s.flash }

func (s *libraryScreen) captures() bool { return s.typing || s.asking || s.deleting != "" }

func (s *libraryScreen) help() (string, []string) {
	return "Library", []string{
		"enter", "open the show", "i", "show details",
		"a", "add a show", "d", "delete the show",
		"tab", "next category", "1 2 3 4", "jump to a category",
		"/", "filter, esc clears", "s", "sort by the next column",
		"I", "reverse the order", "q", "quit",
	}
}
