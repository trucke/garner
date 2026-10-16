package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/text/language"

	"github.com/trucke/garner/internal/catalog"
	"github.com/trucke/garner/internal/client"
)

// Add show searches TVDB in the bottom line and opens a show's details
// before adding it. Boards "Add show · …" in the Paper file.

// searchPause is how long typing must pause before Garner asks TVDB.
const searchPause = 350 * time.Millisecond

type (
	searchDueMsg struct{ seq int }
	searchMsg    struct {
		query string
		page  catalog.SearchPage
		err   error
	}
	// addedMsg returns to the library with the new show selected.
	addedMsg struct {
		id      int64
		name    string
		already bool
	}
)

type addScreen struct {
	sess      *session
	back      screen
	input     textinput.Model
	seq       int    // counts edits, so only the last pause searches
	running   string // the query being searched
	shown     string // the query the results belong to
	page      catalog.SearchPage
	err       error
	selected  int64
	offset    int
	rowsShown int
	adding    string // the show being added
	flash     flash
}

func newAddShow(sess *session, back screen) *addScreen {
	in := textinput.New()
	in.Prompt = "> "
	in.Placeholder = "name, TVDB ID or thetvdb.com link"
	st := textinput.DefaultStyles(true)
	st.Cursor.Blink = false
	st.Cursor.Color = cFg
	st.Focused.Prompt = sNormal
	st.Focused.Text = sNormal
	st.Focused.Placeholder = sSubtle
	in.SetStyles(st)
	return &addScreen{sess: sess, back: back, input: in, rowsShown: 20}
}

func (s *addScreen) init() tea.Cmd { return s.input.Focus() }

// searchable reports whether a query is worth asking TVDB about.
func searchable(q string) bool {
	return len([]rune(q)) >= 2 || q != "" && strings.IndexFunc(q, func(r rune) bool { return !unicode.IsDigit(r) }) < 0
}

func (s *addScreen) search(q string) tea.Cmd {
	s.running = q
	cl := s.sess.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		page, err := cl.Search(ctx, q)
		return searchMsg{q, page, err}
	}
}

func (s *addScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case searchDueMsg:
		q := strings.TrimSpace(s.input.Value())
		if msg.seq == s.seq && searchable(q) && q != s.running && q != s.shown {
			return s, s.search(q)
		}
		return s, nil
	case searchMsg:
		if msg.query != s.running {
			return s, nil // an earlier search finished late
		}
		s.running = ""
		if errors.Is(msg.err, client.ErrUnauthorized) {
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		}
		s.shown, s.page, s.err = msg.query, msg.page, msg.err
		s.selected, s.offset = 0, 0
		if len(s.page.Results) > 0 {
			s.selected = s.page.Results[0].ID
		}
		return s, nil
	case addResultMsg:
		s.adding = ""
		return s, added(msg, &s.flash)
	case tea.KeyPressMsg:
		if s.adding != "" {
			return s, nil
		}
		s.flash.clear()
		switch msg.String() {
		case "esc":
			return s.back, s.back.init()
		case "ctrl+u":
			// Clear the whole search, wherever the cursor is, and drop a
			// search still due for the old text.
			s.input.SetValue("")
			s.seq++
			return s, nil
		case "ctrl+a":
			q := strings.TrimSpace(s.input.Value())
			r, ok := s.cursorResult()
			if !ok || q != s.shown {
				return s, nil
			}
			if r.Added {
				return s, func() tea.Msg { return addedMsg{id: r.ID, name: resultName(r), already: true} }
			}
			s.adding = resultName(r)
			return s, addShow(s.sess.client, r.ID, s.adding)
		case "enter":
			q := strings.TrimSpace(s.input.Value())
			if s.err != nil && q == s.shown {
				return s, s.search(q)
			}
			if r, ok := s.cursorResult(); ok && q == s.shown {
				p := newPreview(s.sess, s, r)
				return p, p.init()
			}
			return s, nil
		case "up", "ctrl+p":
			s.move(-1)
			return s, nil
		case "down", "ctrl+n":
			s.move(1)
			return s, nil
		case "pgup":
			s.move(-s.rowsShown)
			return s, nil
		case "pgdown":
			s.move(s.rowsShown)
			return s, nil
		}
	}

	before := s.input.Value()
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(msg)
	if s.input.Value() == before {
		return s, cmd
	}
	s.seq++
	seq := s.seq
	return s, tea.Batch(cmd, tea.Tick(searchPause, func(time.Time) tea.Msg { return searchDueMsg{seq} }))
}

