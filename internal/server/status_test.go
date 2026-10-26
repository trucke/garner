package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/api"
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
			Status: "Ended", DefaultOrder: "official", TVDBUpdated: "2026-01-01", Type: "tv"},
		Episodes: []store.Episode{{ID: 101, Aired: "2026-01-01", Numbers: map[string]store.Position{"official": {Season: 1, Number: 1}}}},
	}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: &config.Config{Library: config.Library{TV: "/media/tv"}}, store: st, log: log, events: newBroker(), tlsMode: TLSOff}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	token, _ := st.CreateToken(ctx, "test")
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/status/services", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got api.Services
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
	if l := got.Library; l.Shows != 1 || l.Types["tv"] != 1 || l.Aired != 1 || len(l.Folders) != 1 {
		t.Errorf("library = %+v", l)
	}
}

// TestProxied serves plain HTTP behind a proxy and puts no pin in the
// connection codes: clients verify the proxy's public certificate.
func TestProxied(t *testing.T) {
	s := &Server{cfg: &config.Config{DataDir: t.TempDir(), Server: config.Server{
		Listen: "192.168.1.20:7979", URL: "https://garner.example.com", Proxied: true}}}
	cfg, err := s.tlsConfig()
	if err != nil || cfg != nil || s.tlsMode != TLSProxy || s.pin != "" {
		t.Errorf("tls config %v, mode %q, pin %q, %v", cfg, s.tlsMode, s.pin, err)
	}
}

// rewrite sends every request to one test server, whatever its host.
type rewrite struct{ host string }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = "http", r.host
	return http.DefaultTransport.RoundTrip(req)
}

// TestTVDBStatus shows on the About screen which key Garner uses and,
// after TheTVDB asked it to slow down, that its requests are paused.
func TestTVDBStatus(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	st, err := store.Open(ctx, t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tvdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer tvdb.Close()
	s := &Server{cfg: &config.Config{TVDB: config.TVDB{APIKey: "key", BuiltIn: true}}, store: st, log: log,
		events: newBroker(), tlsMode: TLSOff, deps: Deps{Transport: rewrite{tvdb.Listener.Addr().String()}}}
	if err := s.setupCatalog(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	token, _ := st.CreateToken(ctx, "test")
	get := func(path string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	tvdbCheck := func() api.ServiceCheck {
		resp := get("/api/v1/status/services")
		defer resp.Body.Close()
		var got api.Services
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got.Services[0]
	}

	if c := tvdbCheck(); !c.OK || !strings.Contains(c.Detail, "Garner's built-in key") {
		t.Errorf("before any request: %+v", c)
	}
	get("/api/v1/search?q=harbor").Body.Close()
	if c := tvdbCheck(); c.OK || !strings.Contains(c.Detail, "paused until") {
		t.Errorf("after a 429: %+v", c)
	}
}
