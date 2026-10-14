// Package tui is Garner's terminal interface. The mockups in the Paper file
// "Garner" are the source of truth; docs/tui-design.md explains them.
package tui

import (
	"context"
	"errors"
	"io/fs"

	tea "charm.land/bubbletea/v2"

	"github.com/trucke/garner/internal/client"
	"github.com/trucke/garner/internal/connect"
)

// screen is one full screen. The app owns the terminal size and the keys
// that work everywhere; screens own the rest.
type screen interface {
	init() tea.Cmd
	update(tea.Msg) (screen, tea.Cmd)
	view(width, height int) string
}

// connectedMsg opens the library once Connect has stored a working code.
type connectedMsg struct {
	client *client.Client
	status client.Status
}

// disconnectedMsg sends the user back to Connect, for example after the
// token was revoked.
type disconnectedMsg struct{ reason string }

type app struct {
	path          string
	width, height int
	screen        screen
}

// Run opens the interface. path is where the connection code is stored.
func Run(ctx context.Context, path string) error {
	a := &app{path: path}
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
			a.screen = newLibrary(cl, client.Status{})
		}
	}
	_, err = tea.NewProgram(a, tea.WithContext(ctx)).Run()
	if ctx.Err() != nil || errors.Is(err, tea.ErrInterrupted) {
		return nil
	}
	return err
}

func (a *app) Init() tea.Cmd { return a.screen.init() }

func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		return a, nil
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return a, tea.Quit
		}
	case connectedMsg:
		a.screen = newLibrary(msg.client, msg.status)
		return a, a.screen.init()
	case disconnectedMsg:
		a.screen = newConnect(a.path, msg.reason)
		return a, a.screen.init()
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
	}
	return v
}
