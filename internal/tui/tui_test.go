package tui

import (
	"testing"

	"github.com/trucke/garner/internal/catalog"
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
