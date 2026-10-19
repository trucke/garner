package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/client"
)

// Taking over a show's files from the library: the overlay after adding a
// show, or after t on Show details, says what Garner found and, when files
// lie outside Garner's folder for the show, asks whether to rename and move
// them. Keeping them as they are is the safe choice and preselected.

type (
	takenOverMsg struct {
		result api.TakeOver
		err    error
	}
	renamedMsg struct {
		result api.Renamed
		err    error
	}
)

// filesPrompt is the overlay.
type filesPrompt struct {
	open   bool
	result api.TakeOver
	choice int // 1: rename and move
}

func (p *filesPrompt) show(t api.TakeOver) { *p = filesPrompt{open: true, result: t} }

// worthShowing reports whether a take-over after adding a show needs more
// than a word in the bottom line: a question, files left alone or problems.
func worthShowing(t *api.TakeOver) bool {
	return t != nil && (t.Renamable > 0 || len(t.Skipped) > 0 || len(t.Notes) > 0)
}

// key answers the overlay; it reports whether renaming was chosen.
func (p *filesPrompt) key(key string) (rename bool) {
	switch key {
	case "esc":
		p.open = false
	case "h", "left", "shift+tab":
		p.choice = 0
	case "l", "right", "tab":
		if p.result.Renamable > 0 {
			p.choice = 1
		}
	case "enter":
		p.open = false
		return p.choice == 1
	}
	return false
}

// maxSkipped is how many files left alone the overlay names.
const maxSkipped = 5

func (p filesPrompt) lines(name string) []string {
	t := p.result
	var lines []string
	if t.Files > 0 {
		lines = append(lines, gDone+" "+sBold.Render("Took over "+plural(t.Files, "file")+" of "+name))
	} else {
		lines = append(lines, sBold.Render("No new files of "+name+" found"))
	}
	lines = append(lines, "")
	if t.Files > 0 && t.Episodes != t.Files {
		lines = append(lines, sMuted.Render("They hold "+plural(t.Episodes, "episode")+"."))
	}
	if len(t.Folders) == 0 {
		lines = append(lines, sMuted.Render("Garner found no folder of the show."))
	}
	for i, f := range t.Folders {
		label := "Looked in "
		if i > 0 {
			label = "      and "
		}
		lines = append(lines, sMuted.Render(label+clean(f)))
	}
	if len(t.Skipped) > 0 {
		lines = append(lines, "", sNormal.Render("Left alone:"))
		for _, s := range t.Skipped[:min(len(t.Skipped), maxSkipped)] {
			lines = append(lines, sMuted.Render(ansi.Truncate("· "+clean(filepath.Base(s.Path))+": "+clean(s.Reason), 100, "…")))
		}
		if n := len(t.Skipped) - maxSkipped; n > 0 {
			lines = append(lines, sMuted.Render(fmt.Sprintf("· and %d more", n)))
		}
	}
	if len(t.Notes) > 0 {
		lines = append(lines, "")
		for _, n := range t.Notes {
			lines = append(lines, sAmber.Render("▲ ")+sNormal.Render(ansi.Truncate(clean(n), 100, "…")))
		}
	}
	if t.Renamable == 0 {
		return append(lines, "", "  "+choice("OK", true), "", keyBar("enter", "ok"))
	}
	return append(lines, "",
		sNormal.Render(plural(t.Renamable, "file")+" of the show lie outside Garner's folder for it."),
		sNormal.Render("Rename and move them there, named the way Garner names files?"),
		"",
		"  "+choice("Keep as they are", p.choice == 0)+"   "+choice("Rename and move", p.choice == 1),
		"",
		keyBar("h l", "choose", "enter", "ok", "esc", "keep"))
}

// takeOverFiles looks for a show's files in the library. FFprobe checks
// every file it finds, which takes a while for a long show on a share.
func takeOverFiles(cl *client.Client, id int64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		t, err := cl.TakeOver(ctx, id)
		return takenOverMsg{t, err}
	}
}

// renameFiles moves a show's taken-over files into Garner's folder.
func renameFiles(cl *client.Client, id int64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		r, err := cl.Rename(ctx, id)
		return renamedMsg{r, err}
	}
}

// reportRenamed says in the bottom line what renaming did.
func reportRenamed(msg renamedMsg, f *flash) {
	switch {
	case msg.err != nil:
		f.stay(gFailed + " " + sRed.Render("Could not rename the files: "+clean(msg.err.Error())))
	case len(msg.result.Notes) > 0:
		f.stay(gDone + " " + sNormal.Render("Moved "+plural(msg.result.Files, "file")+" into Garner's folder; ") +
			sAmber.Render(clean(msg.result.Notes[0])))
	default:
		f.info(gDone + " " + sNormal.Render("Moved "+plural(msg.result.Files, "file")+" into Garner's folder"))
	}
}
