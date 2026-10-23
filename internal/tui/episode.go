package tui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/client"
)

// Episode follows one episode through Garner: its release, the six steps
// from SABnzbd to Jellyfin, and what to fix or what was noted.

type (
	episodeMsg struct {
		from   *episodeScreen
		detail api.EpisodeDetail
		err    error
	}
	cancelledMsg struct {
		download api.Download
		err      error
	}
	retriedMsg struct {
		download api.Download
		err      error
	}
)

type episodeScreen struct {
	sess   *session
	show   *showScreen
	back   screen // where esc goes: the show, or the activity
	id     int64  // TVDB episode ID
	detail *api.EpisodeDetail
	err    error
	// A reload asked for while one runs follows it.
	loading, dirty bool
	asking         bool // the cancel question
	choice         int
	busy           string
	flash          flash
	scroll         scroller
}

func newEpisode(sess *session, show *showScreen, id int64) *episodeScreen {
	return &episodeScreen{sess: sess, show: show, back: show, id: id}
}

// init loads the episode, and its show unless the show screen has already.
func (s *episodeScreen) init() tea.Cmd {
	if s.show.detail == nil {
		return tea.Batch(s.reload(), s.show.reload())
	}
	return s.reload()
}

func (s *episodeScreen) reload() tea.Cmd {
	if s.loading {
		s.dirty = true
		return nil
	}
	s.loading = true
	cl, id := s.sess.client, s.id
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		d, err := cl.Episode(ctx, id)
		return episodeMsg{s, d, err}
	}
}

func (s *episodeScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case episodeMsg:
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
			s.err, s.detail = nil, &msg.detail
		}
		var cmd tea.Cmd
		if s.dirty {
			s.dirty = false
			cmd = s.reload()
		}
		return s, tea.Batch(cmd, s.redrawWhenSlow())
	case cancelledMsg:
		s.busy = ""
		switch {
		case errors.Is(msg.err, client.ErrUnauthorized):
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		case msg.err != nil:
			s.flash.stay(gFailed + " " + sRed.Render("Could not cancel the download: "+clean(msg.err.Error())))
			return s, nil
		}
		if f, ok := s.back.(flasher); ok {
			f.flashed().info(gDone + " " + sNormal.Render("Cancelled the download of "+s.label()))
		}
		return s, func() tea.Msg { return switchMsg{s.back} }
	case retriedMsg:
		s.busy = ""
		switch {
		case errors.Is(msg.err, client.ErrUnauthorized):
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		case msg.err != nil:
			s.flash.stay(gFailed + " " + sRed.Render("Could not retry the import: "+clean(msg.err.Error())))
			return s, nil
		}
		s.flash.info(gWorking + " " + sNormal.Render("Retrying the import"))
		return s, s.apply(msg.download)
	case acknowledgedMsg:
		s.busy = ""
		switch {
		case errors.Is(msg.err, client.ErrUnauthorized):
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		case msg.err != nil:
			s.flash.stay(gFailed + " " + sRed.Render("Could not clear the failure: "+clean(msg.err.Error())))
			return s, nil
		}
		s.flash.info(gDone + " " + sNormal.Render("Failure cleared"))
		return s, tea.Batch(s.apply(msg.download), s.show.reload())
	case eventMsg:
		var cmd tea.Cmd
		switch msg.Type {
		case client.EventConnected:
			cmd = s.reload()
		case "download.updated":
			var d api.Download
			if json.Unmarshal(msg.Data, &d) == nil && d.EpisodeID == s.id {
				cmd = s.apply(d)
			}
		}
		// The show screen keeps the episode states current.
		_, showCmd := s.show.update(msg)
		return s, tea.Batch(cmd, showCmd)
	case seriesMsg:
		// Answers the show screen asked for.
		_, cmd := s.show.update(msg)
		return s, cmd
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

// apply takes a changed download from an event. Progress changes in place;
// a new attempt or a new step loads the episode again, for its file.
func (s *episodeScreen) apply(d api.Download) tea.Cmd {
	if s.detail == nil {
		return s.reload()
	}
	list := s.detail.Downloads
	i := slices.IndexFunc(list, func(x api.Download) bool { return x.ID == d.ID })
	if i < 0 || list[i].State != d.State || list[i].Import.State != d.Import.State {
		if i >= 0 {
			list[i] = d
		}
		return tea.Batch(s.reload(), s.redrawWhenSlow())
	}
	list[i] = d
	return nil
}

