package tui

import (
	"slices"

	"github.com/trucke/garner/internal/store"
)

// The monitor question asks which of a show's episodes Garner gets on its
// own, when adding a show and on its details page.

// monitorModes are the choices, the default first, as the server names them.
var monitorModes = []struct{ mode, label, means string }{
	{store.MonitorNew, "new episodes", "those that air from today on"},
	{store.MonitorAll, "all missing", "every aired episode not in the library"},
	{store.MonitorOff, "off", "none; only what you search for"},
}

// monitorLabel names a show's mode, such as "new episodes".
func monitorLabel(mode string) string {
	if i := monitorIndex(mode); i >= 0 {
		return monitorModes[i].label
	}
	return mode
}

func monitorIndex(mode string) int {
	return slices.IndexFunc(monitorModes, func(m struct{ mode, label, means string }) bool { return m.mode == mode })
}

// monitorKey moves the choice in the monitor question or answers it. done
// closes the question; mode is the answer, empty when cancelled.
func monitorKey(key string, chosen *int) (done bool, mode string) {
	switch key {
	case "esc":
		return true, ""
	case "k", "up", "shift+tab":
		*chosen = max(0, *chosen-1)
	case "j", "down", "tab":
		*chosen = min(len(monitorModes)-1, *chosen+1)
	case "enter":
		return true, monitorModes[*chosen].mode
	}
	return false, ""
}

// monitorQuestion is the overlay: title such as "Add Stromberg", the
// choices, a note when the monitor does not run, and its keys with ok, the
// enter key's action.
func monitorQuestion(title string, chosen int, off, ok string) []string {
	lines := []string{sBold.Render(title), sMuted.Render("Which episodes should Garner get on its own?"), ""}
	width := 0
	for _, m := range monitorModes {
		width = max(width, len(m.label))
	}
	for i, m := range monitorModes {
		row := " " + pad(m.label, width) + "   " + m.means + " "
		if i == chosen {
			lines = append(lines, sNormal.Background(cSelect).Render(row))
		} else {
			lines = append(lines, sNormal.Render(row))
		}
	}
	if off != "" {
		lines = append(lines, "", sMuted.Render("The monitor is "+clean(off)+";"), sMuted.Render("this applies once it runs."))
	}
	return append(lines, "", keyBar("j k", "choose", "enter", ok, "esc", "cancel"))
}
