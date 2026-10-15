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

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/client"
	"github.com/trucke/garner/internal/server"
	"github.com/trucke/garner/internal/store"
)

// Show lists a show's episodes by season. Boards "Show" and "Show · 8
// seasons" in the Paper file.

type (
	seriesMsg struct {
		from   *showScreen
		detail server.SeriesDetail
		err    error
	}
)

type showScreen struct {
	sess    *session
	back    screen
	id      int64
	name    string
	detail  *server.SeriesDetail
	err     error
	loading bool
	dirty   bool
	// cursor is the selected row: a season header (episode 0) or an
	// episode.
	cursor  rowKey
	placed  bool // the cursor was set once; the Specials header is the zero key
	offset  int
	page    int
	flash   flash
	lastKey string
}

type rowKey struct {
	season  int
	episode int64
}

// season is one season of the show's default order; number 0 holds the
// specials and episodes the order does not place.
type season struct {
	number   int
	episodes []episodeRow
}

type episodeRow struct {
	episode store.Episode
	pos     store.Position
	status  store.EpisodeStatus
}

func newShow(sess *session, back screen, id int64, name, message string) *showScreen {
	s := &showScreen{sess: sess, back: back, id: id, name: name, page: 20}
	if message != "" {
		s.flash.info(message)
	}
	return s
}

func (s *showScreen) init() tea.Cmd { return s.reload() }

func (s *showScreen) reload() tea.Cmd {
	if s.loading {
		s.dirty = true
		return nil
	}
	s.loading = true
	cl, id := s.sess.client, s.id
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		d, err := cl.Series(ctx, id)
		return seriesMsg{s, d, err}
	}
}

func (s *showScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case seriesMsg:
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
			s.setDetail(msg.detail)
		}
		if s.dirty {
			s.dirty = false
			return s, s.reload()
		}
	case eventMsg:
		switch msg.Type {
		case client.EventConnected, "search.finished", "series.updated":
			return s, s.reload()
		case "download.updated":
			var d store.Download
			if json.Unmarshal(msg.Data, &d) != nil || s.holds(d.EpisodeID) {
				return s, s.reload()
			}
		case "series.deleted":
			var deleted struct{ ID int64 }
			if json.Unmarshal(msg.Data, &deleted) == nil && deleted.ID == s.id {
				return s, func() tea.Msg { return switchMsg{s.back} }
			}
		}
	case tea.KeyPressMsg:
		return s, s.key(msg.String())
	}
	return s, nil
}

// holds reports whether an episode belongs to the show, or the show is not
// loaded yet.
func (s *showScreen) holds(episodeID int64) bool {
	if s.detail == nil {
		return true
	}
	return slices.ContainsFunc(s.detail.Episodes, func(e store.Episode) bool { return e.ID == episodeID })
}

func (s *showScreen) setDetail(d server.SeriesDetail) {
	s.detail = &d
	s.name = cmp.Or(d.Series.NameEng, d.Series.Name)
	if s.sess.folded == nil {
		s.sess.folded = map[int64]map[int]bool{}
	}
	if _, remembered := s.sess.folded[s.id]; !remembered {
		s.sess.folded[s.id] = s.initialFolding()
	}
	if !s.placed {
		// Reopened with its folding remembered: start at the top.
		s.placed = true
		if rows := s.rows(); len(rows) > 0 {
			s.cursor = rows[0]
		}
	}
}

// initialFolding unfolds one season of a show with several: the season of
// the most urgent episode that needs something, or the newest season. The
// cursor starts on that episode, or on the season.
func (s *showScreen) initialFolding() map[int]bool {
	seasons := s.seasons()
	folded := map[int]bool{}
	if len(seasons) == 0 {
		return folded
	}
	open, rank := seasons[0].number, 99
	s.cursor = rowKey{season: open}
	for _, se := range seasons {
		for _, e := range se.episodes {
			// Up to waiting and not searched: what needs something.
			if r, ok := stateRank[e.status.State]; ok && r <= stateRank[store.EpisodeNotSearched] && r < rank {
				open, rank = se.number, r
				s.cursor = rowKey{se.number, e.episode.ID}
			}
		}
	}
	if len(seasons) > 1 {
		for _, se := range seasons {
			folded[se.number] = se.number != open
		}
	}
	s.placed = true
	return folded
}

