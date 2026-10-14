package tui

import (
	"context"
	"errors"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/trucke/garner/internal/client"
)

// libraryScreen is a placeholder until the Library milestone of GRR-22: it
// confirms the connection and sends the user back to Connect when the token
// stopped working.
type libraryScreen struct {
	client *client.Client
	status client.Status
	err    error
}

func newLibrary(cl *client.Client, st client.Status) *libraryScreen {
	return &libraryScreen{client: cl, status: st}
}

func (s *libraryScreen) init() tea.Cmd {
	if s.status.Version != "" {
		return nil
	}
	return s.load
}

func (s *libraryScreen) load() tea.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := s.client.Status(ctx)
	return statusMsg{st, err}
}

func (s *libraryScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case statusMsg:
		if errors.Is(msg.err, client.ErrUnauthorized) {
			return s, func() tea.Msg {
				return disconnectedMsg{"The stored token no longer works: " + client.ErrUnauthorized.Error() + "."}
			}
		}
		s.status, s.err = msg.status, msg.err
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q":
			return s, tea.Quit
		case "r":
			if s.err != nil {
				s.err = nil
				return s, s.load
			}
		}
	}
	return s, nil
}

func (s *libraryScreen) view(width, height int) string {
	body := breadcrumb("Garner", "Library") + "\n\n"
	keys := keyBar("q", "quit")
	switch {
	case s.err != nil:
		body += "  " + gFailed + " " + sRed.Render(clean(s.err.Error()))
		keys = keyBar("r", "try again", "q", "quit")
	case s.status.Version == "":
		body += "  " + sMuted.Render("Asking "+clean(s.client.Addr())+" …")
	default:
		body += "  " + gDone + " " + sNormal.Render(clean("Connected to Garner "+s.status.Version+" at "+s.client.Addr()+" as token "+s.status.Token+".")) + "\n\n" +
			"  " + sMuted.Render("The library comes in the next step of GRR-22.")
	}
	return screenLayout(width, height, body, keys)
}
