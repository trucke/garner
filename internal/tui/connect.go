package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/client"
	"github.com/trucke/garner/internal/connect"
)

// Connect takes a connection code, checks the server in three steps and
// stores the code. Boards "Connect · …" in the Paper file.

type checkState int

const (
	pending checkState = iota
	running
	passed
	failed
)

type check struct {
	label   string
	state   checkState
	detail  string   // muted, after the label once passed
	problem string   // red, after the label once failed
	pin     string   // the server's pin when it did not match
	help    []string // muted lines below a failure
}

type connectPhase int

const (
	editing connectPhase = iota
	connecting
	stopped // a check failed
	connected
)

type connectScreen struct {
	path   string
	notice string // why Connect opened instead of the library, if not first start
	input  textinput.Model
	code   connect.Code
	bad    error // why the input is not a connection code
	phase  connectPhase
	retry  bool // the failure may pass on its own, so enter tries again
	checks []check
	client *client.Client
	status client.Status
}

type (
	checkedMsg struct{ err error }
	statusMsg  struct {
		status client.Status
		err    error
	}
	savedMsg struct{ err error }
	openMsg  struct{}
)

func newConnect(path, notice string) *connectScreen {
	in := textinput.New()
	in.Prompt = "› "
	in.EchoMode = textinput.EchoPassword
	in.EchoCharacter = '•'
	st := textinput.DefaultStyles(true)
	st.Cursor.Blink = false
	st.Cursor.Color = cFg
	st.Focused.Prompt = sSubtle
	st.Focused.Text = sNormal
	in.SetStyles(st)
	return &connectScreen{path: path, notice: notice, input: in}
}

func (s *connectScreen) init() tea.Cmd { return s.input.Focus() }

func (s *connectScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "enter":
			if s.phase == editing && s.bad == nil && s.input.Value() != "" || s.phase == stopped && s.retry {
				return s, s.start()
			}
			return s, nil
		case "ctrl+u":
			if s.phase == editing || s.phase == stopped {
				s.input.Reset()
				s.edited()
			}
			return s, nil
		}
	case checkedMsg:
		if msg.err != nil {
			s.fail(0, msg.err)
			return s, nil
		}
		s.checks[0].state = passed
		if s.code.Pin == "" && strings.HasPrefix(s.code.URL, "http:") {
			s.checks[0].detail = "plain HTTP, this machine only"
		}
		s.checks[1].state = running
		cl := s.client
		return s, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			st, err := cl.Status(ctx)
			return statusMsg{st, err}
		}
	case statusMsg:
		if msg.err != nil {
			s.fail(1, msg.err)
			return s, nil
		}
		s.status = msg.status
		s.checks[1].state = passed
		s.checks[1].detail = clean("Garner " + msg.status.Version + " · token " + msg.status.Token)
		s.checks[2].state = running
		path, code := s.path, s.code
		return s, func() tea.Msg { return savedMsg{connect.Save(path, code)} }
	case savedMsg:
		if msg.err != nil {
			s.checks[2].state = failed
			s.checks[2].problem = "could not store the token"
			s.checks[2].help = []string{clean(msg.err.Error())}
			s.phase = stopped
			s.retry = true
			return s, nil
		}
		s.checks[2].state = passed
		s.checks[2].detail = homeShort(s.path)
		s.phase = connected
		return s, tea.Tick(800*time.Millisecond, func(time.Time) tea.Msg { return openMsg{} })
	case openMsg:
		cl, st := s.client, s.status
		return s, func() tea.Msg { return connectedMsg{cl, st} }
	}

	if s.phase == editing || s.phase == stopped {
		before := s.input.Value()
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		if s.input.Value() != before {
			s.edited()
		}
		return s, cmd
	}
	return s, nil
}

// edited re-reads the input and drops the result of an earlier attempt.
func (s *connectScreen) edited() {
	s.phase, s.retry, s.checks = editing, false, nil
	s.code, s.bad = connect.Code{}, nil
	if v := s.input.Value(); v != "" {
		s.code, s.bad = connect.Parse(v)
	}
}

func (s *connectScreen) start() tea.Cmd {
	cl, err := client.New(s.code)
	if err != nil {
		s.bad = err
		return nil
	}
	first := "checking certificate"
	if strings.HasPrefix(s.code.URL, "http:") {
		first = "checking the address"
	}
	s.client, s.phase, s.retry = cl, connecting, false
	s.checks = []check{{label: first, state: running}, {label: "asking the server"}, {label: "storing the token"}}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return checkedMsg{cl.Check(ctx)}
	}
}

func (s *connectScreen) fail(i int, err error) {
	c := &s.checks[i]
	c.state = failed
	s.phase = stopped
	var unreachable *client.UnreachableError
	var untrusted *client.UntrustedError
	var server *client.ServerError
	switch {
	case errors.As(err, &unreachable):
		c.problem = clean(unreachable.Error())
		c.help = []string{"Check that the server is running and this machine can reach it."}
		s.retry = true
	case errors.As(err, &untrusted) && untrusted.ServerPin != "":
		c.problem = untrusted.Error()
		c.pin = untrusted.ServerPin
		c.help = []string{"Garner did not send the token. Create a new code on the server.", "If the certificate did not change, do not connect from this network."}
	case errors.Is(err, client.ErrNotLoopback):
		c.problem = err.Error()
		c.help = []string{"Garner did not send the token. For another machine, use HTTPS or an SSH tunnel."}
	case errors.As(err, &untrusted):
		c.problem = clean(untrusted.Error())
		c.help = []string{"Garner did not send the token."}
	case errors.Is(err, client.ErrUnauthorized):
		c.problem = err.Error()
		c.help = []string{"It may have been revoked. Create a new code on the server."}
	case errors.As(err, &server):
		c.problem = clean(server.Message)
		s.retry = true
	default:
		c.problem = clean(err.Error())
		s.retry = true
	}
}

