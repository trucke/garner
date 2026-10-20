package tui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/client"
)

// Activity lists what runs, what waits for the user, and what ended within
// the last day: one row per episode, its latest download.

type activityMsg struct {
	from  *activityScreen
	items []api.ActivityItem
	err   error
}

type activityScreen struct {
	sess  *session
	back  screen
	items []api.ActivityItem
	err   error
	// A reload asked for while one runs follows it.
	loading, dirty bool
	selected       int64 // episode ID
	offset         int
	asking         bool // the cancel question
	choice         int
	busy           string
	flash          flash
}

func newActivity(sess *session, back screen) *activityScreen {
	return &activityScreen{sess: sess, back: back}
}

func (s *activityScreen) init() tea.Cmd { return s.reload() }

func (s *activityScreen) reload() tea.Cmd {
	if s.loading {
		s.dirty = true
		return nil
	}
	s.loading = true
	cl := s.sess.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		items, err := cl.Activity(ctx)
		return activityMsg{s, items, err}
	}
}

func (s *activityScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case activityMsg:
		if msg.from != s {
			return s, nil
		}
		s.loading = false
		switch {
		case errors.Is(msg.err, client.ErrUnauthorized):
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		case msg.err != nil:
			s.err = msg.err
		default:
			s.err, s.items = nil, msg.items
			if rows := s.rows(); len(rows) > 0 && !slices.ContainsFunc(rows, func(it api.ActivityItem) bool { return it.Download.EpisodeID == s.selected }) {
				s.selected, s.offset = rows[0].Download.EpisodeID, 0
			}
		}
		var cmd tea.Cmd
		if s.dirty {
			s.dirty = false
			cmd = s.reload()
		}
		var waiting []*api.Download
		for i := range s.items {
			waiting = append(waiting, &s.items[i].Download)
		}
		return s, tea.Batch(cmd, redrawWhenSlow(waiting...))
	case cancelledMsg:
		s.busy = ""
		switch {
		case errors.Is(msg.err, client.ErrUnauthorized):
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		case msg.err != nil:
			s.flash.stay(gFailed + " " + sRed.Render("Could not cancel the download: "+clean(msg.err.Error())))
			return s, nil
		}
		s.flash.info(gDone + " " + sNormal.Render("Cancelled the download"))
		return s, s.reload()
	case eventMsg:
		switch msg.Type {
		case client.EventConnected, "series.deleted":
			return s, s.reload()
		case "download.updated":
			var d api.Download
			if json.Unmarshal(msg.Data, &d) != nil {
				return s, s.reload()
			}
			return s, s.apply(d)
		}
	case tea.KeyPressMsg:
		if s.busy != "" {
			return s, nil
		}
		s.flash.clear()
		if s.asking {
			return s, s.answer(msg.String())
		}
		return s, s.key(msg.String())
	}
	return s, nil
}

// apply takes a changed download from an event: progress changes in
// place; anything else loads the list again, which may add or drop a row.
func (s *activityScreen) apply(d api.Download) tea.Cmd {
	i := slices.IndexFunc(s.items, func(it api.ActivityItem) bool { return it.Download.ID == d.ID })
	if i < 0 || s.items[i].Download.State != d.State || s.items[i].Download.Import.State != d.Import.State {
		return s.reload()
	}
	s.items[i].Download = d
	return nil
}

func (s *activityScreen) key(key string) tea.Cmd {
	switch key {
	case "esc":
		return func() tea.Msg { return switchMsg{s.back} }
	case "j", "down":
		s.move(1)
	case "k", "up":
		s.move(-1)
	case "g", "home":
		s.move(-1 << 30)
	case "G", "end":
		s.move(1 << 30)
	case "ctrl+d", "pgdown":
		s.move(10)
	case "ctrl+u", "pgup":
		s.move(-10)
	case "enter":
		it, ok := s.cursor()
		if !ok {
			return nil
		}
		// The episode screen needs its show; esc on either comes back here.
		show := newShow(s.sess, s, it.SeriesID, itemShow(it), "")
		next := newEpisode(s.sess, show, it.Download.EpisodeID)
		next.back = s
		return func() tea.Msg { return switchMsg{next} }
	case "x":
		if it, ok := s.cursor(); ok && cancellable(it.Download) {
			s.asking, s.choice = true, 0 // the safe choice: keep going
		}
	}
	return nil
}

