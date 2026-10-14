package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trucke/garner/internal/connect"
)

// TestWatch checks that events arrive, that a dropped stream is reported,
// and that a revoked token ends the stream instead of retrying forever.
func TestWatch(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "retry: 3000\n\n: ping\n\nevent: download.updated\ndata: {\"id\":6}\n\n")
	}))
	defer srv.Close()

	c, _ := New(connect.Code{URL: srv.URL, Token: "secret"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got []string
	for e := range c.Watch(ctx) {
		got = append(got, e.Type+string(e.Data))
	}
	want := []string{EventConnected, `download.updated{"id":6}`, EventDisconnected, EventUnauthorized}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events %q, want %q", got, want)
	}
}
