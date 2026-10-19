// Package tui is Garner's terminal interface. The mockups in the Paper file
// "Garner" are the source of truth; docs/tui-design.md explains them.
package tui

import (
	"context"
	"errors"
	"io/fs"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/client"
	"github.com/trucke/garner/internal/connect"
)

// screen is one full screen. The app owns the terminal size, the session
// and the keys that work everywhere; screens own the rest.
type screen interface {
	init() tea.Cmd
	update(tea.Msg) (screen, tea.Cmd)
	view(width, height int) string
}

// session is one connection to the server, shared by all screens.
type session struct {
	client  *client.Client
	status  client.Status
	events  <-chan client.Event
	stop    context.CancelFunc
	offline bool
	// shows is the library as last loaded, for the activity on every screen.
	shows []api.Show
	// folded remembers each show's folded seasons.
	folded map[int64]map[int]bool
}

// connectedMsg opens the library once Connect has stored a working code.
type connectedMsg struct {
	client *client.Client
	status client.Status
}

// disconnectedMsg sends the user back to Connect, for example after the
// token was revoked.
type disconnectedMsg struct{ reason string }

// switchMsg opens another screen.
type switchMsg struct{ screen screen }

// eventMsg carries one event from a session's stream.
type eventMsg struct {
	client.Event
	from <-chan client.Event
}

var errRevoked = "The stored token no longer works: " + client.ErrUnauthorized.Error() + "."

type app struct {
	path          string
	version       string // of this program
	help          bool   // the Help overlay is open
	helpScroll    scroller
	width, height int
	sess          *session
	screen        screen
	library       *libraryScreen
	stored        *client.Client // a stored connection to open on start
}

// Run opens the interface. path is where the connection code is stored.
func Run(ctx context.Context, path, version string) error {
	a := &app{path: path, version: version}
	code, err := connect.Load(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		a.screen = newConnect(path, "")
	case err != nil:
		a.screen = newConnect(path, "The stored connection can't be read: "+clean(err.Error()))
	default:
		cl, err := client.New(code)
		if err != nil {
			a.screen = newConnect(path, "The stored connection can't be used: "+clean(err.Error()))
		} else {
			a.stored = cl
		}
	}
	_, err = tea.NewProgram(a, tea.WithContext(ctx)).Run()
	if a.sess != nil {
		a.sess.stop()
	}
	if ctx.Err() != nil || errors.Is(err, tea.ErrInterrupted) {
		return nil
	}
	return err
}

func (a *app) Init() tea.Cmd {
	if a.stored != nil {
		return a.open(a.stored, client.Status{})
	}
	return a.screen.init()
}

// open starts a session and shows the library.
func (a *app) open(cl *client.Client, st client.Status) tea.Cmd {
	if a.sess != nil {
		a.sess.stop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.sess = &session{client: cl, status: st, stop: cancel, events: cl.Watch(ctx)}
	a.library = newLibrary(a.sess)
	a.screen = a.library
	return tea.Batch(a.screen.init(), waitEvent(a.sess.events))
}

func waitEvent(ch <-chan client.Event) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return nil
		}
		return eventMsg{e, ch}
	}
}

func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if done, ok := msg.(flashDoneMsg); ok {
		if done.f.seq == done.seq {
			done.f.clear()
		}
		return a, nil
	}
	m, cmd := a.update(msg)
	// A new success or info message on the current screen goes by itself.
	if f, ok := a.screen.(flasher); ok {
		cmd = tea.Batch(cmd, f.flashed().expire())
	}
	return m, cmd
}

