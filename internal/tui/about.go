package tui

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/client"
	"github.com/trucke/garner/internal/server"
)

// About shows the versions, the connection, how the services the server
// uses are doing, and what the library holds, with TVDB's attribution.
// Board "About" in the Paper file.

const sourceURL = "https://github.com/trucke/garner"

type (
	aboutStatusMsg struct {
		from   *aboutScreen
		status client.Status
		err    error
	}
	aboutServicesMsg struct {
		from     *aboutScreen
		services server.Services
		err      error
	}
)

type aboutScreen struct {
	sess     *session
	back     screen
	path     string // the connection file
	version  string // of this program
	status   *client.Status
	services *server.Services
	err      error // of the services check
	flash    flash
	scroll   scroller
}

func newAbout(sess *session, back screen, path, version string) *aboutScreen {
	return &aboutScreen{sess: sess, back: back, path: path, version: version}
}

// init asks for the server's status and has it check its services; the
// checks take a moment, so the rest shows first.
func (s *aboutScreen) init() tea.Cmd {
	cl := s.sess.client
	return tea.Batch(
		func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			st, err := cl.Status(ctx)
			return aboutStatusMsg{s, st, err}
		},
		func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			sv, err := cl.Services(ctx)
			return aboutServicesMsg{s, sv, err}
		},
	)
}

func (s *aboutScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case aboutStatusMsg:
		if msg.from != s {
			return s, nil
		}
		if errors.Is(msg.err, client.ErrUnauthorized) {
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		}
		if msg.err == nil {
			s.status = &msg.status
		}
	case aboutServicesMsg:
		if msg.from != s {
			return s, nil
		}
		if errors.Is(msg.err, client.ErrUnauthorized) {
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		}
		s.services, s.err = &msg.services, msg.err
	case openedMsg:
		if msg.err != nil {
			s.flash.stay(gFailed + " " + sRed.Render("Could not open the browser: "+clean(msg.err.Error())))
		}
	case tea.KeyPressMsg:
		s.flash.clear()
		if s.scroll.key(msg.String()) {
			return s, nil
		}
		switch msg.String() {
		case "esc":
			return s, func() tea.Msg { return switchMsg{s.back} }
		case "o":
			return s, browse(sourceURL)
		}
	}
	return s, nil
}

func (s *aboutScreen) view(width, height int) string {
	w := min(width, designWidth) - 6
	var b strings.Builder
	b.WriteString(header(width, s.sess, "Garner", "About") + "\n\n")
	b.WriteString("   " + sBold.Render("Garner") + "\n")
	b.WriteString("   " + sMuted.Render("TV and anime manager") + "\n\n")

	// A field's details move to a line of their own when they do not fit.
	field := func(label, value, rest string) {
		line := "   " + sSubtle.Render(fmt.Sprintf("%-12s", label)) + sNormal.Render(value)
		if lipgloss.Width(line+rest) <= w+6 {
			b.WriteString(line + sMuted.Render(rest) + "\n")
			return
		}
		b.WriteString(line + "\n")
		if rest = strings.TrimPrefix(rest, " · "); rest != "" {
			b.WriteString("   " + strings.Repeat(" ", 12) + sMuted.Render(rest) + "\n")
		}
	}
	field("TUI", clean(s.version), " · "+platform())
	server := sMuted.Render("asking…")
	if st := s.status; st != nil {
		up := ""
		if !st.Started.IsZero() {
			up = " · up " + uptime(time.Since(st.Started))
		}
		field("Server", clean(st.Version), " · "+clean(s.sess.client.Addr())+up)
		created := ""
		if !st.TokenCreated.IsZero() {
			created = " · created " + st.TokenCreated.Local().Format("2 Jan 2006")
		}
		field("Token", clean(st.Token), created)
	} else {
		b.WriteString("   " + sSubtle.Render(fmt.Sprintf("%-12s", "Server")) + server + "\n")
	}
	for i, l := range wrapPath(clean(homeShort(s.path)), w-12) {
		label := "Connection"
		if i > 0 {
			label = ""
		}
		field(label, l, "")
	}
	b.WriteString("\n")

	switch {
	case s.services == nil:
		b.WriteString(indent(frame("services", []string{gWorking + " " + sMuted.Render("Checking the services…")}, w), 3) + "\n")
	case s.err != nil:
		b.WriteString(indent(frame("services", []string{gFailed + " " + sRed.Render(clean(s.err.Error()))}, w), 3) + "\n")
	default:
		var lines []string
		for _, c := range s.services.Services {
			glyph, detail := gDone, sMuted.Render(clean(c.Detail))
			if !c.OK {
				glyph, detail = gFailed, sRed.Render(clean(c.Detail))
			}
			if c.At != nil {
				detail += sMuted.Render(" " + ago(*c.At))
			}
			lines = append(lines, glyph+" "+pad(sNormal.Render(clean(c.Name)), 10)+detail)
		}
		b.WriteString(indent(frame("services", lines, w), 3) + "\n\n")
		l := s.services.Library
		shows := fmt.Sprint(l.Shows)
		if n := l.Profiles["tv"] + l.Profiles["anime"]; n > 0 {
			shows += fmt.Sprintf(" · %d TV · %d anime", l.Profiles["tv"], l.Profiles["anime"])
		}
		episodes := fmt.Sprintf("%d of %d", l.Imported, l.Aired)
		if sz := size(l.Bytes); sz != "" {
			episodes += " · " + sz
		}
		row := func(label, value string) string { return pad(sSubtle.Render(label), 10) + sNormal.Render(value) }
		rows := []string{row("Shows", shows), row("Episodes", episodes)}
		// One folder a line, broken only after a slash.
		label := "Folders"
		for _, f := range l.Folders {
			for _, part := range wrapPath(clean(f), w-6-10) {
				rows, label = append(rows, row(label, part)), ""
			}
		}
		b.WriteString(indent(frame("library", rows, w), 3) + "\n")
	}

	b.WriteString("\n   " + sSubtle.Render(fmt.Sprintf("%-12s", "Source")) + sNormal.Render(sourceURL) + "\n")
	// TheTVDB's terms ask for this attribution wherever its data shows.
	credit := "Metadata provided by TheTVDB (https://thetvdb.com). Please consider adding missing information or subscribing."
	for _, l := range strings.Split(ansi.Wrap(credit, w, ""), "\n") {
		b.WriteString("   " + sMuted.Render(l) + "\n")
	}
	return s.scroll.layout(width, height, b.String(), s.bottom())
}

func (s *aboutScreen) bottom() string {
	if s.flash.text != "" {
		return s.flash.text
	}
	return keyBar("o", "open on GitHub", "esc", "back")
}

func (s *aboutScreen) flashed() *flash { return &s.flash }

func (s *aboutScreen) help() (string, []string) {
	return "About", []string{"o", "open the source on GitHub"}
}

// platform names the system this program runs on, as in "macOS arm64".
func platform() string {
	name := map[string]string{"darwin": "macOS", "linux": "Linux", "windows": "Windows"}[runtime.GOOS]
	if name == "" {
		name = runtime.GOOS
	}
	return name + " " + runtime.GOARCH
}

// uptime writes a duration roughly: "12 minutes", "3 hours", "6 days".
func uptime(d time.Duration) string {
	switch {
	case d < time.Hour:
		return plural(max(1, int(d.Minutes())), "minute")
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour")
	}
	return plural(int(d.Hours()/24), "day")
}
