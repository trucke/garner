package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/client"
	"github.com/trucke/garner/internal/profile"
)

// Import takes a profile's text, pasted or fetched from a link the user
// types, checks it with the same check the server runs, and shows what
// saving would do before it saves.

// fetchedMsg is the text behind a link, or why it could not be fetched.
type fetchedMsg struct {
	from *importScreen
	link string
	text string
	err  error
}

type importScreen struct {
	sess     *session
	back     *profilesScreen
	text     string
	fetching string   // the link being fetched
	problems []string // why the text cannot be imported as it is
	flash    flash
	scroll   scroller
}

func newImport(sess *session, back *profilesScreen) *importScreen {
	return &importScreen{sess: sess, back: back}
}

func (s *importScreen) init() tea.Cmd { return nil }

func (s *importScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case fetchedMsg:
		if msg.from != s || msg.link != s.fetching {
			return s, nil
		}
		s.fetching = ""
		if msg.err != nil {
			s.flash.stay(gFailed + " " + sRed.Render(clean(msg.err.Error())))
			return s, nil
		}
		s.text = importText(msg.text)
		return s, s.preview()
	case tea.PasteMsg:
		if s.fetching == "" {
			s.text += importText(msg.Content)
			s.problems = nil
		}
	case tea.KeyPressMsg:
		if s.fetching != "" {
			return s, nil
		}
		s.flash.clear()
		return s, s.key(msg)
	}
	return s, nil
}

// importText keeps the text's lines and tabs and drops what could not be
// in a profile: other control characters and direction overrides. Some
// terminals paste a line break as a carriage return.
func importText(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r), unicode.Is(unicode.Bidi_Control, r):
			return -1
		}
		return r
	}, s)
}

func (s *importScreen) key(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		return func() tea.Msg { return switchMsg{s.back} }
	case "ctrl+u":
		s.text, s.problems = "", nil
	case "backspace":
		if r := []rune(s.text); len(r) > 0 {
			s.text = string(r[:len(r)-1])
		}
		s.problems = nil
	case "enter":
		text := strings.TrimSpace(s.text)
		if text == "" {
			return nil
		}
		if isLink(text) {
			s.fetching = text
			s.flash.clear()
			return s.fetch(text)
		}
		return s.preview()
	default:
		if msg.Text != "" && (msg.Mod == 0 || msg.Mod == tea.ModShift) {
			s.text += importText(msg.Text)
			s.problems = nil
		}
	}
	return nil
}

// isLink reports whether the text is one line that looks like a web
// address.
func isLink(text string) bool {
	lower := strings.ToLower(text)
	return !strings.ContainsAny(text, "\n") && (strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://"))
}

func (s *importScreen) fetch(link string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		text, err := fetchProfile(ctx, link, nil)
		return fetchedMsg{s, link, text, err}
	}
}

// preview checks the text and opens the preview, or lists the problems.
func (s *importScreen) preview() tea.Cmd {
	p, warnings, err := profile.Parse(s.text)
	var pe *profile.Error
	switch {
	case errors.As(err, &pe):
		s.problems = pe.Problems
		return nil
	case err != nil:
		s.problems = []string{err.Error()}
		return nil
	}
	s.problems = nil
	next := newImportPreview(s.sess, s, p, warnings)
	return func() tea.Msg { return switchMsg{next} }
}

func (s *importScreen) view(width, height int) string {
	var b strings.Builder
	b.WriteString(header(width, s.sess, "Garner", "Profiles", "Import") + "\n\n")
	b.WriteString("   " + sMuted.Render("Paste a profile, or type a link to one, then press enter.") + "\n\n")
	var lines []string
	link := isLink(strings.TrimSpace(s.text))
	for _, l := range strings.Split(s.text, "\n") {
		switch {
		case link:
			lines = append(lines, lipgloss.NewStyle().Foreground(cIris).Underline(true).Render(l))
		case strings.HasPrefix(strings.TrimSpace(l), "#"):
			lines = append(lines, sSubtle.Render(l))
		default:
			lines = append(lines, sNormal.Render(l))
		}
	}
	if s.fetching == "" {
		lines[len(lines)-1] += lipgloss.NewStyle().Background(cFg).Render(" ")
	}
	b.WriteString(indent(frame("profile", lines, min(width, designWidth)-6), 3) + "\n")
	if n := len(s.problems); n > 0 {
		b.WriteString("\n   " + gFailed + " " + sRed.Render(fmt.Sprintf("%s; the profile cannot be imported as it is", plural(n, "problem"))) + "\n")
		for _, p := range s.problems {
			b.WriteString("   " + gFailed + " " + sNormal.Render(clean(p)) + "\n")
		}
	}
	return s.scroll.layout(width, height, b.String(), s.bottom())
}

