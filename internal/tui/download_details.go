package tui

import (
	"strings"

	"github.com/trucke/garner/internal/api"
)

// downloadDetails keeps the original failure and its evidence on the
// episode details screen. The reason comes from the server; the interface
// does not infer a cause from an unfamiliar error.
func downloadDetails(d api.Download, width int) string {
	state := downloadState(d)
	if state != api.EpisodeFailed && state != api.EpisodeAttention {
		return ""
	}
	var lines []string
	if d.FailureSummary != "" {
		lines = append(lines, sRed.Render(clean(d.FailureSummary)))
	}
	if d.Failure != "" {
		lines = append(lines, sRed.Render(clean(d.Failure)))
	}
	if d.Import.Detail != "" && d.Import.Detail != d.Failure {
		lines = append(lines, sNormal.Render(clean(d.Import.Detail)))
	}
	if d.Import.Recovery != "" {
		lines = append(lines, "", sNormal.Render(clean(d.Import.Recovery)))
	}
	for _, stage := range d.Stages {
		if len(stage.Actions) == 0 {
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, sBold.Render(clean(stage.Name)))
		for _, action := range stage.Actions {
			for _, line := range strings.Split(action, "\n") {
				lines = append(lines, sMuted.Render(clean(line)))
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return frame("details", lines, width)
}