// seasons groups the episodes by season of the show's default order:
// regular seasons newest first, then the specials.
func (s *showScreen) seasons() []season {
	if s.detail == nil {
		return nil
	}
	states := make(map[int64]store.EpisodeStatus, len(s.detail.States))
	for _, st := range s.detail.States {
		states[st.EpisodeID] = st
	}
	order := s.detail.Series.DefaultOrder
	bySeason := map[int]*season{}
	for _, e := range s.detail.Episodes {
		pos, ok := e.Numbers[order]
		if !ok {
			pos = store.Position{}
		}
		se := bySeason[pos.Season]
		if se == nil {
			se = &season{number: pos.Season}
			bySeason[pos.Season] = se
		}
		se.episodes = append(se.episodes, episodeRow{episode: e, pos: pos, status: states[e.ID]})
	}
	out := make([]season, 0, len(bySeason))
	for _, se := range bySeason {
		slices.SortStableFunc(se.episodes, func(a, b episodeRow) int {
			return cmp.Or(cmp.Compare(a.pos.Number, b.pos.Number), cmp.Compare(a.episode.Aired, b.episode.Aired))
		})
		out = append(out, *se)
	}
	slices.SortFunc(out, func(a, b season) int {
		switch {
		case a.number == 0:
			return 1
		case b.number == 0:
			return -1
		}
		return cmp.Compare(b.number, a.number)
	})
	return out
}

// gap is a blank display line between seasons; the cursor never rests
// on it.
var gap = rowKey{season: -1, episode: -1}

// lines are the display lines: every season header, the episodes of
// unfolded seasons, and a blank line after each unfolded season.
func (s *showScreen) lines() []rowKey {
	var lines []rowKey
	folded := s.sess.folded[s.id]
	seasons := s.seasons()
	for i, se := range seasons {
		lines = append(lines, rowKey{season: se.number})
		if folded[se.number] {
			continue
		}
		for _, e := range se.episodes {
			lines = append(lines, rowKey{se.number, e.episode.ID})
		}
		if i < len(seasons)-1 {
			lines = append(lines, gap)
		}
	}
	return lines
}

// rows are the lines the cursor can rest on.
func (s *showScreen) rows() []rowKey {
	return slices.DeleteFunc(s.lines(), func(k rowKey) bool { return k == gap })
}

func (s *showScreen) key(key string) tea.Cmd {
	prev := s.lastKey
	s.lastKey = key
	s.flash.clear()
	folded := s.sess.folded[s.id]
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
	case "h", "left":
		if folded != nil {
			folded[s.cursor.season] = true
			s.cursor.episode = 0
		}
	case "l", "right":
		if folded != nil {
			folded[s.cursor.season] = false
		}
	case "tab", "shift+tab":
		s.jumpSeason(key == "tab")
	case "enter":
		if s.cursor.episode == 0 && folded != nil {
			folded[s.cursor.season] = !folded[s.cursor.season]
		} else if s.cursor.episode != 0 {
			s.flash.info(sMuted.Render("The episode screen is not available yet."))
		}
	case "s":
		e, ok := s.cursorEpisode()
		if !ok {
			s.flash.info(sMuted.Render("Choose an episode to search for."))
			return nil
		}
		next := newReleases(s.sess, s, e, true)
		return func() tea.Msg { return switchMsg{next} }
	case "S":
		s.flash.info(sMuted.Render("Searching every missing episode is not available yet."))
	case "i":
		if s.detail != nil {
			next := newShowDetails(s.sess, s)
			return func() tea.Msg { return switchMsg{next} }
		}
	case "A":
		s.flash.info(sMuted.Render("Activity is not available yet."))
	case "?":
		s.flash.info(sMuted.Render("Help is not available yet."))
	}
	return nil
}

func (s *showScreen) move(n int) {
	rows := s.rows()
	if len(rows) == 0 {
		return
	}
	i := max(0, slices.Index(rows, s.cursor))
	s.cursor = rows[min(len(rows)-1, max(0, i+n))]
}

func (s *showScreen) jumpSeason(forward bool) {
	seasons := s.seasons()
	i := slices.IndexFunc(seasons, func(se season) bool { return se.number == s.cursor.season })
	switch {
	case i < 0:
		return
	case forward:
		i = (i + 1) % len(seasons)
	case s.cursor.episode == 0:
		i = (i + len(seasons) - 1) % len(seasons)
	}
	s.cursor = rowKey{season: seasons[i].number}
}

func (s *showScreen) cursorEpisode() (episodeRow, bool) {
	for _, se := range s.seasons() {
		for _, e := range se.episodes {
			if se.number == s.cursor.season && e.episode.ID == s.cursor.episode {
				return e, true
			}
		}
	}
	return episodeRow{}, false
}

func episodeLabel(e episodeRow) string {
	return fmt.Sprintf("S%02dE%02d", e.pos.Season, e.pos.Number)
}

const showTop = 6 // rows above the table: header, blank, facts, blank, column names, rule