func (s *episodeScreen) redrawWhenSlow() tea.Cmd {
	if d, ok := s.latest(); ok {
		return redrawWhenSlow(&d)
	}
	return nil
}

func (s *episodeScreen) key(key string) tea.Cmd {
	if s.scroll.key(key) {
		return nil
	}
	switch key {
	case "esc":
		return func() tea.Msg { return switchMsg{s.back} }
	case "x":
		if _, ok := s.cancellable(); ok {
			s.asking, s.choice = true, 0 // the safe choice: keep going
		}
	case "c":
		if d, ok := s.latest(); ok && d.CanAcknowledge() {
			s.busy = "Clearing the failure…"
			return acknowledgeFailure(s.sess.client, d.ID)
		}
	case "r":
		return s.again()
	}
	return nil
}

// answer handles the cancel question.
func (s *episodeScreen) answer(key string) tea.Cmd {
	switch key {
	case "esc":
		s.asking = false
	case "h", "left", "shift+tab":
		s.choice = 0
	case "l", "right", "tab":
		s.choice = 1
	case "enter":
		s.asking = false
		d, ok := s.cancellable()
		switch {
		case s.choice != 1:
			return nil
		case !ok:
			s.flash.info(gWaiting + " " + sNormal.Render("Too late to cancel: SABnzbd has finished downloading it."))
			return nil
		}
		s.busy = "Cancelling the download…"
		cl := s.sess.client
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			d, err := cl.CancelDownload(ctx, d.ID)
			return cancelledMsg{d, err}
		}
	}
	return nil
}

// Labels for r, by what the episode needs.
const (
	againReleases = "other releases"
	againSearch   = "search again"
	againImport   = "retry import"
)

// againLabel is what r does: look at the last search's other releases, run
// a new search, or retry a failed import. Empty while an import runs.
func (s *episodeScreen) againLabel() string {
	d, ok := s.latest()
	switch {
	case !ok:
		return againSearch
	case d.Import.State == api.ImportFailed, d.Import.State == api.ImportUnverified:
		return againImport
	case d.Active():
		return againReleases
	case d.State == api.Completed && d.Import.State != api.ImportDone:
		return "" // being imported
	case d.State == api.Completed:
		return againSearch // a better release may have come out
	}
	return againReleases
}

func (s *episodeScreen) again() tea.Cmd {
	label := s.againLabel()
	if label == againImport {
		d, _ := s.latest()
		s.busy = "Retrying the import…"
		cl := s.sess.client
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			d, err := cl.RetryImport(ctx, d.ID)
			return retriedMsg{d, err}
		}
	}
	row, ok := s.row()
	if label == "" || !ok {
		return nil
	}
	next := newReleases(s.sess, s.show, row, label == againSearch)
	return func() tea.Msg { return switchMsg{next} }
}

// latest is the episode's newest download.
func (s *episodeScreen) latest() (api.Download, bool) {
	if s.detail == nil || len(s.detail.Downloads) == 0 {
		return api.Download{}, false
	}
	return s.detail.Downloads[0], true
}

// cancellable is the download x cancels: one SABnzbd queues or downloads.
func (s *episodeScreen) cancellable() (api.Download, bool) {
	d, ok := s.latest()
	return d, ok && (d.State == api.Queued || d.State == api.Downloading)
}

// row is the episode as the show screen has it.
func (s *episodeScreen) row() (episodeRow, bool) {
	for _, se := range s.show.seasons() {
		for _, e := range se.episodes {
			if e.episode.ID == s.id {
				return e, true
			}
		}
	}
	return episodeRow{}, false
}

func (s *episodeScreen) label() string {
	if row, ok := s.row(); ok {
		return episodeLabel(row)
	}
	return "the episode"
}

func (s *episodeScreen) view(width, height int) string {
	now := time.Now()
	row, _ := s.row()
	var b strings.Builder
	b.WriteString(header(width, s.sess, "Garner", "Library", clean(s.show.name), s.label()) + "\n\n")
	b.WriteString(factLines(width, episodeFacts(row)...) + "\n")
	switch {
	case s.detail == nil && s.err != nil:
		b.WriteString("\n   " + gFailed + " " + sRed.Render(clean(s.err.Error())))
	case s.detail == nil:
		b.WriteString("\n   " + sMuted.Render("Loading…"))
	default:
		// As wide as the design at most, so times stay near their steps.
		w := min(width, designWidth)
		if d, ok := s.latest(); ok {
			b.WriteString("   " + s.releaseFacts(d, w-6) + "\n\n")
			for _, st := range pipeline(d, now) {
				b.WriteString(st.render(w) + "\n")
			}
		}
		for _, f := range s.frames(row, max(40, w-6), now) {
			b.WriteString("\n" + indent(f, 3) + "\n")
		}
	}
	page := s.scroll.layout(width, height, b.String(), s.bottom())
	if s.asking {
		return overlay(page, s.cancelQuestion(), width, height)
	}
	return page
}

