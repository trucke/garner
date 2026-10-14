package tui

import (
	"strings"
	"unicode"

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
	return strings.Join(parts, "    ")
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
	return strings.Join(append(lines, ansi.Truncate(" "+bottom, width, "")), "\n")
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
