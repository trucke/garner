package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/store"
)

// TestServices reports services that are not set up as such, without
// asking them, and sums up the library.
func TestServices(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	st, err := store.Open(ctx, t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveSeries(ctx, store.SeriesData{
		Series: store.Series{ID: 42, Slug: "x", Name: "Harbor Precinct", OriginalLanguage: "eng", OriginalCountry: "usa",
			Status: "Ended", DefaultOrder: "official", TVDBUpdated: "2026-01-01", Profile: "tv"},
		Episodes: []store.Episode{{ID: 101, Aired: "2026-01-01", Numbers: map[string]store.Position{"official": {Season: 1, Number: 1}}}},
	}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: &config.Config{Library: config.Library{TV: "/media/tv"}}, store: st, log: log, events: newBroker(), tlsMode: TLSOff}
	api := httptest.NewServer(s.Routes())
	defer api.Close()
	token, _ := st.CreateToken(ctx, "test")
	req, _ := http.NewRequest(http.MethodGet, api.URL+"/api/v1/status/services", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got Services
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Services) != 4 {
		t.Fatalf("services = %+v", got.Services)
	}
	for _, c := range got.Services {
		if c.OK || c.Detail == "" {
			t.Errorf("%s without setup: %+v", c.Name, c)
		}
	}
	if l := got.Library; l.Shows != 1 || l.Profiles["tv"] != 1 || l.Aired != 1 || len(l.Folders) != 1 {
		t.Errorf("library = %+v", l)
	}
}