func (s *addScreen) cursorResult() (catalog.SearchResult, bool) {
	i := slices.IndexFunc(s.page.Results, func(r catalog.SearchResult) bool { return r.ID == s.selected })
	if i < 0 {
		return catalog.SearchResult{}, false
	}
	return s.page.Results[i], true
}

func (s *addScreen) move(n int) {
	rs := s.page.Results
	if len(rs) == 0 {
		return
	}
	i := max(0, slices.IndexFunc(rs, func(r catalog.SearchResult) bool { return r.ID == s.selected }))
	s.selected = rs[min(len(rs)-1, max(0, i+n))].ID
}

const addTop = 4 // rows above the results: header, blank, column names, rule

func (s *addScreen) view(width, height int) string {
	q := strings.TrimSpace(s.input.Value())
	current := q == s.shown && s.shown != ""
	cols := searchColumns(s.page.Results, width)

	var b strings.Builder
	b.WriteString(header(width, s.sess, "Garner", "Library", "Add show") + "\n\n")
	b.WriteString(cols.head() + "\n")
	b.WriteString("   " + sBorder.Render(strings.Repeat("─", max(0, width-6))) + "\n")

	rs := s.page.Results
	switch {
	case q == "":
		b.WriteString("   " + sMuted.Render("Type a show's name, a TVDB ID or a thetvdb.com link."))
	case s.shown == "" && s.running != "":
		b.WriteString("   " + sMuted.Render("Searching TVDB…"))
	case s.shown == "":
	case s.err != nil:
		var server *client.ServerError
		text := clean(s.err.Error())
		if errors.As(s.err, &server) && server.Status == http.StatusBadGateway {
			text = "TVDB did not answer: " + text + "."
		}
		b.WriteString("   " + gFailed + " " + sNormal.Render(text) + sMuted.Render(" Change the search or press ") +
			sKey.Render("enter") + sMuted.Render(" to try again."))
	case len(rs) == 0:
		b.WriteString("   " + sMuted.Render("No show matches "+strconv.Quote(s.shown)+"."))
	default:
		more := 0
		if s.page.Total > len(rs) {
			more = 2
		}
		s.rowsShown = max(1, height-addTop-1-more)
		cursor := max(0, slices.IndexFunc(rs, func(r catalog.SearchResult) bool { return r.ID == s.selected }))
		s.offset = min(max(s.offset, cursor-s.rowsShown+1), cursor)
		for i := s.offset; i < min(len(rs), s.offset+s.rowsShown); i++ {
			b.WriteString(cols.row(rs[i], current && i == cursor) + "\n")
		}
		if more > 0 {
			b.WriteString("\n   " + sMuted.Render(fmt.Sprintf("TVDB found %d; these are the first %d. Search more precisely to narrow it down.", s.page.Total, len(rs))))
		}
	}
	return screenLayout(width, height, b.String(), s.bottom(width, q))
}

func (s *addScreen) bottom(width int, q string) string {
	var count string
	switch {
	case s.running != "":
		count = "searching…"
	case s.shown == "" || q != s.shown:
	case s.err == nil && s.page.Total > len(s.page.Results):
		count = fmt.Sprintf("%d of %d", len(s.page.Results), s.page.Total)
	case s.err == nil:
		count = strconv.Itoa(len(s.page.Results)) + " results"
	}
	switch {
	case s.adding != "":
		return sMuted.Render("Adding " + s.adding + " and loading its episodes from TVDB…")
	case s.flash.text != "":
		return s.flash.text
	}
	keys := keyBar("ctrl+u", "clear", "esc", "back")
	switch {
	case s.err != nil && q == s.shown:
		keys = keyBar("enter", "try again", "ctrl+u", "clear", "esc", "back")
	case len(s.page.Results) > 0 && q == s.shown:
		keys = keyBar("↑↓", "move", "enter", "details", "ctrl+a", "add", "ctrl+u", "clear", "esc", "back")
	}
	right := keys
	if count != "" {
		right = sMuted.Render(count) + "    " + keys
	}
	s.input.SetWidth(max(10, width-2-lipgloss.Width(right)-6))
	left := s.input.View()
	return left + strings.Repeat(" ", max(2, width-2-lipgloss.Width(left)-lipgloss.Width(right))) + right
}

