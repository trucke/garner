package tui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

// Releases shows what a search found for one episode, ranked, with the
// reasons for the release under the cursor.

type (
	releasesMsg struct {
		from   *releasesScreen
		search api.Search
		err    error
	}
	startedMsg struct {
		download api.Download
		err      error
	}
)

type releasesScreen struct {
	sess    *session
	back    *showScreen
	episode episodeRow
	// season is set for a season search, which lists packs only.
	season    int
	search    *api.Search
	progress  *api.SearchProgress
	searching bool
	// cancel stops the running search, so leaving the screen cannot let
	// its late result replace a newer search's.
	cancel   context.CancelFunc
	err      error
	selected int64 // candidate ID
	offset   int
	confirm  int64 // a rejected release waiting for a second enter
	busy     string
	flash    flash
}

// newReleases opens the releases of an episode; with fresh set it searches
// again first, otherwise it shows the stored search.
func newReleases(sess *session, back *showScreen, e episodeRow, fresh bool) *releasesScreen {
	return &releasesScreen{sess: sess, back: back, episode: e, searching: fresh}
}

// newSeasonReleases searches a whole season for packs.
func newSeasonReleases(sess *session, back *showScreen, season int) *releasesScreen {
	return &releasesScreen{sess: sess, back: back, season: season, searching: true}
}

func (s *releasesScreen) init() tea.Cmd {
	if s.searching {
		return s.run()
	}
	cl, id := s.sess.client, s.episode.episode.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		sr, err := cl.EpisodeSearch(ctx, id)
		return releasesMsg{s, sr, err}
	}
}

// run searches the indexers. It takes as long as they take to answer.
func (s *releasesScreen) run() tea.Cmd {
	s.searching, s.progress, s.err, s.confirm = true, nil, nil, 0
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	s.cancel = cancel
	cl, id, series, season := s.sess.client, s.episode.episode.ID, s.back.id, s.season
	return func() tea.Msg {
		defer cancel()
		if season > 0 {
			sr, err := cl.SearchSeason(ctx, series, season)
			return releasesMsg{s, sr, err}
		}
		sr, err := cl.SearchEpisode(ctx, id)
		return releasesMsg{s, sr, err}
	}
}

func (s *releasesScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case releasesMsg:
		if msg.from != s {
			return s, nil
		}
		fresh := s.searching
		s.searching = false
		var server *client.ServerError
		switch {
		case errors.Is(msg.err, client.ErrUnauthorized):
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		case errors.As(msg.err, &server) && server.Status == http.StatusNotFound && !fresh:
			// Never searched: search now.
			return s, s.run()
		case msg.err != nil:
			s.err = msg.err
			return s, nil
		}
		s.search = &msg.search
		if c := s.candidates(); len(c) > 0 && !slices.ContainsFunc(c, func(c api.Candidate) bool { return c.ID == s.selected }) {
			s.selected, s.offset = c[0].ID, 0
		}
		if fresh {
			glyph := gWaiting
			if msg.search.Outcome == "found" {
				glyph = gDone
			}
			s.flash.info(glyph + " " + sNormal.Render(clean(msg.search.Summary)))
		}
	case startedMsg:
		s.busy = ""
		switch {
		case errors.Is(msg.err, client.ErrUnauthorized):
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		case msg.err != nil:
			s.flash.stay(gFailed + " " + sRed.Render("Could not start the download: "+clean(msg.err.Error())))
			return s, nil
		}
		// The episode screen follows the download; a pack's belongs to its
		// first missing episode.
		next := newEpisode(s.sess, s.back, msg.download.EpisodeID)
		return s, func() tea.Msg { return switchMsg{next} }
	case eventMsg:
		if msg.Type == "search.progress" {
			var p api.SearchProgress
			if json.Unmarshal(msg.Data, &p) != nil || !s.searching {
				break
			}
			ours := p.EpisodeID == s.episode.episode.ID
			if s.season > 0 {
				ours = p.SeriesID == s.back.id && p.Season == s.season
			}
			if ours {
				s.progress = &p
			}
		}
	case tea.KeyPressMsg:
		if s.busy != "" {
			return s, nil
		}
		key := msg.String()
		if key != "enter" {
			s.confirm = 0
		}
		s.flash.clear()
		return s, s.key(key)
	}
	return s, nil
}