// releaseFacts is the release line: name, tier, size, indexer and which
// attempt this is.
func (s *episodeScreen) releaseFacts(d api.Download, w int) string {
	meta := []string{}
	switch d.Verdict {
	case "accept":
		meta = append(meta, sMuted.Render(clean(d.Tier)))
	case "fallback":
		meta = append(meta, sAmber.Render("last resort"))
	case "reject":
		meta = append(meta, sRed.Render("rejected"))
	}
	if d.Replaces != "" {
		meta = append(meta, sMuted.Render("replaces "+clean(d.Replaces)))
	}
	if sz := size(d.Size); sz != "" {
		meta = append(meta, sMuted.Render(sz))
	}
	meta = append(meta, sMuted.Render(clean(d.Indexer)))
	if !d.Acknowledged.IsZero() {
		meta = append(meta, sSubtle.Render("cleared "+when(d.Acknowledged, time.Now())))
	}
	tail := "      " + strings.Join(meta, sMuted.Render(" · "))
	// Attempts in a row: the downloads that failed or were cancelled just
	// before this one.
	attempt := 1
	for _, e := range s.detail.Downloads[1:] {
		if e.State != api.Failed && e.State != api.Cancelled {
			break
		}
		attempt++
	}
	if attempt > 1 {
		tail += "      " + fact("Attempt", fmt.Sprint(attempt))
	}
	// Narrow: the name keeps the line, the rest moves under it.
	room := w - len("Release  ")
	if ansi.StringWidth(clean(d.Title))+lipgloss.Width(tail) <= room || room-lipgloss.Width(tail) >= 40 {
		return fact("Release", cutMiddle(clean(d.Title), max(20, room-lipgloss.Width(tail)))) + tail
	}
	return fact("Release", cutMiddle(clean(d.Title), room)) + "\n   " + strings.Repeat(" ", len("Release  ")) +
		strings.TrimPrefix(tail, "      ")
}

// frames are the panes under the pipeline, most important first.
func (s *episodeScreen) frames(row episodeRow, w int, now time.Time) []string {
	var out []string
	d, ok := s.latest()
	if ok {
		if details := downloadDetails(d, w); details != "" {
			out = append(out, details)
		}
	}
	if ok && d.Active() && len(d.Reasons) > 0 {
		out = append(out, frame("why this release", reasonLines(d.Verdict, d.Reasons, d.Decisive, w-8), w))
	}
	if f := s.detail.File; f != nil && (row.status.State == api.EpisodeImported || row.status.State == api.EpisodeAttention) {
		dir, name := filepath.Split(clean(f.Path))
		var lines []string
		for _, l := range wrapPath(dir, w-6) {
			lines = append(lines, sMuted.Render(l))
		}
		for _, l := range wrapAfter(name, ".", w-6) {
			lines = append(lines, sNormal.Render(l))
		}
		out = append(out, frame("file", append(lines, sMuted.Render(size(f.Size))), w))
	}
	if ok && !d.Active() && len(d.Import.Notes) > 0 {
		var notes []string
		for _, n := range d.Import.Notes {
			notes = append(notes, sNormal.Render(clean(n)))
		}
		out = append(out, frame("notes", notes, w))
	}
	var earlier []string
	if ok {
		for _, e := range s.detail.Downloads[1:] {
			if len(earlier) == 3*2 {
				break
			}
			glyph, why := gFailed, clean(cmp.Or(e.FailureSummary, e.Failure))
			switch e.State {
			case api.Cancelled:
				glyph, why = sSubtle.Render("✗"), "cancelled"
			case api.Failed:
			default:
				continue // an earlier version that was imported, or one still running
			}
			earlier = append(earlier, glyph+" "+sNormal.Render(clean(e.Title))+sSubtle.Render("  "+when(e.Created, now)),
				"  "+sMuted.Render(why))
		}
	}
	if len(earlier) > 0 {
		out = append(out, frame("earlier attempts", earlier, w))
	}
	if ov := s.detail.Overview; ov != "" {
		out = append(out, frame(overviewLabel(s.detail.OverviewLanguage), []string{sNormal.Render(clean(ov))}, w))
	}
	return out
}

