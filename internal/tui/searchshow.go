package tui

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/trucke/garner/internal/client"
	"github.com/trucke/garner/internal/monitor"
	"github.com/trucke/garner/internal/store"
)

// S on the Show screen searches every missing episode of the show and
// downloads the best release of each that meets its rules. It asks first;
// the server runs the search, so it goes on when the screen is left, and
// the facts line follows it.

// searchShowMsg answers asking for the search.
type searchShowMsg struct {
	from     *showScreen
	episodes int
	err      error
}

// missingCount counts the show's missing episodes, specials left out, and
// those of them whose download was cancelled.
func (s *showScreen) missingCount() (missing, cancelled int) {
	for _, se := range s.seasons() {
		if se.number == 0 {
			continue
		}
		for _, e := range se.episodes {
			switch e.status.State {
			case store.EpisodeNotSearched, store.EpisodeWaiting, store.EpisodeFailed:
				missing++
				if d := e.status.Download; d != nil && d.State == store.Cancelled {
					cancelled++
				}
			}
		}
	}
	return missing, cancelled
}

// askSearch is S: the question, or why there is nothing to ask.
func (s *showScreen) askSearch() {
	switch missing, _ := s.missingCount(); {
	case s.detail == nil:
	case s.detail.Searching != nil:
		s.flash.info(sMuted.Render("Garner is already searching the missing episodes."))
	case missing == 0:
		s.flash.info(sMuted.Render("No episode is missing."))
	default:
		s.asking, s.choice = true, 0 // the safe choice: cancel
	}
}

// searchKey moves the choice in the question or answers it.
func (s *showScreen) searchKey(key string) tea.Cmd {
	switch key {
	case "esc":
		s.asking = false
	case "h", "left", "shift+tab":
		s.choice = 0
	case "l", "right", "tab":
		s.choice = 1
	case "enter":
		s.asking = false
		if s.choice == 1 {
			cl, id := s.sess.client, s.id
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				n, err := cl.SearchShow(ctx, id)
				return searchShowMsg{s, n, err}
			}
		}
	}
	return nil
}

// searchQuestion is the overlay.
func (s *showScreen) searchQuestion() []string {
	missing, cancelled := s.missingCount()
	lines := []string{
		sBold.Render("Search " + plural(missing, "missing episode") + " of " + clean(s.name) + "?"),
		"",
		sNormal.Render("Garner downloads the best release of each that meets the show's rules."),
	}
	if cancelled > 0 {
		lines = append(lines, sNormal.Render("This includes "+plural(cancelled, "episode")+" whose download was cancelled."))
	}
	return append(lines, "",
		"  "+choice("Cancel", s.choice == 0)+"   "+choice("Search", s.choice == 1),
		"",
		keyBar("h l", "choose", "enter", "ok", "esc", "cancel"))
}

// searched handles the answer to asking for the search.
func (s *showScreen) searched(msg searchShowMsg) tea.Cmd {
	switch {
	case errors.Is(msg.err, client.ErrUnauthorized):
		return func() tea.Msg { return disconnectedMsg{errRevoked} }
	case msg.err != nil:
		s.flash.stay(gFailed + " " + sRed.Render("Could not search: "+clean(msg.err.Error())))
	case msg.episodes == 0:
		s.flash.info(sMuted.Render("No episode is missing."))
	}
	return nil
}

// progressed follows the search from its events.
func (s *showScreen) progressed(p monitor.ShowProgress) tea.Cmd {
	if s.detail == nil || p.SeriesID != s.id {
		return nil
	}
	if !p.Finished {
		s.detail.Searching = &p
		return nil
	}
	s.detail.Searching = nil
	text := "Searched " + plural(p.Done, "missing episode") + " · started " + plural(p.Downloads, "download")
	if p.Done < p.Total {
		text = "Stopped after " + plural(p.Done, "episode") + " of " + plural(p.Total, "missing episode") + " · started " + plural(p.Downloads, "download")
	}
	s.flash.info(gDone + " " + sNormal.Render(text))
	return s.reload()
}

// searchFact is the facts line's report of the search, empty when none runs.
func searchFact(p *monitor.ShowProgress) string {
	switch {
	case p == nil:
		return ""
	case p.Waiting:
		return fact("Search", "waiting for the searches before it")
	case p.Total == 0:
		return fact("Search", "starting")
	}
	parts := []string{strconv.Itoa(p.Done) + " of " + strconv.Itoa(p.Total)}
	if p.Downloads > 0 {
		parts = append(parts, plural(p.Downloads, "download"))
	}
	return fact("Search", strings.Join(parts, " · "))
}