func (a *app) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		return a, nil
	case tea.KeyPressMsg:
		key := msg.String()
		if key == "ctrl+c" {
			return a, tea.Quit
		}
		if a.help {
			return a, a.helpKey(key)
		}
		if c, ok := a.screen.(capturer); a.sess == nil || ok && c.captures() {
			break
		}
		switch key {
		case "?":
			a.help, a.helpScroll = true, scroller{}
			return a, nil
		case "A":
			if _, there := a.screen.(*activityScreen); !there {
				a.screen = newActivity(a.sess, a.screen)
				return a, a.screen.init()
			}
			return a, nil
		}
	case connectedMsg:
		return a, a.open(msg.client, msg.status)
	case switchMsg:
		a.screen = msg.screen
		return a, a.screen.init()
	case addedMsg:
		if a.library == nil {
			return a, nil
		}
		message := gDone + " " + sNormal.Render("Added "+msg.name)
		switch {
		case msg.already:
			message = gDone + " " + sNormal.Render(msg.name+" is already in the library")
		case msg.taken != nil && msg.taken.Files > 0:
			message += sNormal.Render(" · took over " + plural(msg.taken.Files, "file") + " from the library")
		}
		show := newShow(a.sess, a.library, msg.id, msg.name, message)
		if worthShowing(msg.taken) {
			show.files.show(*msg.taken)
		}
		a.screen = show
		return a, tea.Batch(a.screen.init(), a.library.showAdded(msg))
	case deletedMsg:
		if a.library == nil {
			return a, nil
		}
		a.screen = a.library
		return a, a.library.showDeleted(msg)
	case disconnectedMsg:
		if a.sess != nil {
			a.sess.stop()
			a.sess = nil
		}
		a.library = nil
		a.screen = newConnect(a.path, msg.reason)
		return a, a.screen.init()
	case eventMsg:
		if a.sess == nil || msg.from != a.sess.events {
			return a, nil // from a closed session
		}
		switch msg.Type {
		case client.EventUnauthorized:
			return a, func() tea.Msg { return disconnectedMsg{errRevoked} }
		case client.EventConnected:
			a.sess.offline = false
		case client.EventDisconnected:
			a.sess.offline = true
		}
		var cmd, libCmd tea.Cmd
		a.screen, cmd = a.screen.update(msg)
		if a.library != nil && a.screen != screen(a.library) {
			// The library keeps the activity on every screen current.
			_, libCmd = a.library.update(msg)
		}
		return a, tea.Batch(cmd, libCmd, waitEvent(a.sess.events))
	case libraryMsg:
		// The library may have been loading when another screen opened,
		// or belong to a session that has ended.
		if a.library == nil || msg.from != a.library {
			return a, nil
		}
		_, cmd := a.library.update(msg)
		return a, cmd
	}
	var cmd tea.Cmd
	a.screen, cmd = a.screen.update(msg)
	return a, cmd
}

func (a *app) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.WindowTitle = "Garner"
	if a.width > 0 && a.height > 0 {
		v.Content = a.screen.view(a.width, a.height)
		if a.help {
			// The box's borders and a line of margin above and below.
			all := helpLines(a.screen, a.width-8)
			lines := a.helpScroll.window(all, a.height-4)
			// As wide as all its lines, so the box keeps its size while it scrolls.
			widest := 0
			for _, l := range all {
				widest = max(widest, lipgloss.Width(l))
			}
			lines[0] = pad(lines[0], widest)
			v.Content = overlay(v.Content, lines, a.width, a.height)
		}
	}
	return v
}

// helpKey handles a key while the Help overlay is open: ? and esc close it,
// a opens About.
func (a *app) helpKey(key string) tea.Cmd {
	if a.helpScroll.key(key) {
		return nil
	}
	switch key {
	case "?", "esc":
		a.help = false
	case "a":
		a.help = false
		a.screen = newAbout(a.sess, a.screen, a.path, a.version)
		return a.screen.init()
	}
	return nil
}

// header renders the first row: the breadcrumb on the left, the activity
// or the offline notice on the right.
func header(width int, sess *session, crumbs ...string) string {
	left := " " + breadcrumb(crumbs...)
	right := activity(sess)
	gap := width - 1 - lipgloss.Width(left) - lipgloss.Width(right)
	if right == "" || gap < 2 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

// activity sums the library's running work: only non-zero counts, nothing
// when all are zero.
func activity(sess *session) string {
	if sess == nil {
		return ""
	}
	if sess.offline {
		return sRed.Render("✗ offline · reconnecting")
	}
	counts := map[string]int{}
	for _, sh := range sess.shows {
		for state, n := range sh.Counts {
			counts[state] += n
		}
	}
	var parts []string
	add := func(n int, glyph, label string, style lipgloss.Style) {
		if n > 0 {
			parts = append(parts, style.Render(glyph+" "+strconv.Itoa(n)+" "+label))
		}
	}
	add(counts[api.EpisodeDownloading]+counts[api.EpisodeProcessing], "◐", "downloading", sAmber)
	add(counts[api.EpisodeImporting], "●", "confirming", sAmber)
	add(counts[api.EpisodeAttention], "▲", "needs you", sAmber)
	add(counts[api.EpisodeFailed], "✗", "failed", sRed)
	return strings.Join(parts, sMuted.Render(" · "))
}
