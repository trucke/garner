package tui

import (
	"testing"
	"time"

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
