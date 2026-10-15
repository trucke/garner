package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/store"
)

// TestDeleteSeries checks that a show is not deleted while its files are
// being moved, and that deleting it removes its files and its records.
// It also checks that changing the profile clears stored search results.
func TestDeleteSeries(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	st, err := store.Open(ctx, t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	if err := st.SaveSeries(ctx, store.SeriesData{
		Series: store.Series{ID: 42, Slug: "harbor-precinct", Name: "Harbor Precinct", OriginalLanguage: "eng",
			OriginalCountry: "usa", Status: "Ended", DefaultOrder: "official", TVDBUpdated: "2026-01-01", Profile: "tv"},
		Episodes: []store.Episode{
			{ID: 101, Aired: "2026-01-01", Numbers: map[string]store.Position{"official": {Season: 1, Number: 1}}},
			{ID: 102, Aired: "2026-01-08", Numbers: map[string]store.Position{"official": {Season: 1, Number: 2}}},
		},
	}, true, now); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(t.TempDir(), "Harbor.Precinct.[tvdbid-42]", "Season.01", "Harbor.Precinct.S01E01.mkv")
	if err := os.MkdirAll(filepath.Dir(video), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(video, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEpisodeFile(ctx, store.EpisodeFile{EpisodeID: 101, Path: video, Size: 5}, 0, now); err != nil {
		t.Fatal(err)
	}
	d, err := st.StartDownload(ctx, store.Download{EpisodeID: 102, Title: "Harbor.Precinct.S01E02.1080p.WEB-EXAMPLE", Link: "x"}, now)
	if err != nil {
		t.Fatal(err)
	}
	d.State = store.Completed
	if err := st.UpdateDownload(ctx, d, now); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateImport(ctx, d.ID, store.Import{State: store.ImportMoving}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSearch(ctx, store.Search{EpisodeID: 101, Searched: now, Outcome: "nothing", Summary: "No matching German release found"}); err != nil {
		t.Fatal(err)
	}

	s := &Server{cfg: &config.Config{}, store: st, log: log, events: newBroker(), tlsMode: TLSOff}
	api := httptest.NewServer(s.Routes())
	defer api.Close()
	token, err := st.CreateToken(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) int {
		t.Helper()
		req, _ := http.NewRequest(method, api.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := call(http.MethodPatch, "/api/v1/series/42", `{"profile":"anime"}`); code != http.StatusOK {
		t.Fatalf("change profile: %d", code)
	}
	if _, err := st.LoadSearch(ctx, 101); err == nil {
		t.Error("the search judged with the old profile survived the change")
	}

	if err := st.SaveSearch(ctx, store.Search{EpisodeID: 101, Searched: now, Outcome: "nothing", Summary: "No matching release found"}); err != nil {
		t.Fatal(err)
	}
	if code := call(http.MethodDelete, "/api/v1/series/42?files=delete", ""); code != http.StatusConflict {
		t.Fatalf("delete while files are moved: %d, want 409", code)
	}
	if _, err := os.Stat(video); err != nil {
		t.Fatal("a refused delete removed a file")
	}

	if err := st.UpdateImport(ctx, d.ID, store.Import{State: store.ImportDone}, now); err != nil {
		t.Fatal(err)
	}
	// Without saying what happens to the files, nothing happens.
	if code := call(http.MethodDelete, "/api/v1/series/42", ""); code != http.StatusBadRequest {
		t.Fatalf("delete without files=: %d, want 400", code)
	}
	if ok, _ := st.HasSeries(ctx, 42); !ok {
		t.Fatal("a refused delete removed the show")
	}
	if code := call(http.MethodDelete, "/api/v1/series/42?files=delete", ""); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	if _, err := os.Stat(video); !os.IsNotExist(err) {
		t.Error("the imported file is still there")
	}
	if ok, _ := st.HasSeries(ctx, 42); ok {
		t.Error("the show is still in the catalogue")
	}
	// Its records go with it, so adding it again starts clean.
	if _, found, _ := st.EpisodeFile(ctx, 101); found {
		t.Error("the record of a deleted file survived")
	}
	if _, err := st.GetDownload(ctx, d.ID); err == nil {
		t.Error("a download of the deleted show survived")
	}
	if _, err := st.LoadSearch(ctx, 101); err == nil {
		t.Error("a search of the deleted show survived")
	}
	if shows, err := st.Library(ctx, now); err != nil || len(shows) != 0 {
		t.Errorf("library after delete: %d shows, %v", len(shows), err)
	}

	// files=keep removes the show and leaves its files in the library.
	if err := st.SaveSeries(ctx, store.SeriesData{
		Series: store.Series{ID: 43, Slug: "quiet-harbour", Name: "Quiet Harbour", OriginalLanguage: "eng",
			OriginalCountry: "usa", Status: "Ended", DefaultOrder: "official", TVDBUpdated: "2026-01-01", Profile: "tv"},
		Episodes: []store.Episode{{ID: 201, Aired: "2026-01-01", Numbers: map[string]store.Position{"official": {Season: 1, Number: 1}}}},
	}, true, now); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(t.TempDir(), "Quiet.Harbour.[tvdbid-43]", "Season.01", "Quiet.Harbour.S01E01.mkv")
	if err := os.MkdirAll(filepath.Dir(kept), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kept, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEpisodeFile(ctx, store.EpisodeFile{EpisodeID: 201, Path: kept, Size: 5}, 0, now); err != nil {
		t.Fatal(err)
	}
	if code := call(http.MethodDelete, "/api/v1/series/43?files=keep", ""); code != http.StatusOK {
		t.Fatalf("delete keeping files: %d", code)
	}
	if ok, _ := st.HasSeries(ctx, 43); ok {
		t.Error("the show is still in the catalogue")
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("the kept file is gone: %v", err)
	}
}