func (s *showScreen) view(width, height int) string {
	var b strings.Builder
	b.WriteString(header(width, s.sess, "Garner", "Library", clean(s.name)) + "\n\n")
	switch {
	case s.detail == nil && s.err != nil:
		b.WriteString("   " + gFailed + " " + sRed.Render(clean(s.err.Error())))
		return screenLayout(width, height, b.String(), keyBar("esc", "back"))
	case s.detail == nil:
		b.WriteString("   " + sMuted.Render("Loading "+clean(s.name)+"…"))
		return screenLayout(width, height, b.String(), keyBar("esc", "back"))
	}
	d := s.detail
	now := time.Now()
	b.WriteString("   " + s.facts(now) + "\n\n")
	cols := showColumns(d, width)
	b.WriteString(cols.head() + "\n")
	b.WriteString("   " + sBorder.Render(strings.Repeat("─", max(0, width-6))) + "\n")

	seasons := map[int]season{}
	episodes := map[int64]episodeRow{}
	for _, se := range s.seasons() {
		seasons[se.number] = se
		for _, e := range se.episodes {
			episodes[e.episode.ID] = e
		}
	}
	folded := s.sess.folded[s.id]
	rows, lines := s.rows(), s.lines()
	if len(rows) > 0 && !slices.Contains(rows, s.cursor) {
		s.cursor = rows[0]
	}
	s.page = max(1, height-showTop-1)
	// Scroll by display lines, so the cursor stays in view with the gaps.
	cursor := max(0, slices.Index(lines, s.cursor))
	s.offset = min(max(s.offset, cursor-s.page+1), cursor)
	for _, key := range lines[s.offset:min(len(lines), s.offset+s.page)] {
		switch {
		case key == gap:
			b.WriteString("\n")
		case key.episode == 0:
			se := seasons[key.season]
			b.WriteString(cols.seasonRow(se, folded[se.number], key == s.cursor) + "\n")
		default:
			b.WriteString(cols.episodeRow(episodes[key.episode], d, now, key == s.cursor) + "\n")
		}
	}
	return screenLayout(width, height, b.String(), s.bottom())
}

func (s *showScreen) facts(now time.Time) string {
	d := s.detail
	typ := "TV"
	if d.Series.Profile == "anime" {
		typ = "Anime"
	}
	st := status(d.Series.Status)
	if d.Summary.NextAired != "" {
		st += " · next episode " + day(d.Summary.NextAired, now)
	}
	episodes := fmt.Sprintf("%d of %d", d.Summary.Imported, d.Summary.Aired)
	if b := size(d.Summary.Bytes); b != "" {
		episodes += " · " + b
	}
	field := func(label, value string) string { return sSubtle.Render(label) + "  " + sNormal.Render(value) }
	return field("Type", typ) + "      " + field("Status", st) + "      " + field("Episodes", episodes)
}

func (s *showScreen) bottom() string {
	if s.flash.text != "" {
		return s.flash.text
	}
	return keyBar("enter", "episode", "h l", "fold", "tab", "season", "s", "search", "S", "search missing",
		"i", "show details", "A", "activity", "esc", "back")
}

// showCols sizes the episode table: glyph, episode, title, aired, size,
// state.
type showCols struct {
	width, title int
}

const airedWidth = len("26 Sep 2026")

func showColumns(d *server.SeriesDetail, width int) showCols {
	c := showCols{width: width, title: len("Title")}
	for _, e := range d.Episodes {
		c.title = max(c.title, ansi.StringWidth(clean(cmp.Or(e.NameEng, e.Name))))
	}
	// Margins and gaps: 3 + glyph 1 + 3 + episode 7 + 3 + title + 3 +
	// aired + 3 + size 7 + 3 + state.
	fixed := 3 + 1 + 3 + 7 + 3 + 3 + airedWidth + 3 + 7 + 3
	const minState = 28
	c.title = max(10, min(c.title, 40, width-fixed-minState))
	return c
}

func (c showCols) head() string {
	return "   " + " " + "   " + sSubtle.Render(fit("Episode", 7, false)) + "   " + sSubtle.Render(fit("Title", c.title, false)) +
		"   " + sSubtle.Render(fit("Aired", airedWidth, false)) + "   " + sSubtle.Render(fit("Size", 7, true)) + "   " + sSubtle.Render("State")
}

func (c showCols) seasonRow(se season, folded, selected bool) string {
	bg := selectBg(selected)
	glyph, glyphStyle := seasonGlyph(se)
	name := "Season " + strconv.Itoa(se.number)
	if se.number == 0 {
		name = "Specials"
	}
	nameStyle := sBold
	if folded {
		nameStyle = sSubtle
	}
	line := bg(lipgloss.NewStyle()).Render("   ") + bg(glyphStyle).Render(glyph) + bg(lipgloss.NewStyle()).Render("   ") +
		bg(nameStyle).Render(name) + bg(lipgloss.NewStyle()).Render("   ") + bg(sMuted).Render(seasonCounts(se))
	return fill(line, c.width, bg)
}