func (s *importScreen) bottom() string {
	switch {
	case s.fetching != "":
		return sMuted.Render("Fetching " + clean(s.fetching) + "…")
	case s.flash.text != "":
		return s.flash.text
	}
	return keyBar("enter", "preview", "ctrl+u", "clear", "esc", "cancel")
}

func (s *importScreen) flashed() *flash { return &s.flash }

// captures: the pane takes every key.
func (s *importScreen) captures() bool { return true }

// fetchTimeout bounds fetching a profile behind a link.
const fetchTimeout = 15 * time.Second

// fetchProfile reads a profile's text from an https link. It follows
// redirects only to https, and refuses more than profile.MaxSize bytes.
// A github.com page link is read in its raw form. rt is the transport, nil
// for the default.
func fetchProfile(ctx context.Context, link string, rt http.RoundTripper) (string, error) {
	link = rawLink(link)
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", errors.New("only https links are fetched")
	}
	cl := &http.Client{
		Transport: rt,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New("the link redirects away from https")
			}
			if len(via) >= 10 {
				return errors.New("the link redirects too often")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain, application/toml, */*")
	resp, err := cl.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return "", fmt.Errorf("could not fetch the link: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the link answered %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, profile.MaxSize+1))
	if err != nil {
		return "", fmt.Errorf("could not read the link: %w", err)
	}
	if len(data) > profile.MaxSize {
		return "", fmt.Errorf("the link holds more than %d KiB; a profile is a few hundred bytes", profile.MaxSize>>10)
	}
	return string(data), nil
}

// rawLink turns a github.com page link, .../blob/..., into the link to
// the file itself on raw.githubusercontent.com.
func rawLink(link string) string {
	u, err := url.Parse(link)
	if err != nil || u.Host != "github.com" {
		return link
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) < 5 || parts[2] != "blob" {
		return link
	}
	return "https://raw.githubusercontent.com/" + parts[0] + "/" + parts[1] + "/" + strings.Join(parts[3:], "/")
}

// importPreviewScreen shows the profile a text holds before it is saved,
// and what changes when a profile of that name exists.
type importPreviewScreen struct {
	sess     *session
	back     *importScreen
	p        profile.Profile
	warnings []string
	existing *profileRow // the stored profile of the same name
	busy     string
	flash    flash
	scroll   scroller
}

// importSavedMsg is the server's answer to saving.
type importSavedMsg struct {
	from  *importPreviewScreen
	saved api.Profile
	err   error
}

func newImportPreview(sess *session, back *importScreen, p profile.Profile, warnings []string) *importPreviewScreen {
	s := &importPreviewScreen{sess: sess, back: back, p: p, warnings: warnings}
	for _, r := range back.back.rows {
		if r.Name == p.Name {
			s.existing = &r // a copy: the list behind may reload meanwhile
		}
	}
	return s
}

func (s *importPreviewScreen) init() tea.Cmd { return nil }

func (s *importPreviewScreen) update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case importSavedMsg:
		if msg.from != s {
			return s, nil
		}
		s.busy = ""
		var serverErr *client.ServerError
		switch {
		case errors.Is(msg.err, client.ErrUnauthorized):
			return s, func() tea.Msg { return disconnectedMsg{errRevoked} }
		case errors.As(msg.err, &serverErr) && len(serverErr.Problems) > 0:
			s.flash.stay(gFailed + " " + sRed.Render("Not saved: "+clean(strings.Join(serverErr.Problems, "; "))))
		case msg.err != nil:
			s.flash.stay(gFailed + " " + sRed.Render("Not saved: "+clean(msg.err.Error())))
		default:
			text := "Imported " + s.p.Name
			if s.existing != nil {
				text = "Replaced " + s.p.Name
				if n := len(s.existing.Shows); n > 0 {
					text += " · " + follows(n)
				}
			}
			back := s.back.back.returned(msg.saved.ID, gDone+" "+sNormal.Render(text))
			return s, func() tea.Msg { return switchMsg{back} }
		}
	case tea.KeyPressMsg:
		if s.busy != "" {
			return s, nil
		}
		s.flash.clear()
		if s.scroll.key(msg.String()) {
			return s, nil
		}
		switch msg.String() {
		case "esc":
			return s, func() tea.Msg { return switchMsg{s.back} }
		case "e":
			var id int64
			if s.existing != nil {
				id = s.existing.ID
			}
			next := newEditor(s.sess, s.back.back, id, s.p, s.shows())
			// The import is unsaved: leaving the editor asks, unless it equals
			// the stored profile.
			next.opened = ""
			if s.existing != nil {
				next.opened = profile.Encode(s.existing.parsed)
			}
			return s, func() tea.Msg { return switchMsg{next} }
		case "enter":
			return s, s.save()
		}
	}
	return s, nil
}

func (s *importPreviewScreen) shows() int {
	if s.existing == nil {
		return 0
	}
	return len(s.existing.Shows)
}

func (s *importPreviewScreen) save() tea.Cmd {
	s.busy = "Saving " + s.p.Name + "…"
	cl, text := s.sess.client, profile.Encode(s.p)
	var id int64
	if s.existing != nil {
		id = s.existing.ID
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var saved api.Profile
		var err error
		if id == 0 {
			saved, err = cl.CreateProfile(ctx, text)
		} else {
			saved, err = cl.ReplaceProfile(ctx, id, text)
		}
		return importSavedMsg{s, saved, err}
	}
}

func (s *importPreviewScreen) view(width, height int) string {
	w := min(width, designWidth) - 6
	var b strings.Builder
	b.WriteString(header(width, s.sess, "Garner", "Profiles", "Import", "Preview") + "\n\n")
	b.WriteString("   " + sBold.Render(s.p.Name) + "\n")
	if s.existing != nil {
		b.WriteString(s.replaceLines(w))
	} else {
		if s.p.Description != "" {
			b.WriteString("   " + sMuted.Render(s.p.Description) + "\n")
		}
		b.WriteString("\n" + indent(frame("preferences", []string{sNormal.Render(prefsLine(s.p))}, w), 3) + "\n\n")
		for _, l := range settingLines(s.p, w) {
			b.WriteString("   " + l + "\n")
		}
	}
	if len(s.warnings) > 0 {
		b.WriteString("\n")
		for _, warning := range s.warnings {
			b.WriteString("   " + sAmber.Render("!") + " " + sNormal.Render(clean(warning)) + "\n")
		}
	}
	return s.scroll.layout(width, height, b.String(), s.bottom())
}

// replaceLines say which profile saving replaces, which shows follow, and
// what changes.
func (s *importPreviewScreen) replaceLines(w int) string {
	old := s.existing
	var b strings.Builder
	n := len(old.Shows)
	uses := fmt.Sprintf("%d shows use it and follow", n)
	if n == 1 {
		uses = "1 show uses it and follows"
	}
	if n == 0 {
		b.WriteString("   " + sAmber.Render("!") + " " + sNormal.Render("Saving replaces your profile "+old.Name+". No show uses it.") + "\n")
	} else {
		b.WriteString("   " + sAmber.Render("!") + " " + sNormal.Render("Saving replaces your profile "+old.Name+". "+uses+" the new settings:") + "\n")
		for _, sh := range old.Shows {
			b.WriteString("     " + sNormal.Render(clean(sh)) + "\n")
		}
	}
	b.WriteString("\n" + indent(frame("what changes", s.changeLines(), w), 3) + "\n")
	return b.String()
}

// changeLines list the settings that differ, before and after, and the
// preferences before and after.
func (s *importPreviewScreen) changeLines() []string {
	const beforeWidth = 30
	old, p := s.existing.parsed, s.p
	lines := []string{sSubtle.Render(pad("", settingWidth) + pad("before", beforeWidth) + "after")}
	diff := func(label, before, after string) {
		if before != after {
			lines = append(lines, labelled(label, sMuted.Render(fit(before, beforeWidth-1, false))+" "+sNormal.Render(after)))
		}
	}
	diff("Audio", then(languageNames(old.Audio)), then(languageNames(p.Audio)))
	diff("Resolution", then(resolutionNames(old.Resolution)), then(resolutionNames(p.Resolution)))
	for _, o := range profileOptions {
		diff(o.label, o.get(old), o.get(p))
	}
	diff("Description", old.Description, p.Description)
	if len(lines) == 1 {
		lines = append(lines, sMuted.Render(pad("", settingWidth)+"nothing: the settings are the same"))
	}
	return append(lines, "",
		labelled("Preferences", sSubtle.Render(pad("before", 8))+sMuted.Render(prefsLine(old))),
		labelled("", sSubtle.Render(pad("after", 8))+sNormal.Render(prefsLine(p))))
}

func (s *importPreviewScreen) bottom() string {
	switch {
	case s.busy != "":
		return sMuted.Render(s.busy)
	case s.flash.text != "":
		return s.flash.text
	}
	return keyBar("enter", "save", "e", "edit", "esc", "cancel")
}

func (s *importPreviewScreen) flashed() *flash { return &s.flash }

func (s *importPreviewScreen) captures() bool { return s.busy != "" }

func (s *importPreviewScreen) help() (string, []string) {
	return "Import preview", []string{
		"enter", "save the profile", "e", "open it in the editor first",
		"esc", "back to the text",
	}
}
