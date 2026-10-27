package tui

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/client"
	"github.com/trucke/garner/internal/connect"
)

// recorder answers every request with an empty JSON object and remembers
// what the interface asked for, so a test can see which request an answer
// produced.
type recorder struct {
	mu       sync.Mutex
	requests []string
	bodies   map[string]string // the last body sent to each request
}

func newRecorder(t *testing.T) (*recorder, *client.Client) {
	t.Helper()
	var rec recorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.requests = append(rec.requests, r.Method+" "+r.URL.RequestURI())
		if rec.bodies == nil {
			rec.bodies = map[string]string{}
		}
		rec.bodies[r.Method+" "+r.URL.RequestURI()] = string(body)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	}))
	t.Cleanup(srv.Close)
	cl, err := client.New(connect.Code{URL: srv.URL, Token: "garner_" + strings.Repeat("a", 26)})
	if err != nil {
		t.Fatal(err)
	}
	return &rec, cl
}

// made lists the requests of one method, ignoring the reloads that follow
// an answer.
func (rec *recorder) made(method string) []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var made []string
	for _, r := range rec.requests {
		if strings.HasPrefix(r, method+" ") {
			made = append(made, r)
		}
	}
	return made
}

// body is the last body sent with a request, such as "PUT /api/v1/profiles/3".
func (rec *recorder) body(request string) string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.bodies[request]
}

// keyMsg builds the key press the interface would receive for a named key
// or letter.
func keyMsg(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	}
	return tea.KeyPressMsg{Code: rune(k[0]), Text: k}
}

// sendKeys sends keys to the app and returns the command of the last one.
func sendKeys(a *app, keys ...string) tea.Cmd {
	var cmd tea.Cmd
	for _, k := range keys {
		_, cmd = a.update(keyMsg(k))
	}
	return cmd
}

// libraryApp is an app whose library lists Harbor Precinct with files when
// withFiles, without them otherwise.
func libraryApp(cl *client.Client, withFiles bool) (*app, *libraryScreen) {
	sess := &session{client: cl}
	a := &app{sess: sess, library: newLibrary(sess)}
	a.screen = a.library
	a.library.loaded = true
	a.library.selected = 7
	var bytes int64
	if withFiles {
		bytes = 3 << 30
	}
	a.sess.shows = []api.Show{{Series: api.Series{ID: 7, Name: "Harbor Precinct"}, Aired: 4, Imported: 2, Bytes: bytes}}
	return a, a.library
}

// TestDeleteDialogFromLibrary checks the delete question the library opens:
// Cancel comes first and is preselected, Escape cancels, keeping the files
// is offered only when the show has them, and only Delete files asks the
// server to delete them. The details page shares deleteKey and deleteShow,
// so the request checks here cover both entry points.
func TestDeleteDialogFromLibrary(t *testing.T) {
	rec, cl := newRecorder(t)
	a, lib := libraryApp(cl, true)

	// The question opens with Cancel preselected, and offers to keep the
	// files of a show that has them.
	sendKeys(a, "d")
	if !lib.asking || lib.choice != 0 {
		t.Fatalf("after d: asking %v, choice %d", lib.asking, lib.choice)
	}
	if view := ansi.Strip(lib.view(120, 36)); !strings.Contains(view, "Keep files") {
		t.Fatalf("the question does not offer to keep the files: %q", view)
	}

	// Enter on the preselected Cancel asks for nothing and keeps the show.
	if cmd := sendKeys(a, "enter"); cmd != nil {
		t.Fatal("enter on Cancel returned a command")
	}
	if lib.asking {
		t.Error("enter on Cancel left the question open")
	}
	if n := len(rec.made("DELETE")); n != 0 {
		t.Errorf("Cancel asked the server to delete: %v", rec.made("DELETE"))
	}
	if _, ok := lib.cursorShow(); !ok {
		t.Error("cancelling removed the show from the library")
	}

	// Escape cancels too.
	sendKeys(a, "d", "esc")
	if lib.asking {
		t.Error("esc left the question open")
	}
	if n := len(rec.made("DELETE")); n != 0 {
		t.Errorf("Escape asked the server to delete: %v", rec.made("DELETE"))
	}

	// Keeping the files removes the show but asks to keep them.
	cmd := sendKeys(a, "d", "right", "enter")
	if cmd == nil {
		t.Fatal("Keep files returned no command")
	}
	cmd() // the answer reaches the recording server
	if made := rec.made("DELETE"); len(made) != 1 || made[0] != "DELETE /api/v1/series/7?files=keep" {
		t.Errorf("Keep files asked %v", made)
	}
}

// TestDeleteDialogDeleteFiles checks that Delete files asks the server to
// delete the show with its files.
func TestDeleteDialogDeleteFiles(t *testing.T) {
	rec, cl := newRecorder(t)
	a, _ := libraryApp(cl, true)

	cmd := sendKeys(a, "d", "right", "right", "enter")
	if cmd == nil {
		t.Fatal("Delete files returned no command")
	}
	if _, ok := cmd().(deleteResultMsg); !ok {
		t.Fatal("the command did not return a delete result")
	}
	if made := rec.made("DELETE"); len(made) != 1 || made[0] != "DELETE /api/v1/series/7?files=delete" {
		t.Errorf("Delete files asked %v", made)
	}
}