func (s *releasesScreen) key(key string) tea.Cmd {
	switch key {
	case "esc":
		if s.cancel != nil {
			s.cancel()
		}
		return func() tea.Msg { return switchMsg{s.back} }
	case "j", "down":
		s.move(1)
	case "k", "up":
		s.move(-1)
	case "g", "home":
		s.move(-1 << 30)
	case "G", "end":
		s.move(1 << 30)
	case "ctrl+d", "pgdown":
		s.move(10)
	case "ctrl+u", "pgup":
		s.move(-10)
	case "r":
		if !s.searching {
			return s.run()
		}
	case "enter":
		return s.download()
	}
	return nil
}

func (s *releasesScreen) download() tea.Cmd {
	c, ok := s.cursor()
	if !ok || s.searching || s.err != nil {
		// While an error shows, the list underneath is not on screen.
		return nil
	}
	if c.Verdict == "reject" && s.confirm != c.ID {
		// A rejected release is downloaded only when asked twice.
		s.confirm = c.ID
		why := "it was rejected"
		if n := min(max(c.Decisive, 1), len(c.Reasons)); n > 0 {
			why = clean(strings.Join(c.Reasons[:n], "; "))
		}
		s.flash.stay(gFailed + " " + sNormal.Render("Rejected: "+why+". Press ") + sKey.Render("enter") + sNormal.Render(" again to download it anyway."))
		return nil
	}
	s.confirm = 0
	s.busy = "Starting the download…"
	cl, id, pack := s.sess.client, c.ID, s.season > 0
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if pack {
			d, err := cl.DownloadSeasonCandidate(ctx, id)
			return startedMsg{d, err}
		}
		d, err := cl.DownloadCandidate(ctx, id)
		return startedMsg{d, err}
	}
}

// candidates are the releases in display order: accepted, then last
// resorts, then rejected, each by rank.
func (s *releasesScreen) candidates() []api.Candidate {
	if s.search == nil {
		return nil
	}
	order := map[string]int{"accept": 0, "fallback": 1, "reject": 2}
	c := slices.Clone(s.search.Candidates)
	slices.SortStableFunc(c, func(a, b api.Candidate) int {
		return cmp.Or(cmp.Compare(order[a.Verdict], order[b.Verdict]), cmp.Compare(a.Rank, b.Rank))
	})
	return c
}

func (s *releasesScreen) cursor() (api.Candidate, bool) {
	c := s.candidates()
	i := slices.IndexFunc(c, func(c api.Candidate) bool { return c.ID == s.selected })
	if i < 0 {
		return api.Candidate{}, false
	}
	return c[i], true
}

func (s *releasesScreen) move(n int) {
	c := s.candidates()
	if len(c) == 0 {
		return
	}
	i := max(0, slices.IndexFunc(c, func(c api.Candidate) bool { return c.ID == s.selected }))
	s.selected = c[min(len(c)-1, max(0, i+n))].ID
}

func (s *releasesScreen) view(width, height int) string {
	var b strings.Builder
	label := episodeLabel(s.episode)
	if s.season > 0 {
		label = fmt.Sprintf("Season %d", s.season)
	}
	b.WriteString(header(width, s.sess, "Garner", "Library", clean(s.back.name), label, "Releases") + "\n\n")
	b.WriteString(s.facts(width) + "\n\n")

	switch {
	case s.searching:
		b.WriteString(s.progressLines())
	case s.err != nil:
		b.WriteString("   " + gFailed + " " + sRed.Render(clean(s.err.Error())) + sMuted.Render(" Press ") + sKey.Render("r") + sMuted.Render(" to search again."))
	case s.search != nil:
		b.WriteString(s.table(width, height))
	}
	return screenLayout(width, height, b.String(), s.bottom())
}