// answer handles the cancel question.
func (s *activityScreen) answer(key string) tea.Cmd {
	switch key {
	case "esc":
		s.asking = false
	case "h", "left", "shift+tab":
		s.choice = 0
	case "l", "right", "tab":
		s.choice = 1
	case "enter":
		s.asking = false
		it, ok := s.cursor()
		switch {
		case s.choice != 1 || !ok:
			return nil
		case !cancellable(it.Download):
			s.flash.info(gWaiting + " " + sNormal.Render("Too late to cancel: SABnzbd has finished downloading it."))
			return nil
		}
		s.busy = "Cancelling the download…"
		cl, id := s.sess.client, it.Download.ID
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			d, err := cl.CancelDownload(ctx, id)
			return cancelledMsg{d, err}
		}
	}
	return nil
}

// cancellable reports whether x can stop the download: SABnzbd queues or
// downloads it.
func cancellable(d api.Download) bool {
	return d.State == api.Queued || d.State == api.Downloading
}

// activityRank orders the rows: what needs the user first, then what runs,
// furthest along first, then what ended.
var activityRank = map[string]int{
	api.EpisodeFailed: 0, api.EpisodeAttention: 1, api.EpisodeImporting: 2, api.EpisodeProcessing: 3,
	api.EpisodeDownloading: 4, api.EpisodeImported: 5, api.Cancelled: 5,
}

// rows are the items in display order: by state, then the newest first.
func (s *activityScreen) rows() []api.ActivityItem {
	rows := slices.Clone(s.items)
	slices.SortStableFunc(rows, func(a, b api.ActivityItem) int {
		return cmp.Or(cmp.Compare(activityRank[downloadState(a.Download)], activityRank[downloadState(b.Download)]),
			since(b.Download).Compare(since(a.Download)))
	})
	return rows
}

func (s *activityScreen) cursor() (api.ActivityItem, bool) {
	rows := s.rows()
	i := slices.IndexFunc(rows, func(it api.ActivityItem) bool { return it.Download.EpisodeID == s.selected })
	if i < 0 {
		return api.ActivityItem{}, false
	}
	return rows[i], true
}

func (s *activityScreen) move(n int) {
	rows := s.rows()
	if len(rows) == 0 {
		return
	}
	i := max(0, slices.IndexFunc(rows, func(it api.ActivityItem) bool { return it.Download.EpisodeID == s.selected }))
	s.selected = rows[min(len(rows)-1, max(0, i+n))].Download.EpisodeID
}

// downloadState is the state a download puts its episode in, as the
// library names them; a cancelled one is "cancelled".
func downloadState(d api.Download) string {
	switch {
	case d.State == api.Cancelled:
		return api.Cancelled
	case d.State == api.Processing:
		return api.EpisodeProcessing
	case d.Active():
		return api.EpisodeDownloading
	case d.State == api.Completed && (d.Import.State == "" || d.Import.State == api.ImportPending ||
		d.Import.State == api.ImportMoving || d.Import.State == api.ImportScanning):
		return api.EpisodeImporting
	case d.Import.State == api.ImportUnverified:
		return api.EpisodeAttention
	case d.Import.State == api.ImportDone:
		return api.EpisodeImported
	}
	return api.EpisodeFailed
}

// since is when the row's state began: when the download started while it
// runs, otherwise when it last changed.
func since(d api.Download) time.Time {
	if d.Active() {
		return d.Created
	}
	if d.Import.Updated.After(d.Updated) {
		return d.Import.Updated
	}
	return d.Updated
}

func itemShow(it api.ActivityItem) string { return clean(cmp.Or(it.SeriesNameEng, it.SeriesName)) }

func itemEpisode(it api.ActivityItem) string {
	return fmt.Sprintf("S%02dE%02d", it.Season, it.Number)
}

const (
	activityTop        = 4 // rows above the list: header, blank, column names, rule
	activityShowWidth  = 34
	activityEpWidth    = len("Episode")
	activitySinceWidth = len("16 Oct 14:02")
)

