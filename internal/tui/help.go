package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Help is an overlay that lists the current screen's keys, the keys every
// screen has, and the glyphs. Board "Help" in the Paper file.

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

func helpLines(s screen) []string {
	var lines []string
	if h, ok := s.(keyHelper); ok {
		name, keys := h.help()
		lines = append(lines, sBold.Render(name))
		lines = append(lines, keyColumns(keys)...)
		lines = append(lines, "")
	}
	lines = append(lines, sBold.Render("Everywhere"))
	lines = append(lines, keyColumns(everywhere)...)
	lines = append(lines, "", sBold.Render("Glyphs"))
	for i := 0; i < len(glyphs); i += 4 {
		var row []string
		for _, g := range glyphs[i:min(i+4, len(glyphs))] {
			row = append(row, pad(g.style.Render(g.glyph)+"  "+sMuted.Render(g.label), 20))
		}
		lines = append(lines, "  "+strings.Join(row, ""))
	}
	return append(lines, "", keyBar("? esc", "close", "a", "about Garner"))
}

// keyColumns lays key, label pairs out in two columns, each as wide as
// its own keys and labels.
func keyColumns(pairs []string) []string {
	var keyWidth, labelWidth [2]int
	for i := 0; i+1 < len(pairs); i += 2 {
		col := i / 2 % 2
		keyWidth[col] = max(keyWidth[col], lipgloss.Width(pairs[i]))
		labelWidth[col] = max(labelWidth[col], lipgloss.Width(pairs[i+1]))
	}
	cell := func(i int) string {
		col := i / 2 % 2
		return pad(sKey.Render(pairs[i]), keyWidth[col]+2) + pad(sMuted.Render(pairs[i+1]), labelWidth[col])
	}
	var lines []string
	for i := 0; i+1 < len(pairs); i += 4 {
		line := "  " + cell(i)
		if i+3 < len(pairs) {
			line += "    " + cell(i+2)
		}
		lines = append(lines, strings.TrimRight(line, " "))
	}
	return lines
}