const connectWidth = 78

func (s *connectScreen) view(width, height int) string {
	w := min(connectWidth, width-4)
	var b strings.Builder
	title := "first start"
	if s.notice != "" {
		title = "connect again"
	}
	b.WriteString(sBold.Render("Garner") + "  " + sSubtle.Render(title) + "\n\n")
	if s.notice != "" {
		b.WriteString(gFailed + " " + sNormal.Render(s.notice) + "\n\n")
	}

	s.input.SetWidth(max(10, w-6-6-2))
	b.WriteString(frame("connect", []string{
		sBold.Render("1") + "  " + sNormal.Render("On the server, create a connection code for this machine:"),
		"",
		"     " + sCommand.Render(" garnerd token create "+tokenName()+" "),
		"",
		sBold.Render("2") + "  " + sNormal.Render("Paste the code it prints here:"),
		"",
		"   " + s.input.View(),
	}, w))
	b.WriteString("\n\n")

	switch {
	case s.input.Value() == "":
	case s.bad != nil:
		b.WriteString(gFailed + " " + sRed.Render(s.bad.Error()) + "\n")
	default:
		b.WriteString(frame("code", s.codeLines(), w) + "\n\n")
		if s.phase == editing {
			b.WriteString(sMuted.Render("Press ") + sKey.Render("enter") + sMuted.Render(" to connect.") + "\n")
		}
	}
	if s.checks != nil {
		b.WriteString(frame("checks", s.checkLines(w-6), w) + "\n\n")
	}
	if s.phase == connected {
		b.WriteString(gDone + " " + sNormal.Render("Connected. Opening the library.") + "\n")
	}

	top := 1
	if height >= 30 {
		top = 4
	}
	body := strings.Repeat("\n", top) + indent(strings.TrimRight(b.String(), "\n"), max(0, (width-w)/2))
	return screenLayout(width, height, body, s.keys())
}

func (s *connectScreen) codeLines() []string {
	label := func(l string) string { return sSubtle.Render(pad(l, 10)) }
	pin := sMuted.Render(shortPin(s.code.Pin))
	switch {
	case strings.HasPrefix(s.code.URL, "http:"):
		pin = sMuted.Render("none · plain HTTP")
	case s.code.Pin == "":
		pin = sMuted.Render("none · checked against the system's certificate authorities")
	}
	host := s.code.URL
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	return []string{
		label("Server") + sNormal.Render(clean(host)),
		label("Pin") + pin,
		label("Token") + sMuted.Render("•••• ends in "+s.code.Token[len(s.code.Token)-4:]),
	}
}

func (s *connectScreen) checkLines(inner int) []string {
	const labelWidth = 23 // longest label plus three spaces
	var lines []string
	for _, c := range s.checks {
		glyph := map[checkState]string{pending: gWaiting, running: gWorking, passed: gDone, failed: gFailed}[c.state]
		head := glyph + " " + pad(sNormal.Render(c.label), labelWidth)
		text, style := c.detail, sMuted
		if c.problem != "" {
			text, style = c.problem, sRed
		}
		// Wrap long text under itself rather than under the label.
		for i, l := range strings.Split(ansi.Wrap(text, max(10, inner-2-labelWidth), ""), "\n") {
			if i > 0 {
				head = strings.Repeat(" ", 2+labelWidth)
			}
			lines = append(lines, head+style.Render(l))
		}
		if c.pin != "" {
			lines = append(lines, "    "+sSubtle.Render(pad("Server", 10))+sRed.Render(shortPin(c.pin)))
		}
		for _, h := range c.help {
			for _, l := range strings.Split(ansi.Wrap(h, max(10, inner-4), ""), "\n") {
				lines = append(lines, "    "+sMuted.Render(l))
			}
		}
	}
	return lines
}

func (s *connectScreen) keys() string {
	switch {
	case s.phase == editing && s.bad == nil && s.input.Value() != "":
		return keyBar("enter", "connect", "ctrl+u", "clear", "ctrl+c", "quit")
	case s.phase == editing && s.input.Value() != "":
		return keyBar("ctrl+u", "clear", "ctrl+c", "quit")
	case s.phase == stopped && s.retry:
		return keyBar("enter", "try again", "ctrl+u", "clear", "ctrl+c", "quit")
	case s.phase == stopped:
		return keyBar("ctrl+u", "clear", "ctrl+c", "quit")
	}
	return keyBar("ctrl+c", "quit")
}

// tokenName suggests a token name from this machine's name.
func tokenName() string {
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(strings.ToLower(host), ".")
	name := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			return r
		}
		return -1
	}, host)
	if name == "" {
		return "laptop"
	}
	return name
}

// shortPin shows the start and end of a pin, enough to compare by eye.
func shortPin(pin string) string {
	if len(pin) <= 16 {
		return "sha256/" + pin
	}
	return "sha256/" + pin[:10] + "…" + pin[len(pin)-4:]
}

func homeShort(path string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return path
}

// captures: the code field takes every key.
func (s *connectScreen) captures() bool { return true }
