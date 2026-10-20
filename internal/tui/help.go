package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Help is an overlay that lists the current screen's keys, the keys every
// screen has, and the glyphs.

// capturer is a screen that takes every key itself for now: while text is
// typed or a question is open. ? and A are then its own.
type capturer interface{ captures() bool }

// keyHelper is a screen that lists its keys in the Help overlay: its name
// and key, label pairs.
type keyHelper interface {
	help() (name string, keys []string)
}

// everywhere are the keys every screen has.
var everywhere = []string{
	"j k", "move", "enter", "open",
	"gg G", "top, bottom", "esc", "back",
	"ctrl+d u", "half page", "A", "activity",
	"?", "this help", "q", "quit, from the Library",
}

// glyphs are the states every list shows, in pipeline order.
var glyphs = []struct {
	glyph string
	style lipgloss.Style
	label string
}{
	{"·", sSubtle, "not aired"}, {"○", sSubtle, "nothing yet"}, {"◐", sAmber, "downloading"}, {"●", sAmber, "processing"},
	{"✓", sMint, "in the library"}, {"▲", sAmber, "needs you"}, {"✗", sRed, "failed"}, {"~", sAmber, "last resort"},
}

// helpLines are the overlay's lines for room cells: keys in two columns
// and four glyphs a row, or fewer when that is too wide.
func helpLines(s screen, room int) []string {
	var lines []string
	if h, ok := s.(keyHelper); ok {
		name, keys := h.help()
		lines = append(lines, sBold.Render(name))
		lines = append(lines, keyColumns(keys, room)...)
		lines = append(lines, "")
	}
	lines = append(lines, sBold.Render("Everywhere"))
	lines = append(lines, keyColumns(everywhere, room)...)
	lines = append(lines, "", sBold.Render("Glyphs"))
	const glyphWidth = 20
	perRow := max(1, min(4, (room-2)/glyphWidth))
	for i := 0; i < len(glyphs); i += perRow {
		var row []string
		for _, g := range glyphs[i:min(i+perRow, len(glyphs))] {
			row = append(row, pad(g.style.Render(g.glyph)+"  "+sMuted.Render(g.label), glyphWidth))
		}
		lines = append(lines, strings.TrimRight("  "+strings.Join(row, ""), " "))
	}
	return append(lines, "", keyBar("? esc", "close", "a", "about Garner"))
}

// keyColumns lays key, label pairs out in two columns, each as wide as
// its own keys and labels, or in one when two do not fit room.
func keyColumns(pairs []string, room int) []string {
	two := columnLines(pairs, 2)
	for _, l := range two {
		if lipgloss.Width(l) > room {
			return columnLines(pairs, 1)
		}
	}
	return two
}

func columnLines(pairs []string, columns int) []string {
	keyWidth, labelWidth := make([]int, columns), make([]int, columns)
	for i := 0; i+1 < len(pairs); i += 2 {
		col := i / 2 % columns
		keyWidth[col] = max(keyWidth[col], lipgloss.Width(pairs[i]))
		labelWidth[col] = max(labelWidth[col], lipgloss.Width(pairs[i+1]))
	}
	cell := func(i int) string {
		col := i / 2 % columns
		return pad(sKey.Render(pairs[i]), keyWidth[col]+2) + pad(sMuted.Render(pairs[i+1]), labelWidth[col])
	}
	var lines []string
	for i := 0; i+1 < len(pairs); i += 2 * columns {
		cells := []string{}
		for c := 0; c < columns && i+2*c+1 < len(pairs); c++ {
			cells = append(cells, cell(i+2*c))
		}
		lines = append(lines, strings.TrimRight("  "+strings.Join(cells, "    "), " "))
	}
	return lines
}