func seasonCounts(se season) string {
	imported, aired, toCome := 0, 0, 0
	for _, e := range se.episodes {
		switch {
		case e.status.State == store.EpisodeImported:
			imported++
			aired++
		case e.status.State == store.EpisodeUpcoming:
			toCome++
		case e.status.State != "":
			aired++
		}
	}
	if se.number == 0 {
		return fmt.Sprintf("%d of %d in the library", imported, len(se.episodes))
	}
	if aired == 0 {
		return fmt.Sprintf("%d to come", toCome)
	}
	s := fmt.Sprintf("%d of %d", imported, aired)
	if toCome > 0 {
		s += fmt.Sprintf(" · %d to come", toCome)
	}
	return s
}

func (c showCols) episodeRow(e episodeRow, d *server.SeriesDetail, now time.Time, selected bool) string {
	bg := selectBg(selected)
	st := e.status
	main := sNormal
	if st.State == store.EpisodeImported || st.State == "" {
		main = sSubtle
	}
	glyph, glyphStyle := episodeGlyph(st.State)
	aired := ""
	if t, err := time.Parse(time.DateOnly, e.episode.Aired); err == nil {
		aired = t.Format("2 Jan 2006")
	}
	gap := bg(lipgloss.NewStyle()).Render("   ")
	line := gap + bg(glyphStyle).Render(glyph) + gap +
		bg(main).Render(fit(episodeLabel(e), 7, false)) + gap +
		bg(main).Render(fit(clean(cmp.Or(e.episode.NameEng, e.episode.Name)), c.title, false)) + gap +
		bg(main).Render(fit(aired, airedWidth, false)) + gap +
		bg(main).Render(fit(size(st.Size), 7, true)) + gap
	stateWidth := max(0, c.width-lipgloss.Width(line))
	return fill(line+renderParts(episodeParts(e, d, now), stateWidth, bg), c.width, bg)
}

// episodeParts describes an episode's state in the table.
func episodeParts(e episodeRow, d *server.SeriesDetail, now time.Time) []part {
	st := e.status
	switch st.State {
	case store.EpisodeImported:
		if st.Tier == "" {
			return []part{{"in the library", sSubtle}}
		}
		if st.Tier != d.Best {
			// Below the best tier: an upgrade may come.
			return []part{{clean(st.Tier), sAmber}}
		}
		return []part{{clean(st.Tier), sSubtle}}
	case store.EpisodeUpcoming:
		if e.episode.Aired == "" || e.episode.Aired != d.Summary.NextAired {
			return nil
		}
		t, err := time.ParseInLocation(time.DateOnly, e.episode.Aired, time.Local)
		if err != nil {
			return nil
		}
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
		switch days := int(t.Sub(today).Hours()/24 + 0.5); days {
		case 0:
			return []part{{"airs today", sMuted}}
		case 1:
			return []part{{"airs tomorrow", sMuted}}
		default:
			return []part{{"airs in " + strconv.Itoa(days) + " days", sMuted}}
		}
	}
	return statusParts(st.State, st.Download, st.Summary, st.NextRetry, st.Attempts, now)
}

func episodeGlyph(state string) (string, lipgloss.Style) {
	switch state {
	case store.EpisodeImported:
		return "✓", sMint
	case store.EpisodeFailed:
		return "✗", sRed
	case store.EpisodeAttention:
		return "▲", sAmber
	case store.EpisodeDownloading:
		return "◐", sAmber
	case store.EpisodeProcessing, store.EpisodeImporting:
		return "●", sAmber
	case store.EpisodeWaiting, store.EpisodeNotSearched:
		return "○", sSubtle
	case store.EpisodeUpcoming:
		return "·", sSubtle
	}
	return " ", sSubtle
}

// seasonGlyph is the glyph of the season's most urgent episode, or ✓ when
// every episode that counts is in the library.
func seasonGlyph(se season) (string, lipgloss.Style) {
	best, rank := "", 99
	for _, e := range se.episodes {
		if r, ok := stateRank[e.status.State]; ok && r < rank {
			best, rank = e.status.State, r
		}
	}
	if best == "" {
		for _, e := range se.episodes {
			if e.status.State == store.EpisodeImported {
				return episodeGlyph(store.EpisodeImported)
			}
		}
	}
	return episodeGlyph(best)
}

func selectBg(selected bool) func(lipgloss.Style) lipgloss.Style {
	return func(st lipgloss.Style) lipgloss.Style {
		if selected {
			return st.Background(cSelect)
		}
		return st
	}
}

// fill pads a row to width, keeping the cursor background across.
func fill(line string, width int, bg func(lipgloss.Style) lipgloss.Style) string {
	if w := lipgloss.Width(line); w < width {
		line += bg(lipgloss.NewStyle()).Render(strings.Repeat(" ", width-w))
	}
	return line
}

func (s *showScreen) flashed() *flash { return &s.flash }