func (s *releasesScreen) facts(width int) string {
	parts := episodeFacts(s.episode)
	if s.season > 0 {
		parts = s.seasonFacts()
	}
	switch {
	case s.searching:
		parts = append(parts, sSubtle.Render("Searched")+"  "+gWorking+" "+sAmber.Render("running"))
	case s.search != nil:
		searched := when(s.search.Searched, time.Now())
		if n := len(s.search.Indexers); n > 0 {
			searched += fmt.Sprintf(" · %d indexers", n)
			if n == 1 {
				searched = strings.TrimSuffix(searched, "s")
			}
		}
		parts = append(parts, fact("Searched", searched))
	}
	return factLines(width, parts...)
}

// seasonFacts are the season and how many of its episodes are missing.
func (s *releasesScreen) seasonFacts() []string {
	parts := []string{fact("Season", strconv.Itoa(s.season))}
	for _, se := range s.back.seasons() {
		if se.number != s.season {
			continue
		}
		missing := 0
		for _, e := range se.episodes {
			if e.status.State == api.EpisodeNotSearched || e.status.State == api.EpisodeWaiting {
				missing++
			}
		}
		parts = append(parts, fact("Missing", fmt.Sprintf("%d of %d episodes", missing, len(se.episodes))))
	}
	return parts
}

// progressLines show each indexer of a running search.
func (s *releasesScreen) progressLines() string {
	if s.progress == nil {
		return "   " + gWorking + " " + sMuted.Render("Asking Prowlarr for its indexers…") + "\n"
	}
	nameWidth := 0
	for _, ix := range s.progress.Indexers {
		nameWidth = max(nameWidth, ansi.StringWidth(clean(ix.Name)))
	}
	var b strings.Builder
	for _, ix := range s.progress.Indexers {
		name := pad(sNormal.Render(clean(ix.Name)), nameWidth+3)
		switch {
		case !ix.Done:
			b.WriteString("   " + gWorking + " " + name + sMuted.Render("asking") + "\n")
		case ix.Error != "":
			b.WriteString("   " + gFailed + " " + name + sRed.Render(clean(ix.Error)) + "\n")
		default:
			b.WriteString("   " + gDone + " " + name + sMuted.Render(plural(ix.Results, "result")) + "\n")
		}
	}
	return b.String()
}

// Column widths of the releases table, except the release name.
const (
	tierWidth  = len("English 1080p")
	audioWidth = len("DE, EN, JA")
	sizeWidth  = len("99.9 GB")
	ageWidth   = len("11mo")
)

