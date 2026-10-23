package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/client"
)

type acknowledgedMsg struct {
	download api.Download
	err      error
}

func acknowledgeFailure(cl *client.Client, id int64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		d, err := cl.AcknowledgeDownload(ctx, id)
		return acknowledgedMsg{d, err}
	}
}
