package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/catalog"
	"github.com/trucke/garner/internal/server"
	"github.com/trucke/garner/internal/store"
)

// TestLateAnswers checks that an answer reaches only the screen that asked
// for it: a details page left early must not fill the next one, and a
// library from an ended session must not replace the current one.
func TestLateAnswers(t *testing.T) {
	sess := &session{}
	left := newPreview(sess, nil, catalog.SearchResult{ID: 1, Name: "First"})
	next := newPreview(sess, nil, catalog.SearchResult{ID: 2, Name: "Second"})
	next.update(previewMsg{from: left, preview: catalog.Preview{SearchResult: catalog.SearchResult{ID: 1}}})
	if next.preview != nil {
		t.Fatal("the details of a page that was left showed on the next one")
	}

	old := newLibrary(sess)
	a := &app{sess: sess, library: newLibrary(sess)}
	a.screen = a.library
	a.library.loading = true
	a.Update(libraryMsg{from: old, shows: []store.Show{{Series: store.Series{ID: 9}}}})
	if !a.library.loading || len(sess.shows) != 0 {
		t.Fatal("a library answer from an ended session replaced the current library")
	}
	a.Update(libraryMsg{from: a.library, shows: []store.Show{{Series: store.Series{ID: 7}}}})
	if a.library.loading || len(sess.shows) != 1 {
		t.Fatal("the current library's own answer was not applied")
	}
}

// TestInitialFolding checks which season opens first: the one with the
// most urgent episode, the newest when nothing needs anything, and a
// single season always.
func TestInitialFolding(t *testing.T) {
	detail := func(states map[int64]string, positions map[int64][2]int) server.SeriesDetail {
		d := server.SeriesDetail{SeriesData: store.SeriesData{Series: store.Series{ID: 1, DefaultOrder: "official"}}}
		for id, pos := range positions {
			d.Episodes = append(d.Episodes, store.Episode{ID: id, Numbers: map[string]store.Position{"official": {Season: pos[0], Number: pos[1]}}})
			d.States = append(d.States, store.EpisodeStatus{EpisodeID: id, State: states[id]})
		}
		return d
	}
	positions := map[int64][2]int{31: {3, 1}, 21: {2, 1}, 22: {2, 2}, 11: {1, 1}}
	cases := []struct {
		name   string
		states map[int64]string
		open   int
		cursor rowKey
	}{
		{"most urgent", map[int64]string{31: store.EpisodeImported, 21: store.EpisodeImported, 22: store.EpisodeFailed, 11: store.EpisodeNotSearched}, 2, rowKey{2, 22}},
		{"nothing needed", map[int64]string{31: store.EpisodeUpcoming, 21: store.EpisodeImported, 22: store.EpisodeImported, 11: store.EpisodeImported}, 3, rowKey{season: 3}},
	}
	for _, c := range cases {
		s := newShow(&session{}, nil, 1, "Show", "")
		s.setDetail(detail(c.states, positions))
		folded := s.sess.folded[1]
		if folded[c.open] || !folded[1] && c.open != 1 || s.cursor != c.cursor {
			t.Errorf("%s: folded %v, cursor %v; want season %d open, cursor %v", c.name, folded, s.cursor, c.open, c.cursor)
		}
	}
	single := newShow(&session{}, nil, 1, "Show", "")
	single.setDetail(detail(map[int64]string{11: store.EpisodeImported}, map[int64][2]int{11: {1, 1}}))
	if single.sess.folded[1][1] {
		t.Error("a show's only season started folded")
	}
}

// TestFlash checks that a success message goes after its time, that an
// error stays, and that an older timer does not clear a newer message.
func TestFlash(t *testing.T) {
	var f flash
	f.info("✓ Added")
	if f.expire() == nil {
		t.Fatal("no timer for a success message")
	}
	old := f.seq
	f.info("✓ Refreshed")
	f.expire()
	a := &app{screen: newLibrary(&session{})}
	a.Update(flashDoneMsg{&f, old})
	if f.text != "✓ Refreshed" {
		t.Fatal("an older timer cleared a newer message")
	}
	a.Update(flashDoneMsg{&f, f.seq})
	if f.text != "" {
		t.Fatal("a success message stayed after its time")
	}
	f.stay("✗ TVDB did not answer")
	if f.expire() != nil {
		t.Fatal("an error message would go by itself")
	}
}

