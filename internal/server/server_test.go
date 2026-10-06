package server

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/connect"
	"github.com/trucke/garner/internal/store"
)

// TestTokenLifecycle covers the security-critical path: a created token
// grants access, revoking it ends open streams and rejects later requests.
func TestTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{
		cfg:   &config.Config{Server: config.Server{URL: "http://127.0.0.1:7979"}},
		store: st, log: slog.New(slog.DiscardHandler), version: "test", events: newBroker(), tlsMode: TLSOff,
	}
	api := httptest.NewServer(s.Routes())
	defer api.Close()
	admin := httptest.NewServer(s.AdminRoutes())
	defer admin.Close()

	resp, err := http.Post(admin.URL+"/admin/tokens", "application/json", strings.NewReader(`{"name":"laptop"}`))
	if err != nil {
		t.Fatal(err)
	}
	var created CreatedToken
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	code, err := connect.Parse(created.Code)
	if err != nil || code.Token != created.Token || code.URL != "http://127.0.0.1:7979" {
		t.Fatalf("connection code %q: %+v, %v", created.Code, code, err)
	}

	get := func(token string) int {
		req, _ := http.NewRequest(http.MethodGet, api.URL+"/api/v1/status", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := get(""); got != http.StatusUnauthorized {
		t.Errorf("without token: %d", got)
	}
	if got := get("garner_AAAAAAAAAAAAAAAAAAAAAAAAAA"); got != http.StatusUnauthorized {
		t.Errorf("unknown token: %d", got)
	}
	if got := get(created.Token); got != http.StatusOK {
		t.Fatalf("valid token: %d", got)
	}

	streamCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, api.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	lines := bufio.NewScanner(stream.Body)
	hello := false
	for !hello && lines.Scan() {
		hello = lines.Text() == "event: hello"
	}
	if !hello {
		t.Fatal("event stream ended before its hello event")
	}

	revoke, _ := http.NewRequest(http.MethodDelete, admin.URL+"/admin/tokens/laptop", nil)
	resp, err = http.DefaultClient.Do(revoke)
	if err != nil {
		t.Fatal(err)
	}
	var revoked RevokedToken
	json.NewDecoder(resp.Body).Decode(&revoked)
	resp.Body.Close()
	if revoked.ClosedStreams != 1 {
		t.Errorf("closed %d streams, want 1", revoked.ClosedStreams)
	}

	ended := make(chan struct{})
	go func() {
		for lines.Scan() {
		}
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Error("event stream stayed open after revoking its token")
	}
	if got := get(created.Token); got != http.StatusUnauthorized {
		t.Errorf("revoked token: %d", got)
	}
}