// TestDeleteDialogWithoutFiles checks the question about a show without
// files: it offers only Cancel and Delete, and Delete asks the server to
// delete.
func TestDeleteDialogWithoutFiles(t *testing.T) {
	rec, cl := newRecorder(t)
	a, lib := libraryApp(cl, false)

	sendKeys(a, "d")
	if view := ansi.Strip(lib.view(120, 36)); strings.Contains(view, "Keep files") {
		t.Error("the question offered to keep files a show does not have")
	}
	// With two choices, right lands on Delete and back on Cancel.
	sendKeys(a, "right")
	if lib.choice != 1 {
		t.Fatalf("right landed on choice %d, Delete was expected", lib.choice)
	}
	sendKeys(a, "left")
	if lib.choice != 0 {
		t.Fatalf("left landed on choice %d, Cancel was expected", lib.choice)
	}
	if cmd := sendKeys(a, "enter"); cmd != nil || len(rec.made("DELETE")) != 0 {
		t.Error("enter on Cancel asked the server to delete")
	}
	cmd := sendKeys(a, "d", "right", "enter")
	if cmd == nil {
		t.Fatal("Delete returned no command")
	}
	cmd()
	if made := rec.made("DELETE"); len(made) != 1 || made[0] != "DELETE /api/v1/series/7?files=delete" {
		t.Errorf("Delete asked %v", made)
	}
}

// TestDeleteDialogFromDetails checks the same question on the details
// page: it opens with Cancel preselected, Escape cancels, and the answers
// reach the server through the shared deleteShow.
func TestDeleteDialogFromDetails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		how   []string
		files string // the files= query, empty when nothing is asked
	}{
		{"Cancel", []string{"d", "enter"}, ""},
		{"Escape", []string{"d", "esc"}, ""},
		{"Keep files", []string{"d", "right", "enter"}, "keep"},
		{"Delete files", []string{"d", "right", "right", "enter"}, "delete"},
	} {
		rec, cl := newRecorder(t)
		sess := &session{client: cl}
		show := newShow(sess, nil, 7, "Harbor Precinct", "")
		show.setDetail(api.SeriesDetail{
			SeriesData: api.SeriesData{Series: api.Series{ID: 7, Name: "Harbor Precinct", DefaultOrder: "official"}},
			Summary:    api.Show{Bytes: 3 << 30, Aired: 4, Imported: 2},
		})
		a := &app{sess: sess, library: newLibrary(sess)}
		a.screen = newShowDetails(sess, show, a.library)

		cmd := sendKeys(a, tc.how...)
		if tc.files == "" {
			if cmd != nil {
				t.Errorf("%s: returned a command", tc.name)
			}
		} else if cmd == nil {
			t.Errorf("%s: returned no command", tc.name)
		} else {
			cmd()
		}
		made := rec.made("DELETE")
		if tc.files == "" && len(made) != 0 {
			t.Errorf("%s asked %v", tc.name, made)
		}
		if tc.files != "" && (len(made) != 1 || made[0] != "DELETE /api/v1/series/7?files="+tc.files) {
			t.Errorf("%s asked %v", tc.name, made)
		}
		if det, ok := a.screen.(*showDetailsScreen); !ok || det.overlay != noOverlay {
			t.Errorf("%s left the question open", tc.name)
		}
	}
}

// TestRenameAfterTakeOver checks the question after a take-over: Keep as
// they are is preselected, leaving the question renames nothing, and only
// Rename and move asks the server to rename.
func TestRenameAfterTakeOver(t *testing.T) {
	rec, cl := newRecorder(t)
	sess := &session{client: cl}
	show := newShow(sess, nil, 7, "Harbor Precinct", "")
	show.setDetail(api.SeriesDetail{
		SeriesData: api.SeriesData{Series: api.Series{ID: 7, Name: "Harbor Precinct", DefaultOrder: "official"}},
	})
	a := &app{sess: sess, library: newLibrary(sess)}
	a.screen = show

	// The question after a take-over: Keep as they are is preselected.
	show.files.show(api.TakeOver{Files: 3, Episodes: 3, Renamable: 2})
	if show.files.choice != 0 {
		t.Errorf("the question preselected choice %d, Keep as they are was expected", show.files.choice)
	}

	// Enter on the preselected Keep as they are renames nothing.
	if cmd := sendKeys(a, "enter"); cmd != nil {
		t.Fatal("enter on Keep as they are returned a command")
	}
	if show.files.open {
		t.Error("enter on Keep as they are left the question open")
	}
	if made := rec.made("POST"); len(made) != 0 {
		t.Errorf("Keep as they are asked the server to rename: %v", made)
	}

	// Escape leaves the question and renames nothing either.
	show.files.show(api.TakeOver{Files: 3, Episodes: 3, Renamable: 2})
	if cmd := sendKeys(a, "esc"); cmd != nil || show.files.open {
		t.Error("esc did not close the question")
	}
	if made := rec.made("POST"); len(made) != 0 {
		t.Errorf("Escape asked the server to rename: %v", made)
	}

	// Rename and move asks the server to rename.
	show.files.show(api.TakeOver{Files: 3, Episodes: 3, Renamable: 2})
	cmd := sendKeys(a, "right", "enter")
	if cmd == nil {
		t.Fatal("Rename and move returned no command")
	}
	if _, ok := cmd().(renamedMsg); !ok {
		t.Fatal("the command did not return a rename result")
	}
	if made := rec.made("POST"); len(made) != 1 || made[0] != "POST /api/v1/series/7/files/rename" {
		t.Errorf("Rename and move asked %v", made)
	}
}