// TestPipelineFailures checks that each kind of failure marks the step it
// happened in, and the steps after it stay pending.
func TestPipelineFailures(t *testing.T) {
	checked := []store.ImportFile{{Source: "a.mkv", Check: []byte(`{"audio":["German"]}`)}}
	planned := []store.ImportFile{{Source: "a.mkv", Target: "/lib/A.mkv", Check: []byte(`{"audio":["German"]}`)}}
	for _, tc := range []struct {
		name string
		d    store.Download
		want int // the failed step
	}{
		{"SABnzbd refused it", store.Download{State: store.Failed, Failure: "refused"}, 0},
		{"missing articles", store.Download{State: store.Failed, JobID: "j", Failure: "cannot be completed"}, 1},
		{"file check rejected it", store.Download{State: store.Failed, JobID: "j", Failure: "file check: audio is Japanese",
			Import: store.Import{State: store.ImportRejected, Files: checked}}, 3},
		{"share not mounted", store.Download{State: store.Completed, JobID: "j",
			Import: store.Import{State: store.ImportFailed, Detail: "Garner cannot see it"}}, 3},
		{"move failed", store.Download{State: store.Completed, JobID: "j",
			Import: store.Import{State: store.ImportFailed, Detail: "the library folder is missing", Files: planned}}, 4},
	} {
		steps := pipeline(tc.d, time.Now())
		for i, st := range steps {
			switch {
			case i == tc.want && st.glyph != gFailed:
				t.Errorf("%s: step %d (%s) is not marked failed", tc.name, i, st.name)
			case i > tc.want && st.glyph != gWaiting:
				t.Errorf("%s: step %d (%s) after the failure is not pending", tc.name, i, st.name)
			}
		}
	}
	if st := pipeline(store.Download{State: store.Failed, JobID: "j", Failure: "file check: audio is Japanese",
		Import: store.Import{State: store.ImportRejected}}, time.Now())[3]; st.detail[0].text != "audio is Japanese" {
		t.Errorf("rejected detail = %q", st.detail[0].text)
	}
}

// TestKeysWhileTyping sends ? and A to a filter being typed; otherwise they
// open Help and the Activity.
func TestKeysWhileTyping(t *testing.T) {
	sess := &session{}
	a := &app{sess: sess, library: newLibrary(sess)}
	a.screen = a.library
	a.library.typing = true
	for _, k := range []rune{'?', 'A'} {
		a.update(tea.KeyPressMsg{Code: k, Text: string(k)})
	}
	if a.help || a.screen != screen(a.library) || a.library.filter != "?A" {
		t.Fatalf("while typing: help %v, filter %q", a.help, a.library.filter)
	}
	a.library.typing = false
	a.update(tea.KeyPressMsg{Code: '?', Text: "?"})
	if !a.help {
		t.Error("? did not open Help")
	}
	a.update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if a.help {
		t.Error("esc did not close Help")
	}
}

// TestActivityOrder lists what needs the user first, then what runs, then
// what ended; the newest first within each.
func TestActivityOrder(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 10, 16, 12, m, 0, 0, time.UTC) }
	item := func(ep int64, d store.Download) store.ActivityItem {
		d.EpisodeID = ep
		return store.ActivityItem{Download: d}
	}
	s := &activityScreen{items: []store.ActivityItem{
		item(1, store.Download{State: store.Completed, Updated: at(50), Import: store.Import{State: store.ImportDone, Updated: at(55)}}),
		item(2, store.Download{State: store.Downloading, Created: at(10)}),
		item(3, store.Download{State: store.Queued, Created: at(20)}),
		item(4, store.Download{State: store.Completed, Updated: at(1), Import: store.Import{State: store.ImportFailed, Updated: at(2)}}),
		item(5, store.Download{State: store.Cancelled, Updated: at(40)}),
	}}
	var got []int64
	for _, it := range s.rows() {
		got = append(got, it.Download.EpisodeID)
	}
	if want := []int64{4, 3, 2, 1, 5}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestFitKeys leaves out whole keys on a narrow line: "j k move" first,
// then from the right, keeping Help and the last key.
func TestFitKeys(t *testing.T) {
	bar := keyBar("j k", "move", "enter", "open", "a", "add", "d", "delete", "?", "help", "esc", "back")
	for _, tc := range []struct {
		width int
		want  string
	}{
		{200, "j k move    enter open    a add    d delete    ? help    esc back"},
		{60, "enter open    a add    d delete    ? help    esc back"},
		{41, "enter open    a add    ? help    esc back"},
		{40, "enter open    ? help    esc back"},
		{10, "? help    "},
	} {
		if got := ansi.Strip(fitKeys(bar, tc.width)); got != tc.want {
			t.Errorf("width %d: %q, want %q", tc.width, got, tc.want)
		}
	}
}

// TestScroller shows a window of tall content, marks what lies above and
// below, and reaches the last line.
func TestScroller(t *testing.T) {
	var lines []string
	for i := range 10 {
		lines = append(lines, fmt.Sprint(i))
	}
	var s scroller
	view := s.window(lines, 4)
	if len(view) != 4 || view[0] != "0" || !strings.Contains(ansi.Strip(view[3]), "more below") {
		t.Fatalf("top = %q", view)
	}
	s.key("G")
	view = s.window(lines, 4)
	if !strings.Contains(ansi.Strip(view[0]), "more above") || view[3] != "9" {
		t.Errorf("bottom = %q", view)
	}
	s.key("g")
	s.key("g")
	if view = s.window(lines, 4); view[0] != "0" {
		t.Errorf("after gg = %q", view)
	}
	if view = s.window(lines[:3], 4); len(view) != 3 {
		t.Errorf("short content = %q", view)
	}
}