func (s *releasesScreen) table(width, height int) string {
	all := s.candidates()
	indexerWidth := len("Indexer")
	for _, c := range all {
		indexerWidth = max(indexerWidth, min(16, ansi.StringWidth(clean(c.Indexer))))
	}
	cols := releaseCols{audio: audioWidth, age: ageWidth, indexer: indexerWidth}
	longest := len("Release")
	for _, c := range all {
		longest = max(longest, ansi.StringWidth(clean(c.Title)))
	}
	// Narrow: the names keep room; Indexer, then Age, then Audio go.
	for _, col := range []*int{&cols.indexer, &cols.age, &cols.audio} {
		if width-cols.fixed() >= min(longest, minReleaseName) {
			break
		}
		*col = 0
	}
	// As wide as the longest name, so the columns stay together.
	cols.name = max(20, min(longest, width-cols.fixed()))
	// The rule and the WHY frame end with the table.
	tableWidth := cols.fixed() + cols.name - 6

	var b strings.Builder
	b.WriteString(cols.head() + "\n")
	b.WriteString("   " + sBorder.Render(strings.Repeat("─", max(0, tableWidth))) + "\n")

	// Without an accepted release, the summary says what is missing.
	usable := slices.ContainsFunc(all, func(c api.Candidate) bool { return c.Verdict == "accept" })
	var lines []string // rows and gaps, for scrolling
	var keys []int64   // the candidate on each line, 0 for text
	if !usable {
		line := "   " + gWaiting + " " + sNormal.Render(clean(s.search.Summary))
		if s.search.NextRetry != nil {
			line += sMuted.Render(" · " + nextSearch(*s.search.NextRetry, time.Now()))
		}
		lines, keys = append(lines, line), append(keys, 0)
		if len(all) > 0 {
			lines, keys = append(lines, ""), append(keys, 0)
		}
	}
	for i, c := range all {
		if i > 0 && c.Verdict == "reject" && all[i-1].Verdict != "reject" {
			lines, keys = append(lines, ""), append(keys, 0)
		}
		lines = append(lines, cols.row(c, width, c.ID == s.selected))
		keys = append(keys, c.ID)
	}

	why := s.why(tableWidth)
	page := max(3, height-showTop-1-len(why)-1)
	cursor := max(0, slices.Index(keys, s.selected))
	s.offset = min(max(s.offset, cursor-page+1), cursor)
	if s.offset > 0 && cursor < page {
		s.offset = 0
	}
	for _, l := range lines[s.offset:min(len(lines), s.offset+page)] {
		b.WriteString(l + "\n")
	}
	if len(why) > 0 {
		b.WriteString("\n" + strings.Join(why, "\n") + "\n")
	}
	return b.String()
}

// minReleaseName is the room release names keep on a narrow terminal.
const minReleaseName = 30

// releaseCols sizes the releases table; Audio, Age and Indexer are 0 when
// left out on a narrow terminal.
type releaseCols struct {
	name, audio, age, indexer int
}

// fixed is the table's width without the release name: margins, gaps and
// the columns shown.
func (cols releaseCols) fixed() int {
	n := 3 + 1 + 3 + 3 + tierWidth + 3 + sizeWidth + 3
	for _, w := range []int{cols.audio, cols.age, cols.indexer} {
		if w > 0 {
			n += w + 3
		}
	}
	return n
}

func (cols releaseCols) head() string {
	head := func(t string, w int, right bool) string { return sSubtle.Render(fit(t, w, right)) + "   " }
	line := "   " + " " + "   " + head("Release", cols.name, false) + head("Preference", tierWidth, false)
	if cols.audio > 0 {
		line += head("Audio", cols.audio, false)
	}
	line += head("Size", sizeWidth, true)
	if cols.age > 0 {
		line += head("Age", cols.age, true)
	}
	if cols.indexer > 0 {
		line += head("Indexer", cols.indexer, false)
	}
	return strings.TrimRight(line, " ")
}

func (cols releaseCols) row(c api.Candidate, width int, selected bool) string {
	bg := selectBg(selected)
	glyph, glyphStyle := "✓", sMint
	text := sNormal
	tier, tierStyle := clean(c.Tier), sNormal
	switch c.Verdict {
	case "fallback":
		glyph, glyphStyle = "~", sAmber
		tier, tierStyle = "last resort", sAmber
	case "reject":
		glyph, glyphStyle = "✗", sRed
		text, tier, tierStyle = sSubtle, "", sSubtle
	}
	audio := make([]string, len(c.Audio))
	for i, a := range c.Audio {
		audio[i] = strings.ToUpper(clean(a))
	}
	age := ""
	if c.Published != nil {
		age = shortAge(time.Since(*c.Published))
	}
	gap := bg(lipgloss.NewStyle()).Render("   ")
	line := gap + bg(glyphStyle).Render(glyph) + gap +
		bg(text).Render(fit(cutMiddle(clean(c.Title), cols.name), cols.name, false)) + gap +
		bg(tierStyle).Render(fit(tier, tierWidth, false)) + gap
	if cols.audio > 0 {
		line += bg(text).Render(fit(strings.Join(audio, ", "), cols.audio, false)) + gap
	}
	line += bg(text).Render(fit(size(c.Size), sizeWidth, true))
	if cols.age > 0 {
		line += gap + bg(text).Render(fit(age, cols.age, true))
	}
	if cols.indexer > 0 {
		line += gap + bg(text).Render(fit(clean(c.Indexer), cols.indexer, false))
	}
	return fill(line, width, bg)
}

