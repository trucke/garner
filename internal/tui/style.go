package tui

import (
	"slices"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Palette from the "Style" board in the Paper file "Garner".
var (
	cFg     = lipgloss.Color("#EDF1F2")
	cMuted  = lipgloss.Color("#9199A3")
	cSubtle = lipgloss.Color("#80848D")
	cBorder = lipgloss.Color("#3D3D47")
	cSelect = lipgloss.Color("#242037")
	cMint   = lipgloss.Color("#75B39C")
	cAmber  = lipgloss.Color("#F79926")
	cRed    = lipgloss.Color("#F54D47")
	cIris   = lipgloss.Color("#9BA0E3")
)

var (
	sNormal  = lipgloss.NewStyle().Foreground(cFg)
	sBold    = sNormal.Bold(true)
	sMuted   = lipgloss.NewStyle().Foreground(cMuted)
	sSubtle  = lipgloss.NewStyle().Foreground(cSubtle)
	sBorder  = lipgloss.NewStyle().Foreground(cBorder)
	sMint    = lipgloss.NewStyle().Foreground(cMint)
	sAmber   = lipgloss.NewStyle().Foreground(cAmber)
	sRed     = lipgloss.NewStyle().Foreground(cRed)
	sKey     = sMint.Bold(true)
	sCommand = sBold.Background(cSelect)
)

// Glyphs, one set everywhere (see docs/tui-design.md).
var (
	gDone    = sMint.Render("✓")
	gWorking = sAmber.Render("●")
	gWaiting = sSubtle.Render("○")
	gFailed  = sRed.Render("✗")
)

// clean makes untrusted text, such as release names and server messages,
// safe to print: control characters (including the escape that starts
// terminal sequences) are removed, line breaks and tabs become spaces, and
// direction overrides that could make text read differently are dropped.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Bidi_Control, r):
			return -1
		}
		return r
	}, s)
}

// pad fills styled text with spaces to width cells.
func pad(s string, width int) string {
	return s + strings.Repeat(" ", max(0, width-lipgloss.Width(s)))
}

// frame draws a framed pane: square corners in the border colour, the title
// uppercase in subtle cut into the top edge, two cells of padding. Lines
// wider than the pane are wrapped.
func frame(title string, lines []string, width int) string {
	inner := width - 6
	title = strings.ToUpper(title)
	top := sBorder.Render("┌─ ") + sSubtle.Render(title) + sBorder.Render(" "+strings.Repeat("─", max(0, width-5-len(title)))+"┐")
	out := []string{top}
	side := sBorder.Render("│")
	for _, l := range lines {
		for _, w := range strings.Split(ansi.Wrap(l, inner, ""), "\n") {
			out = append(out, side+"  "+pad(w, inner)+"  "+side)
		}
	}
	out = append(out, sBorder.Render("└"+strings.Repeat("─", width-2)+"┘"))
	return strings.Join(out, "\n")
}

// keyBar renders the bottom line's keys: the key in mint bold, its label
// in muted.
func keyBar(pairs ...string) string {
	parts := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, sKey.Render(pairs[i])+" "+sMuted.Render(pairs[i+1]))
	}
	return strings.Join(parts, keyGap)
}

// minName is the room a table's name column keeps on a narrow terminal;
// other columns give way first.
const minName = 24

// keyGap separates the keys of a key bar.
const keyGap = "    "

// fitKeys makes a key bar fit width by leaving out whole keys: "j k move"
// first, then from the right. The Help key and the last one, usually
// "esc back", stay: Help lists every key. Other lines are cut.
func fitKeys(bar string, width int) string {
	parts := strings.Split(bar, keyGap)
	if lipgloss.Width(bar) <= width || len(parts) < 2 {
		return ansi.Truncate(bar, width, "")
	}
	stays := func(i int) bool { return i == len(parts)-1 || strings.HasPrefix(ansi.Strip(parts[i]), "? ") }
	var order []int
	if strings.HasPrefix(ansi.Strip(parts[0]), "j k ") {
		order = append(order, 0)
	}
	for i := len(parts) - 1; i > 0; i-- {
		if !stays(i) {
			order = append(order, i)
		}
	}
	left := map[int]bool{}
	join := func() string {
		var kept []string
		for i, p := range parts {
			if !left[i] {
				kept = append(kept, p)
			}
		}
		return strings.Join(kept, keyGap)
	}
	for _, i := range order {
		if lipgloss.Width(join()) <= width {
			break
		}
		left[i] = true
	}
	return ansi.Truncate(join(), width, "")
}

// factLines lays labelled facts out on one line, closer together when the
// line is narrow, and over more lines when they still do not fit.
func factLines(width int, facts ...string) string {
	room := width - 3
	for _, gap := range []string{"      ", "   "} {
		if line := strings.Join(facts, gap); lipgloss.Width(line) <= room {
			return "   " + line
		}
	}
	var lines []string
	for _, f := range facts {
		if n := len(lines); n > 0 && lipgloss.Width(lines[n-1]+"   "+f) <= room {
			lines[n-1] += "   " + f
		} else {
			lines = append(lines, f)
		}
	}
	return "   " + strings.Join(lines, "\n   ")
}

// breadcrumb renders the first row: Garner › … › screen, the ends in bold.
func breadcrumb(parts ...string) string {
	out := make([]string, len(parts))
	for i, p := range parts {
		if i == 0 || i == len(parts)-1 {
			out[i] = sBold.Render(p)
		} else {
			out[i] = sMuted.Render(p)
		}
	}
	return strings.Join(out, sSubtle.Render(" › "))
}

