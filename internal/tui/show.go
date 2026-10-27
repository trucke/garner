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

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/client"
)

// Show lists a show's episodes by season.

type (
	seriesMsg struct {
		from   *showScreen
		detail api.SeriesDetail
		err    error
	}
)

type showScreen struct {
	sess    *session
	back    screen
	id      int64
	name    string
	detail  *api.SeriesDetail
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
	// asking is the question before searching every missing episode;
	// choice 1 is Search.
	asking bool
	choice int
	// files reports the files Garner took over when the show was added.
	files filesPrompt
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
	episode api.Episode
	pos     api.Position
	status  api.EpisodeStatus
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
		if s.detail != nil {
			var waiting []*api.Download
			for _, st := range s.detail.States {
				waiting = append(waiting, st.Download)
			}
			return s, redrawWhenSlow(waiting...)
		}
	case eventMsg:
		switch msg.Type {
		case client.EventConnected, "search.finished", "series.updated", "profiles.updated":
			// A changed profile changes the show's rules and its details page.
			return s, s.reload()
		case "download.updated":
			var d api.Download
			if json.Unmarshal(msg.Data, &d) != nil || s.holds(d.EpisodeID) {
				return s, s.reload()
			}
		case "series.deleted":
			var deleted struct{ ID int64 }
			if json.Unmarshal(msg.Data, &deleted) == nil && deleted.ID == s.id {
				return s, func() tea.Msg { return switchMsg{s.back} }
			}
		case "series.search":
			var p api.ShowProgress
			if json.Unmarshal(msg.Data, &p) == nil {
				return s, s.progressed(p)
			}
		}
	case searchShowMsg:
		if msg.from == s {
			return s, s.searched(msg)
		}
	case renamedMsg:
		reportRenamed(msg, &s.flash)
		return s, s.reload()
	case tea.KeyPressMsg:
		switch {
		case s.files.open:
			if s.files.key(msg.String()) {
				s.flash.stay(sMuted.Render("Renaming and moving " + plural(s.files.result.Renamable, "file") + "…"))
				return s, renameFiles(s.sess.client, s.id)
			}
			return s, nil
		case s.asking:
			return s, s.searchKey(msg.String())
		}
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
	return slices.ContainsFunc(s.detail.Episodes, func(e api.Episode) bool { return e.ID == episodeID })
}