// searchColumns sizes the result table: glyph, name, year, network,
// language, status.
type searchCols struct {
	width, name, network int
}

func searchColumns(rs []catalog.SearchResult, width int) searchCols {
	c := searchCols{width: width, name: len("Name"), network: len("Network")}
	for _, r := range rs {
		c.name = max(c.name, ansi.StringWidth(resultName(r)))
		c.network = max(c.network, ansi.StringWidth(clean(r.Network)))
	}
	c.network = min(c.network, 18)
	// Margins and gaps: 3 + glyph 1 + 3 + name + 3 + year 4 + 3 + network
	// + 3 + lang 4 + 3 + status 10.
	fixed := 3 + 1 + 3 + 3 + 4 + 3 + c.network + 3 + 4 + 3 + 10
	c.name = max(12, min(c.name, width-fixed-3))
	return c
}

func (c searchCols) head() string {
	return "   " + " " + "   " + sSubtle.Render(fit("Name", c.name, false)) + "   " + sSubtle.Render("Year") + "   " +
		sSubtle.Render(fit("Network", c.network, false)) + "   " + sSubtle.Render(fit("Lang", 4, false)) + "   " + sSubtle.Render("Status")
}

func (c searchCols) row(r catalog.SearchResult, selected bool) string {
	bg := func(st lipgloss.Style) lipgloss.Style {
		if selected {
			return st.Background(cSelect)
		}
		return st
	}
	gap := bg(lipgloss.NewStyle()).Render("   ")
	glyph := bg(lipgloss.NewStyle()).Render(" ")
	if r.Added {
		glyph = bg(sMint).Render("✓")
	}
	year := ""
	if r.Year > 0 {
		year = strconv.Itoa(r.Year)
	}
	line := gap + glyph + gap + bg(sNormal).Render(fit(resultName(r), c.name, false)) + gap +
		bg(sMuted).Render(fit(year, 4, false)) + gap + bg(sMuted).Render(fit(clean(r.Network), c.network, false)) + gap +
		bg(sMuted).Render(fit(langCode(r.Language), 4, false)) + gap + bg(sMuted).Render(fit(status(r.Status), 10, false))
	if w := lipgloss.Width(line); w < c.width {
		line += bg(lipgloss.NewStyle()).Render(strings.Repeat(" ", c.width-w))
	}
	return line
}

func resultName(r catalog.SearchResult) string { return clean(cmp.Or(r.NameEng, r.Name)) }

// langCode turns TVDB's three-letter language codes into two letters: jpn
// becomes JA.
func langCode(code string) string {
	if b, err := language.ParseBase(code); err == nil {
		return strings.ToUpper(b.String())
	}
	return strings.ToUpper(clean(code))
}

// previewScreen shows a show's TVDB record before adding it.
type previewScreen struct {
	sess    *session
	back    screen
	result  catalog.SearchResult
	preview *catalog.Preview
	err     error
	adding  bool
	flash   flash
	scroll  scroller
}

type (
	// previewMsg answers one details page; a late answer for a page that
	// was left must not show on the next one.
	previewMsg struct {
		from    *previewScreen
		preview catalog.Preview
		err     error
	}
	// addResultMsg answers adding a show, from the list or its details.
	addResultMsg struct {
		id   int64
		name string
		err  error
	}
	openedMsg struct{ err error }
)

func newPreview(sess *session, back screen, r catalog.SearchResult) *previewScreen {
	return &previewScreen{sess: sess, back: back, result: r}
}

func (s *previewScreen) init() tea.Cmd {
	s.err = nil
	cl, id := s.sess.client, s.result.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p, err := cl.Preview(ctx, id)
		return previewMsg{s, p, err}
	}
}