// screenLayout places body at the top and bottom on the last row, one cell
// in, cutting the body when the terminal is too short.
func screenLayout(width, height int, body, bottom string) string {
	lines := strings.Split(body, "\n")
	if len(lines) > height-1 {
		lines = lines[:max(0, height-1)]
	}
	for len(lines) < height-1 {
		lines = append(lines, "")
	}
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, width, "")
	}
	return strings.Join(append(lines, " "+fitKeys(bottom, width-1)), "\n")
}

// indent shifts every line of s right by n cells.
func indent(s string, n int) string {
	prefix := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}

var cFaded = lipgloss.Color("#3A3A44")

// overlay draws a rounded box with lines centred over base, everything
// behind it in the faded colour, as the overlay boards show.
func overlay(base string, lines []string, width, height int) string {
	boxWidth := 0
	for _, l := range lines {
		boxWidth = max(boxWidth, lipgloss.Width(l))
	}
	boxWidth = max(6, min(boxWidth+6, width)) // borders and two cells of padding, within the screen
	box := []string{sBorder.Render("╭" + strings.Repeat("─", boxWidth-2) + "╮")}
	for _, l := range lines {
		box = append(box, sBorder.Render("│")+"  "+pad(ansi.Truncate(l, boxWidth-6, "…"), boxWidth-6)+"  "+sBorder.Render("│"))
	}
	box = append(box, sBorder.Render("╰"+strings.Repeat("─", boxWidth-2)+"╯"))

	faded := lipgloss.NewStyle().Foreground(cFaded)
	rows := strings.Split(base, "\n")
	for len(rows) < height {
		rows = append(rows, "")
	}
	top := max(0, (height-len(box))/2-2)
	left := max(0, (width-boxWidth)/2)
	for y, row := range rows {
		plain := ansi.Strip(row)
		i := y - top
		if i < 0 || i >= len(box) {
			rows[y] = faded.Render(plain)
			continue
		}
		plain += strings.Repeat(" ", max(0, left-ansi.StringWidth(plain)))
		rows[y] = faded.Render(ansi.Truncate(plain, left, "")) + box[i] + faded.Render(ansi.TruncateLeft(plain, left+boxWidth, ""))
	}
	return strings.Join(rows, "\n")
}

// choice renders one option of an overlay, highlighted when chosen.
func choice(label string, chosen bool) string {
	if chosen {
		return sNormal.Background(cSelect).Render(" " + label + " ")
	}
	return sNormal.Render(" " + label + " ")
}

// flashFor is how long a success or info message stays in the bottom line.
const flashFor = 3 * time.Second

// flash is a message in the bottom line. Success and info messages go after
// flashFor; errors and work in progress stay until replaced or the next
// key, so there is time to read them.
type flash struct {
	text   string
	sticky bool
	seq    int // counts messages, so a timer clears only its own
	timed  int // the message a timer runs for
}

func (f *flash) info(text string) { f.text, f.sticky = text, false; f.seq++ }
func (f *flash) stay(text string) { f.text, f.sticky = text, true; f.seq++ }

func (f *flash) clear() {
	if f.text != "" {
		f.text = ""
		f.seq++
	}
}

// flasher is a screen with a bottom-line message.
type flasher interface{ flashed() *flash }

// flashDoneMsg ends a message's time, if it is still showing.
type flashDoneMsg struct {
	f   *flash
	seq int
}

// expire starts the timer for a new message that should go by itself.
func (f *flash) expire() tea.Cmd {
	if f.text == "" || f.sticky || f.timed == f.seq {
		return nil
	}
	f.timed = f.seq
	seq := f.seq
	return tea.Tick(flashFor, func(time.Time) tea.Msg { return flashDoneMsg{f, seq} })
}

// scroller moves through content taller than the screen: j k a line,
// ctrl+d u half a page, gg G to the ends. Screens with tables move their
// cursor instead.
type scroller struct {
	offset, page, max int
	last              string // the key before, for gg
}

// key moves the content for a scrolling key and reports whether it was one.
func (s *scroller) key(key string) bool {
	prev := s.last
	s.last = key
	switch key {
	case "j", "down":
		s.offset++
	case "k", "up":
		s.offset--
	case "ctrl+d", "pgdown":
		s.offset += max(1, s.page/2)
	case "ctrl+u", "pgup":
		s.offset -= max(1, s.page/2)
	case "G", "end":
		s.offset = s.max
	case "home":
		s.offset = 0
	case "g":
		if prev == "g" {
			s.offset, s.last = 0, ""
		}
	default:
		return false
	}
	s.offset = min(max(0, s.offset), s.max)
	return true
}

// window shows room of the lines from the offset; the first and last line
// say when there is more above or below.
func (s *scroller) window(lines []string, room int) []string {
	room = max(3, room)
	s.page, s.max = room, max(0, len(lines)-room)
	s.offset = min(max(0, s.offset), s.max)
	if s.max == 0 {
		return lines
	}
	view := slices.Clone(lines[s.offset : s.offset+room])
	if s.offset > 0 {
		view[0] = "   " + sSubtle.Render("↑ more above")
	}
	if s.offset < s.max {
		view[room-1] = "   " + sSubtle.Render("↓ more below · j k scroll")
	}
	return view
}

// layout is screenLayout for a screen whose content scrolls below its
// first two lines, the breadcrumb and a blank.
func (s *scroller) layout(width, height int, body, bottom string) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	const fixed = 2
	if len(lines) > fixed {
		lines = append(lines[:fixed:fixed], s.window(lines[fixed:], height-1-fixed)...)
	}
	return screenLayout(width, height, strings.Join(lines, "\n"), bottom)
}
