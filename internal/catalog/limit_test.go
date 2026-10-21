package catalog

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/httpx"
)

// oversized writes a body just over the limit, enough to trip the check
// without keeping a second copy of it around.
func oversized(w http.ResponseWriter) {
	w.Write([]byte(`{"status":"success","data":"`))
	w.Write(bytes.Repeat([]byte("x"), tvdbMaxResponse+1))
}

// TestTVDBResponseLimit checks that a TVDB answer larger than the limit
// fails with a clear error instead of decoding an unbounded body.
func TestTVDBResponseLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		oversized(w)
	})
	mux.HandleFunc("GET /series/1/extended", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		oversized(w)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client, err := httpx.New("test", "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := NewTVDB(srv.URL, "key", "", client)

	if _, err := c.login(context.Background()); err == nil || !strings.Contains(err.Error(), "TVDB login: response over 16 MiB") {
		t.Fatalf("login over the limit: %v", err)
	}
	c.token, c.tokenAt = "tok", time.Now()
	var out tvdbSeries
	if _, err := c.get(context.Background(), "/series/1/extended", &out); err == nil || !strings.Contains(err.Error(), "TVDB /series/1/extended: response over 16 MiB") {
		t.Fatalf("get over the limit: %v", err)
	}
}

// TestXEMResponseLimit checks the same for TheXEM's mappings.
func TestXEMResponseLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /map/all", func(w http.ResponseWriter, r *http.Request) {
		oversized(w)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client, err := httpx.New("test", "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := NewXEM(srv.URL, client)

	_, err = c.mappings(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("TheXEM /map/all: response over %d MiB", xemMaxResponse>>20)) {
		t.Fatalf("mappings over the limit: %v", err)
	}
}
