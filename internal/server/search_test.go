package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/download"
	"github.com/trucke/garner/internal/prowlarr"
	"github.com/trucke/garner/internal/sabnzbd"
	"github.com/trucke/garner/internal/search"
	"github.com/trucke/garner/internal/store"
)

// TestSearchAndDownload searches through the API against a fake Prowlarr
// and sends a result to a fake SABnzbd: the results are ranked, a failing
// indexer is reported, and download links, which hold Prowlarr's API key,
// never reach the client.
func TestSearchAndDownload(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	st, err := store.Open(ctx, t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveSeries(ctx, store.SeriesData{
		Series: store.Series{ID: 42, Slug: "harbor-precinct", Name: "Harbor Precinct", OriginalLanguage: "eng",
			OriginalCountry: "usa", Status: "Continuing", DefaultOrder: "official", TVDBUpdated: "2026-01-01", Profile: "tv"},
		Episodes: []store.Episode{
			{ID: 101, Aired: "2026-01-01", Numbers: map[string]store.Position{"official": {Season: 1, Number: 1}}},
			{ID: 102, Aired: "2026-01-08", Numbers: map[string]store.Position{"official": {Season: 1, Number: 2}}},
		},
	}, true, time.Now()); err != nil {
		t.Fatal(err)
	}

	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/indexer":
			fmt.Fprint(w, `[{"id":1,"name":"One","enable":true,"protocol":"usenet"},{"id":2,"name":"Two","enable":true,"protocol":"usenet"}]`)
		case "/1/api":
			var items strings.Builder
			for _, title := range []string{
				"Harbor.Precinct.S01E01.German.DL.720p.WEB-GRP",
				"Harbor.Precinct.S01E01.1080p.WEB-DL-GRP",
				"Harbor.Precinct.S01E01.German.DL.1080p.BluRay-GRP",
			} {
				fmt.Fprintf(&items, `<item><title>%s</title><link>http://prowlarr/1/download?apikey=SECRET&amp;link=X</link><size>1</size></item>`, title)
			}
			fmt.Fprintf(w, `<rss><channel>%s</channel></rss>`, items.String())
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer indexer.Close()

	s := &Server{cfg: &config.Config{}, store: st, log: log, events: newBroker(), tlsMode: TLSOff}
	s.search = search.New(st, prowlarr.New(indexer.URL, "key", indexer.Client()), nil, log)
	var progress []string
	s.search.OnProgress = func(p search.Progress) {
		var states []string
		for _, ix := range p.Indexers {
			states = append(states, fmt.Sprintf("%s:%v:%d", ix.Name, ix.Done, ix.Results))
		}
		progress = append(progress, strings.Join(states, " "))
	}
	api := httptest.NewServer(s.Routes())
	defer api.Close()
	token, err := st.CreateToken(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, api.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		code, body := call(method, "/api/v1/episodes/101/search", "")
		if code != http.StatusOK || strings.Contains(body, "SECRET") || strings.Contains(body, "download") {
			t.Fatalf("%s search: %d %s", method, code, body)
		}
		var sr store.Search
		if err := json.Unmarshal([]byte(body), &sr); err != nil {
			t.Fatal(err)
		}
		if sr.Outcome != "found" || !strings.Contains(sr.Summary, "Two failed") || len(sr.Candidates) != 3 ||
			sr.Candidates[0].Title != "Harbor.Precinct.S01E01.German.DL.1080p.BluRay-GRP" || sr.Candidates[2].Verdict != "reject" {
			t.Errorf("%s search = %+v", method, sr)
		}
		// The reasons that decided a verdict are told apart from facts.
		if c := sr.Candidates; c[0].Decisive < 1 || c[0].Decisive > len(c[0].Reasons) || c[2].Decisive < 1 || c[2].Decisive > len(c[2].Reasons) {
			t.Errorf("%s search: decisive reasons %d and %d of %v", method, c[0].Decisive, c[2].Decisive, c[2].Reasons)
		}
	}
	// Every indexer is reported as asked first, then as it answers.
	if want := []string{"One:false:0 Two:false:0", "One:true:3 Two:false:0", "One:true:3 Two:true:0"}; fmt.Sprint(progress) != fmt.Sprint(want) {
		t.Errorf("progress = %q, want %q", progress, want)
	}
	sab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.PostFormValue("mode") == "addurl" {
			fmt.Fprint(w, `{"status":true,"nzo_ids":["job-1"]}`)
			return
		}
		fmt.Fprint(w, `{"queue":{"slots":[]},"history":{"slots":[]}}`)
	}))
	defer sab.Close()
	s.downloads = download.New(st, sabnzbd.New(sab.URL, "key", sab.Client()), "tv", log)
	_, body := call(http.MethodGet, "/api/v1/episodes/101/search", "")
	var sr store.Search
	json.Unmarshal([]byte(body), &sr)
	for _, path := range []string{fmt.Sprintf("/api/v1/candidates/%d/download", sr.Candidates[0].ID), "/api/v1/downloads"} {
		method := http.MethodPost
		if path == "/api/v1/downloads" {
			method = http.MethodGet
		}
		code, body := call(method, path, "")
		if code >= 300 || strings.Contains(body, "SECRET") || !strings.Contains(body, `"job_id":"job-1"`) {
			t.Errorf("%s %s: %d %s", method, path, code, body)
		}
	}

	if code, _ := call(http.MethodPost, "/api/v1/episodes/999/search", ""); code != http.StatusNotFound {
		t.Errorf("unknown episode: %d", code)
	}
	if code, body := call(http.MethodPatch, "/api/v1/series/42", `{"profile":"anime"}`); code != http.StatusOK || !strings.Contains(body, `"profile":"anime"`) {
		t.Errorf("set profile: %d %s", code, body)
	}
}