func (s *episodeScreen) cancelQuestion() []string {
	return []string{
		gFailed + " " + sBold.Render("Cancel the download of "+s.label()+"?"),
		"",
		"  " + choice("Keep going", s.choice == 0) + "   " + choice("Cancel download", s.choice == 1),
		"",
		keyBar("h l", "choose", "enter", "ok", "esc", "back"),
	}
}

func (s *episodeScreen) bottom() string {
	switch {
	case s.busy != "":
		return sMuted.Render(s.busy)
	case s.flash.text != "":
		return s.flash.text
	}
	var keys []string
	if _, ok := s.cancellable(); ok {
		keys = append(keys, "x", "cancel")
	}
	if label := s.againLabel(); label != "" && s.detail != nil {
		keys = append(keys, "r", label)
	}
	if d, ok := s.latest(); ok && d.CanAcknowledge() {
		keys = append(keys, "c", "clear failure")
	}
	return keyBar(append(keys, "A", "activity", "?", "help", "esc", "back")...)
}

func (s *episodeScreen) flashed() *flash { return &s.flash }

func (s *episodeScreen) captures() bool { return s.asking || s.busy != "" }

func (s *episodeScreen) help() (string, []string) {
	return "Episode", []string{
		"x", "cancel the download", "r", "other releases, search again or retry the import",
		"c", "clear the latest download failure",
	}
}

// step is one row of the pipeline.
type step struct {
	glyph  string
	name   string
	style  lipgloss.Style // the name's
	detail []part
	at     time.Time
}

const stepNameWidth = len("Moving into the library")

// designWidth is the width the screens are designed for, in cells.
const designWidth = 120

// render draws the step: glyph, name, detail, and its time at the right.
func (st step) render(width int) string {
	at := ""
	if !st.at.IsZero() {
		at = when(st.at, time.Now())
	}
	left := "   " + st.glyph + "   " + st.style.Render(pad(st.name, stepNameWidth)) + "   "
	room := width - 3 - lipgloss.Width(left) - len(at) - 3
	line := left + renderParts(st.detail, room, func(s lipgloss.Style) lipgloss.Style { return s })
	if at == "" {
		return line
	}
	return pad(line, width-3-len(at)) + sSubtle.Render(at)
}