func (s *previewScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case previewMsg:
		if msg.from != s {
			return s, nil
		}
		if errors.Is(msg.err, client.ErrUnauthorized) {
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		}
		if msg.err != nil {
			s.err = msg.err
			return s, nil
		}
		s.preview = &msg.preview
	case addResultMsg:
		s.adding = false
		return s, added(msg, &s.flash)
	case openedMsg:
		if msg.err != nil {
			s.flash.stay(gFailed + " " + sRed.Render("Could not open the browser: "+clean(msg.err.Error())))
		}
	case tea.KeyPressMsg:
		if s.adding {
			return s, nil
		}
		s.flash.clear()
		if s.scroll.key(msg.String()) {
			return s, nil
		}
		switch msg.String() {
		case "esc":
			return s.back, nil
		case "r":
			if s.err != nil {
				return s, s.init()
			}
		case "o":
			if s.preview != nil {
				return s, openLink(s.preview.URL)
			}
		case "a":
			if s.preview != nil && !s.preview.Added {
				s.adding = true
				return s, addShow(s.sess.client, s.result.ID, s.name())
			}
		}
	}
	return s, nil
}

func (s *previewScreen) name() string {
	if s.preview != nil {
		return clean(cmp.Or(s.preview.NameEng, s.preview.Name))
	}
	return resultName(s.result)
}

// openLink opens a TVDB page in the browser. Only thetvdb.com links are
// opened: the address comes from the server.
func openLink(link string) tea.Cmd {
	if !strings.HasPrefix(link, "https://thetvdb.com/") {
		return func() tea.Msg { return openedMsg{fmt.Errorf("not a thetvdb.com link: %s", link)} }
	}
	return browse(link)
}

// browse opens a link in the browser.
func browse(link string) tea.Cmd {
	return func() tea.Msg {
		name := "xdg-open"
		if runtime.GOOS == "darwin" {
			name = "open"
		}
		// Some openers stay until the browser closes; do not wait for them.
		cmd := exec.Command(name, link)
		if err := cmd.Start(); err != nil {
			return openedMsg{err}
		}
		go cmd.Wait()
		return openedMsg{}
	}
}

// detailsWidth keeps the frames, and with them the overview's lines, to
// about 72 characters, which is easier to read than the full width.
const detailsWidth = 78

func (s *previewScreen) view(width, height int) string {
	var b strings.Builder
	b.WriteString(header(width, s.sess, "Garner", "Library", "Add show", s.name()) + "\n\n")
	w := max(40, min(detailsWidth, width-6))
	p := s.preview
	switch {
	case s.err != nil:
		b.WriteString("   " + gFailed + " " + sNormal.Render(clean(s.err.Error())) + sMuted.Render(" Press ") +
			sKey.Render("r") + sMuted.Render(" to try again."))
	case p == nil:
		b.WriteString("   " + sMuted.Render("Loading "+s.name()+" from TVDB…"))
	default:
		typ := "TV"
		if p.Type == "anime" {
			typ = "Anime"
		}
		b.WriteString(titleBlock(p.Name, p.NameEng, p.NameDeu, typ, p.Year, p.Status))
		if p.Added {
			b.WriteString("   " + gDone + " " + sNormal.Render("In the library") + "\n")
		}
		b.WriteString("\n" + indent(frame("tvdb", tvdbLines(previewFacts(p), w-6), w), 3) + "\n")
		b.WriteString("\n" + indent(frame("garner", garnerLines(p, w-6), w), 3) + "\n")
		if p.Overview != "" {
			b.WriteString("\n" + indent(frame(overviewLabel(p.OverviewLanguage), []string{sNormal.Render(clean(p.Overview))}, w), 3) + "\n")
		}
	}
	return s.scroll.layout(width, height, b.String(), s.bottom())
}

const labelWidth = 12

// field renders a label and a value, wrapping the value under itself.
func field(label, value string, style lipgloss.Style, inner int) []string {
	if value == "" {
		return nil
	}
	var lines []string
	for i, l := range strings.Split(ansi.Wrap(value, max(10, inner-labelWidth), ""), "\n") {
		head := strings.Repeat(" ", labelWidth)
		if i == 0 && label != "" {
			head = sSubtle.Render(pad(label, labelWidth))
		}
		lines = append(lines, head+style.Render(l))
	}
	return lines
}

// tvdbFacts are what the TVDB frame shows, from a preview before adding or
// from the stored show.
type tvdbFacts struct {
	network, status, firstAired, lastAired string
	episodes, seasons, runtime             int
	orders                                 []string
	url                                    string
}

func previewFacts(p *catalog.Preview) tvdbFacts {
	return tvdbFacts{network: p.Network, status: p.Status, firstAired: p.FirstAired, lastAired: p.LastAired,
		episodes: p.Episodes, seasons: p.Seasons, runtime: p.Runtime, orders: p.Orders, url: p.URL}
}