// why explains the release under the cursor: its full name, then the
// reasons that decided its verdict, then the facts.
func (s *releasesScreen) why(w int) []string {
	c, ok := s.cursor()
	if !ok {
		return nil
	}
	lines := append([]string{sNormal.Render(clean(c.Title)), ""}, reasonLines(c.Verdict, c.Reasons, c.Decisive, w-8)...)
	return strings.Split(indent(frame("why", lines, w), 3), "\n")
}

// reasonLines marks the reasons that decided a verdict with its glyph and
// the remaining facts with ·, wrapped to w cells.
func reasonLines(verdict string, reasons []string, decisive, w int) []string {
	mark, style := "✓", sMint
	switch verdict {
	case "reject":
		mark, style = "✗", sRed
	case "fallback":
		mark, style = "~", sAmber
	}
	switch {
	case decisive > 0:
	case verdict == "accept":
		// Stored before accepted releases had the count: all of them.
		decisive = len(reasons)
	default:
		// Stored before the count was kept; the first reason always decides.
		decisive = 1
	}
	var lines []string
	for i, r := range reasons {
		glyph := style.Render(mark)
		if i >= decisive {
			glyph = sSubtle.Render("·") // a fact, not why
		}
		for j, l := range strings.Split(ansi.Wrap(clean(r), max(10, w), ""), "\n") {
			if j > 0 {
				glyph = " "
			}
			lines = append(lines, glyph+" "+sNormal.Render(l))
		}
	}
	return lines
}

func (s *releasesScreen) bottom() string {
	switch {
	case s.busy != "":
		return sMuted.Render(s.busy)
	case s.flash.text != "":
		return s.flash.text
	case s.searching:
		return keyBar("esc", "back")
	}
	return keyBar("j k", "move", "enter", "download", "r", "search again", "A", "activity", "?", "help", "esc", "back")
}

func (s *releasesScreen) flashed() *flash { return &s.flash }

func (s *releasesScreen) captures() bool { return s.busy != "" }

func (s *releasesScreen) help() (string, []string) {
	return "Releases", []string{
		"enter", "download; a rejected one asks twice", "r", "search again",
	}
}

// cutMiddle shortens a release name in the middle, keeping its start and
// the group tag after the last hyphen, as in "Show.S01E01.…-GROUP".
func cutMiddle(name string, width int) string {
	if ansi.StringWidth(name) <= width {
		return name
	}
	tail := ""
	if i := strings.LastIndex(name, "-"); i > 0 && ansi.StringWidth(name[i:]) <= width/2 {
		tail = name[i:]
	}
	return ansi.Truncate(name, width-ansi.StringWidth(tail), "…") + tail
}

// shortAge is a release's age: 45m, 5h, 2d, 3mo, 2y.
func shortAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", max(0, int(d.Minutes())))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d < 730*24*time.Hour:
		return fmt.Sprintf("%dmo", int(d.Hours()/24/30))
	}
	return fmt.Sprintf("%dy", int(d.Hours()/24/365))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// fact is a labelled fact, as in "Aired  8 Jul 2003".
func fact(label, value string) string { return sSubtle.Render(label) + "  " + sNormal.Render(value) }

// episodeFacts are the episode's label, title and air date.
func episodeFacts(e episodeRow) []string {
	title := clean(cmp.Or(e.episode.NameEng, e.episode.Name))
	parts := []string{fact("Episode", strings.TrimSpace(episodeLabel(e)+" "+title))}
	if t, err := time.Parse(time.DateOnly, e.episode.Aired); err == nil {
		parts = append(parts, fact("Aired", t.Format("2 Jan 2006")))
	}
	return parts
}
