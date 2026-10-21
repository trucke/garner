package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/store"
)

// patterns lists the routes register adds.
type patterns []string

func (p *patterns) Handle(pattern string, _ http.Handler) { *p = append(*p, pattern) }

var pathValue = regexp.MustCompile(`\{[a-z_]+\}`)

// TestEveryRouteNeedsAToken calls every route of the client API without a
// token and with an unknown one: each answers 401.
func TestEveryRouteNeedsAToken(t *testing.T) {
	st, err := store.Open(context.Background(), t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{cfg: &config.Config{}, store: st, log: slog.New(slog.DiscardHandler), events: newBroker(), tlsMode: TLSOff}
	var routes patterns
	s.register(&routes)
	if len(routes) == 0 {
		t.Fatal("register added no routes")
	}
	api := s.Routes()
	for _, pattern := range routes {
		method, path, _ := strings.Cut(pattern, " ")
		path = pathValue.ReplaceAllString(path, "1")
		for _, token := range []string{"", "garner_AAAAAAAAAAAAAAAAAAAAAAAAAA"} {
			req := httptest.NewRequest(method, path, nil)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			rec := httptest.NewRecorder()
			api.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q: %d, want 401", method, path, token, rec.Code)
			}
		}
	}
}