func (s *activityScreen) view(width, height int) string {
	var b strings.Builder
	b.WriteString(header(width, s.sess, "Garner", "Activity") + "\n\n")
	// Wide: State takes the room, as on the other tables. Narrow: the show
	// names give way to State, down to a minimum.
	fixed := 3 + 1 + 3 + 3 + activityEpWidth + 3 + 3 + activitySinceWidth + 3
	showWidth := max(16, min(activityShowWidth, width-fixed-24))
	stateWidth := max(10, width-fixed-showWidth)
	head := func(t string, n int, right bool) string { return sSubtle.Render(fit(t, n, right)) }
	b.WriteString("       " + head("Show", showWidth, false) + "   " + head("Episode", activityEpWidth, false) + "   " +
		head("State", stateWidth, false) + "   " + head("Since", activitySinceWidth, true) + "\n")
	b.WriteString("   " + sBorder.Render(strings.Repeat("─", max(0, width-6))) + "\n")

	rows := s.rows()
	switch {
	case s.err != nil:
		b.WriteString("\n   " + gFailed + " " + sRed.Render(clean(s.err.Error())))
	case s.items == nil:
		b.WriteString("\n   " + sMuted.Render("Loading…"))
	case len(rows) == 0:
		b.WriteString("\n   " + sMuted.Render("Nothing running. Finished downloads stay here for a day."))
	default:
		page := max(3, height-activityTop-1)
		cursor := max(0, slices.IndexFunc(rows, func(it api.ActivityItem) bool { return it.Download.EpisodeID == s.selected }))
		s.offset = min(max(s.offset, cursor-page+1), cursor)
		now := time.Now()
		for _, it := range rows[s.offset:min(len(rows), s.offset+page)] {
			b.WriteString(activityRow(it, it.Download.EpisodeID == s.selected, showWidth, stateWidth, width, now) + "\n")
		}
	}
	page := screenLayout(width, height, b.String(), s.bottom())
	if s.asking {
		if it, ok := s.cursor(); ok {
			return overlay(page, []string{
				gFailed + " " + sBold.Render("Cancel the download of "+itemShow(it)+" "+itemEpisode(it)+"?"),
				"",
				"  " + choice("Keep going", s.choice == 0) + "   " + choice("Cancel download", s.choice == 1),
				"",
				keyBar("h l", "choose", "enter", "ok", "esc", "back"),
			}, width, height)
		}
	}
	return page
}

func activityRow(it api.ActivityItem, selected bool, showWidth, stateWidth, width int, now time.Time) string {
	bg := selectBg(selected)
	d := it.Download
	state := downloadState(d)
	glyph, glyphStyle := episodeGlyph(state)
	text := sNormal
	var parts []part
	switch state {
	case api.EpisodeImported:
		// Done: nothing to do, so subtle, as on the show screen.
		text = sSubtle
		parts = []part{{strings.TrimPrefix(clean(d.Tier)+" · "+size(d.Size), " · "), sSubtle}}
		if d.Replaces != "" {
			parts = append(parts, part{" · replaced " + clean(d.Replaces), sSubtle})
		}
	case api.Cancelled:
		glyph, glyphStyle, text = "✗", sSubtle, sSubtle
		parts = []part{{"cancelled", sSubtle}}
	default:
		parts = statusParts(state, &d, "", nil, 0, now)
	}
	gap := bg(lipgloss.NewStyle()).Render("   ")
	line := gap + bg(glyphStyle).Render(glyph) + gap +
		bg(text).Render(fit(itemShow(it), showWidth, false)) + gap +
		bg(text).Render(fit(itemEpisode(it), activityEpWidth, false)) + gap +
		renderParts(parts, stateWidth, bg)
	line = fill(line, width-3-activitySinceWidth, bg) + bg(sSubtle).Render(fit(when(since(d), now), activitySinceWidth, true))
	return fill(line, width-3, bg)
}

func (s *activityScreen) bottom() string {
	switch {
	case s.busy != "":
		return sMuted.Render(s.busy)
	case s.flash.text != "":
		return s.flash.text
	}
	keys := []string{}
	if len(s.items) > 0 {
		keys = append(keys, "j k", "move", "enter", "episode")
	}
	if it, ok := s.cursor(); ok && cancellable(it.Download) {
		keys = append(keys, "x", "cancel")
	}
	return keyBar(append(keys, "?", "help", "esc", "back")...)
}

func (s *activityScreen) flashed() *flash { return &s.flash }

func (s *activityScreen) captures() bool { return s.asking || s.busy != "" }

func (s *activityScreen) help() (string, []string) {
	return "Activity", []string{"enter", "open the episode", "x", "cancel the download"}
}