func (s *showScreen) setDetail(d api.SeriesDetail) {
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
			if r, ok := stateRank[e.status.State]; ok && r <= stateRank[api.EpisodeNotSearched] && r < rank {
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
// regular seasons newest first, then the specials; episodes newest first.
func (s *showScreen) seasons() []season {
	if s.detail == nil {
		return nil
	}
	states := make(map[int64]api.EpisodeStatus, len(s.detail.States))
	for _, st := range s.detail.States {
		states[st.EpisodeID] = st
	}
	order := s.detail.Series.DefaultOrder
	bySeason := map[int]*season{}
	for _, e := range s.detail.Episodes {
		pos, ok := e.Numbers[order]
		if !ok {
			pos = api.Position{}
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
		// Newest first, like the seasons.
		slices.SortStableFunc(se.episodes, func(a, b episodeRow) int {
			return cmp.Or(cmp.Compare(b.pos.Number, a.pos.Number), cmp.Compare(b.episode.Aired, a.episode.Aired))
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
		} else if e, ok := s.cursorEpisode(); ok {
			return s.open(e)
		}
	case "s":
		var next screen
		switch e, ok := s.cursorEpisode(); {
		case ok:
			next = newReleases(s.sess, s, e, true)
		case s.cursor.season == 0:
			s.flash.info(sMuted.Render("Specials come one by one; search them on their own row."))
			return nil
		default:
			next = newSeasonReleases(s.sess, s, s.cursor.season)
		}
		return func() tea.Msg { return switchMsg{next} }
	case "S":
		s.askSearch()
	case "i":
		if s.detail != nil {
			next := newShowDetails(s.sess, s, s)
			return func() tea.Msg { return switchMsg{next} }
		}
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
	b.WriteString(s.facts(now, width) + "\n\n")
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
	page := screenLayout(width, height, b.String(), s.bottom())
	switch {
	case s.files.open:
		return overlay(page, s.files.lines(clean(s.name)), width, height)
	case s.asking:
		return overlay(page, s.searchQuestion(), width, height)
	}
	return page
}

func (s *showScreen) facts(now time.Time, width int) string {
	d := s.detail
	typ := "TV"
	if d.Series.Type == api.TypeAnime {
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
	facts := []string{fact("Type", typ), fact("Status", st), fact("Episodes", episodes)}
	if f := searchFact(d.Searching); f != "" {
		facts = append(facts, f)
	}
	return factLines(width, facts...)
}

func (s *showScreen) help() (string, []string) {
	return "Show", []string{
		"enter", "open the episode", "s", "search the episode, or the season for packs",
		"h l", "fold, unfold the season", "S", "search every missing episode and download",
		"tab", "next season", "i", "show details",
	}
}

func (s *showScreen) bottom() string {
	if s.flash.text != "" {
		return s.flash.text
	}
	search := "search season" // for its packs
	if _, ok := s.cursorEpisode(); ok {
		search = "new search"
	}
	keys := []string{"h l", "fold", "tab", "season", "s", search, "S", "search missing", "i", "show details", "A", "activity", "?", "help", "esc", "back"}
	if label := s.enterLabel(); label != "" {
		keys = append([]string{"enter", label}, keys...)
	}
	return keyBar(keys...)
}

// open is enter on an episode. A missing one opens its releases: the last
// search's, or a new search when it was never searched. Any other opens the
// episode screen, which follows its download or shows its file.
func (s *showScreen) open(e episodeRow) tea.Cmd {
	var next screen
	switch e.status.State {
	case api.EpisodeNotSearched, api.EpisodeWaiting:
		next = newReleases(s.sess, s, e, e.status.State == api.EpisodeNotSearched)
	case api.EpisodeUpcoming:
		s.flash.info(sMuted.Render(episodeLabel(e) + " has not aired yet; s searches anyway."))
		return nil
	default:
		next = newEpisode(s.sess, s, e.episode.ID)
	}
	return func() tea.Msg { return switchMsg{next} }
}

// enterLabel is what enter does on the cursor row, for the key bar; empty
// on a season row, where h l fold, and on an episode not aired yet.
func (s *showScreen) enterLabel() string {
	e, ok := s.cursorEpisode()
	switch {
	case !ok, e.status.State == api.EpisodeUpcoming:
		return ""
	case e.status.State == api.EpisodeNotSearched:
		return "search"
	case e.status.State == api.EpisodeWaiting:
		return "releases"
	}
	return "details"
}

// showCols sizes the episode table: glyph, episode, title, aired, size,
// state.
type showCols struct {
	width, title int
	size         bool // the Size column, left out when narrow
}

const airedWidth = len("26 Sep 2026")

func showColumns(d *api.SeriesDetail, width int) showCols {
	c := showCols{width: width, title: len("Title")}
	for _, e := range d.Episodes {
		c.title = max(c.title, ansi.StringWidth(clean(cmp.Or(e.NameEng, e.Name))))
	}
	// Margins and gaps: 3 + glyph 1 + 3 + episode 7 + 3 + title + 3 +
	// aired + 3 + size 7 + 3 + state.
	fixed := 3 + 1 + 3 + 7 + 3 + 3 + airedWidth + 3 + 7 + 3
	// Narrow: the titles keep room, State gives way, then Size goes.
	c.size = true
	state := 28
	if width-fixed-state < min(c.title, minName) {
		c.size, fixed, state = false, fixed-3-7, 20
	}
	c.title = max(10, min(c.title, 40, width-fixed-state))
	return c
}

func (c showCols) head() string {
	sz := ""
	if c.size {
		sz = sSubtle.Render(fit("Size", 7, true)) + "   "
	}
	return "   " + " " + "   " + sSubtle.Render(fit("Episode", 7, false)) + "   " + sSubtle.Render(fit("Title", c.title, false)) +
		"   " + sSubtle.Render(fit("Aired", airedWidth, false)) + "   " + sz + sSubtle.Render("State")
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
		// A file whose upgrade runs is still in the library.
		case e.status.State == api.EpisodeImported || e.status.Size > 0:
			imported++
			aired++
		case e.status.State == api.EpisodeUpcoming:
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

func (c showCols) episodeRow(e episodeRow, d *api.SeriesDetail, now time.Time, selected bool) string {
	bg := selectBg(selected)
	st := e.status
	main := sNormal
	if st.State == api.EpisodeImported || st.State == "" {
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
		bg(main).Render(fit(aired, airedWidth, false)) + gap
	if c.size {
		line += bg(main).Render(fit(size(st.Size), 7, true)) + gap
	}
	stateWidth := max(0, c.width-lipgloss.Width(line))
	return fill(line+renderParts(episodeParts(e, d, now), stateWidth, bg), c.width, bg)
}

// episodeParts describes an episode's state in the table.
func episodeParts(e episodeRow, d *api.SeriesDetail, now time.Time) []part {
	st := e.status
	switch st.State {
	case api.EpisodeImported:
		if st.Tier == "" {
			return []part{{"in the library", sSubtle}}
		}
		if st.Tier != d.Best {
			// Below the best tier: an upgrade may come.
			return []part{{clean(st.Tier), sAmber}}
		}
		return []part{{clean(st.Tier), sSubtle}}
	case api.EpisodeUpcoming:
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
	case api.EpisodeImported:
		return "✓", sMint
	case api.EpisodeFailed:
		return "✗", sRed
	case api.EpisodeAttention:
		return "▲", sAmber
	case api.EpisodeDownloading:
		return "◐", sAmber
	case api.EpisodeProcessing, api.EpisodeImporting:
		return "●", sAmber
	case api.EpisodeWaiting, api.EpisodeNotSearched:
		return "○", sSubtle
	case api.EpisodeUpcoming:
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
			if e.status.State == api.EpisodeImported {
				return episodeGlyph(api.EpisodeImported)
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

// captures: the search question takes every key, ? and A included.
func (s *showScreen) captures() bool { return s.asking || s.files.open }