// tvdbLines are TVDB's facts about the show.
func tvdbLines(f tvdbFacts, inner int) []string {
	var lines []string
	lines = append(lines, field("Network", clean(f.network), sNormal, inner)...)
	lines = append(lines, field("Aired", aired(f), sNormal, inner)...)
	if f.episodes > 0 {
		e := fmt.Sprintf("%d in %d season", f.episodes, f.seasons)
		if f.seasons != 1 {
			e += "s"
		}
		if f.runtime > 0 {
			e += fmt.Sprintf(" · %d min", f.runtime)
		}
		lines = append(lines, field("Episodes", e, sNormal, inner)...)
	}
	if len(f.orders) > 1 {
		// Releases may follow an order other than the aired one.
		orders := make([]string, len(f.orders))
		for i, o := range f.orders {
			orders[i] = clean(o)
		}
		lines = append(lines, field("Orders", strings.Join(orders, ", "), sNormal, inner)...)
	}
	if strings.HasPrefix(f.url, "https://thetvdb.com/") {
		lines = append(lines, field("Link", f.url, lipgloss.NewStyle().Foreground(cIris).Underline(true).Hyperlink(f.url), inner)...)
	}
	return lines
}

// garnerLines say how Garner will treat the show's releases. The sentences
// come from the server.
func garnerLines(p *catalog.Preview, inner int) []string {
	var lines []string
	lines = append(lines, field("Rules", clean(p.Rules), sNormal, inner)...)
	lines = append(lines, field("Original", clean(p.LanguageName), sNormal, inner)...)
	lines = append(lines, field("", clean(p.LanguageNote), sMuted, inner)...)
	return lines
}

// aired is "8 Apr 2003 to 8 Jul 2003" for an ended show, "since 29 Sep
// 2023" for one still running.
func aired(f tvdbFacts) string {
	date := func(s string) string {
		t, err := time.Parse(time.DateOnly, s)
		if err != nil {
			return clean(s)
		}
		return t.Format("2 Jan 2006")
	}
	switch {
	case f.firstAired == "":
		return ""
	case f.status == "Ended" && f.lastAired != "":
		return date(f.firstAired) + " to " + date(f.lastAired)
	case f.status == "Continuing":
		return "since " + date(f.firstAired)
	}
	return "from " + date(f.firstAired)
}

func (s *previewScreen) bottom() string {
	switch {
	case s.adding:
		return sMuted.Render("Adding " + s.name() + " and loading its episodes from TVDB…")
	case s.flash.text != "":
		return s.flash.text
	case s.err != nil:
		return keyBar("r", "try again", "esc", "back")
	case s.preview == nil:
		return keyBar("esc", "back")
	case s.preview.Added:
		return keyBar("o", "open on TVDB", "esc", "back")
	}
	return keyBar("a", "add to library", "o", "open on TVDB", "esc", "back")
}

func (s *previewScreen) flashed() *flash { return &s.flash }

func (s *previewScreen) help() (string, []string) {
	return "Add show", []string{"a", "add to the library", "o", "open on TVDB"}
}

// captures: the search field takes every key, ? and A included.
func (s *addScreen) captures() bool { return true }

func (s *addScreen) flashed() *flash { return &s.flash }

// addShow adds a show. It loads every episode from TVDB and TheXEM, which
// takes a few seconds.
func addShow(cl *client.Client, id int64, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return addResultMsg{id, name, cl.Add(ctx, id)}
	}
}

// added opens the show once it is in the library, also when it was there
// already, and reports anything else in the bottom line.
func added(msg addResultMsg, f *flash) tea.Cmd {
	var server *client.ServerError
	switch {
	case msg.err == nil:
		return func() tea.Msg { return addedMsg{id: msg.id, name: msg.name} }
	case errors.As(msg.err, &server) && server.Status == http.StatusConflict:
		return func() tea.Msg { return addedMsg{id: msg.id, name: msg.name, already: true} }
	case errors.Is(msg.err, client.ErrUnauthorized):
		return func() tea.Msg { return disconnectedMsg{errRevoked} }
	}
	f.stay(gFailed + " " + sRed.Render("Could not add "+msg.name+": "+clean(msg.err.Error())))
	return nil
}