// pipeline lays a download out as the six steps from SABnzbd to Jellyfin.
// Times show where Garner records them: when it was sent, when SABnzbd
// finished, and when the import ended or failed.
func pipeline(d api.Download, now time.Time) []step {
	// Each step's name while pending, running and done.
	names := [6][3]string{
		{"Send to SABnzbd", "Sending to SABnzbd", "Sent to SABnzbd"},
		{"Download", "Downloading", "Downloaded"},
		{"Processing", "Processing", "Processed"},
		{"File check", "File check", "File check"},
		{"Move into the library", "Moving into the library", "Moved into the library"},
		{"Jellyfin confirms", "Jellyfin confirms", "Jellyfin confirms"},
	}
	steps := make([]step, len(names))
	for i := range steps {
		steps[i] = step{glyph: gWaiting, name: names[i][0], style: sSubtle}
	}
	run := func(i int, glyph string, detail ...part) {
		steps[i] = step{glyph: glyph, name: names[i][1], style: sNormal, detail: detail}
	}
	done := func(i int, at time.Time, detail ...part) {
		steps[i] = step{glyph: gDone, name: names[i][2], style: sNormal, detail: detail, at: at}
	}
	fail := func(i int, why string, at time.Time) {
		steps[i] = step{glyph: gFailed, name: names[i][0], style: sNormal, detail: []part{{clean(why), sRed}}, at: at}
	}
	downloading := sAmber.Render("◐")
	im := d.Import

	switch {
	case d.State == api.Submitting:
		run(0, downloading, part{clean(d.Detail), sMuted})
		return steps
	case d.State == api.Failed && d.JobID == "":
		fail(0, d.Failure, d.Updated)
		return steps
	}
	done(0, d.Created)

	switch {
	case d.State == api.Queued, d.State == api.Downloading:
		run(1, downloading, pipelineParts(api.EpisodeDownloading, &d, "", nil, 0, now)...)
		steps[2].detail = []part{{"repair and unpack", sSubtle}}
		return steps
	case d.State == api.Cancelled:
		steps[1] = step{glyph: sSubtle.Render("✗"), name: names[1][0], style: sNormal, detail: []part{{"cancelled", sMuted}}, at: d.Updated}
		return steps
	case d.State == api.Failed && im.State == "":
		// SABnzbd's failure, in the download or its repair and unpack.
		fail(1, cmp.Or(d.FailureSummary, d.Failure), d.Updated)
		return steps
	}
	done(1, time.Time{}, part{size(d.Size), sMuted})

	if d.State == api.Processing {
		run(2, gWorking, part{clean(cmp.Or(d.Detail, "processing")), sMuted})
		return steps
	}
	var finished time.Time
	if d.State == api.Completed {
		finished = d.Updated
	}
	done(2, finished, part{stageNames(d.Stages), sMuted})

	switch {
	case im.State == "", im.State == api.ImportPending:
		run(3, gWorking, part{"checking the file", sMuted})
		return steps
	case im.State == api.ImportRejected:
		// The row names the step; the reason need not.
		fail(3, strings.TrimPrefix(cmp.Or(d.Failure, im.Detail), "file check: "), d.Updated)
		return steps
	case im.State == api.ImportFailed && !planned(im.Files):
		fail(3, im.Detail, im.Updated)
		return steps
	}
	done(3, time.Time{}, checkParts(im.Files)...)

	switch im.State {
	case api.ImportMoving:
		run(4, gWorking, part{"moving the file", sMuted})
		return steps
	case api.ImportFailed:
		fail(4, im.Detail, im.Updated)
		return steps
	}
	target := ""
	if len(im.Files) > 0 {
		target = filepath.Base(clean(im.Files[0].Target))
	}
	done(4, time.Time{}, part{target, sMuted})

	switch im.State {
	case api.ImportScanning:
		wait := "waiting for Jellyfin to list it"
		if now.Sub(d.Updated) >= importSlowAfter {
			wait += " · since " + when(d.Updated, now)
		}
		run(5, gWorking, part{wait, sMuted})
	case api.ImportUnverified:
		steps[5] = step{glyph: sAmber.Render("▲"), name: names[5][2], style: sNormal,
			detail: []part{{clean(im.Detail), sAmber}}, at: im.Updated}
	default:
		done(5, im.Updated, part{clean(im.Detail), sMuted})
	}
	return steps
}

// planned reports whether the file check gave every file its place in the
// library; a failure after that happened while moving.
func planned(files []api.ImportFile) bool {
	return len(files) > 0 && !slices.ContainsFunc(files, func(f api.ImportFile) bool { return f.Target == "" })
}

// stageNames lists SABnzbd's post-processing steps, such as "repair ·
// unpack".
func stageNames(stages []api.Stage) string {
	var names []string
	for _, st := range stages {
		switch st.Name {
		case "Source", "Download", "Servers":
			continue // where it came from, not what was done to it
		}
		names = append(names, strings.ToLower(clean(st.Name)))
	}
	return strings.Join(names, " · ")
}

// checkParts sums up the file check: "German, Japanese audio · 1080p · 24
// min".
func checkParts(files []api.ImportFile) []part {
	if len(files) == 0 || len(files[0].Check) == 0 {
		return nil
	}
	var res api.FileCheck
	if json.Unmarshal(files[0].Check, &res) != nil {
		return nil
	}
	var facts []string
	var audio []string
	for _, a := range res.Audio {
		if a = clean(a); !slices.Contains(audio, a) {
			audio = append(audio, a)
		}
	}
	if len(audio) > 0 {
		facts = append(facts, strings.Join(audio, ", ")+" audio")
	}
	var w, h int
	if _, err := fmt.Sscanf(res.Resolution, "%dx%d", &w, &h); err == nil {
		facts = append(facts, fmt.Sprintf("%dp", api.ResolutionClass(w, h)))
	}
	if d, err := time.ParseDuration(res.Duration); err == nil {
		facts = append(facts, fmt.Sprintf("%d min", int(d.Round(time.Minute).Minutes())))
	}
	if len(files) > 1 {
		facts = append(facts, plural(len(files), "file"))
	}
	return []part{{strings.Join(facts, " · "), sMuted}}
}
