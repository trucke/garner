package client

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trucke/garner/internal/connect"
	"github.com/trucke/garner/internal/tlsident"
)

// pinnedServer serves like the real server: Garner's own key, no CA.
func pinnedServer(t *testing.T, h http.Handler) (url, pin string) {
	t.Helper()
	id, err := tlsident.LoadOrCreate(t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = tls.NewListener(srv.Listener, &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: id.GetCertificate})
	srv.Start()
	t.Cleanup(srv.Close)
	return "https://" + srv.Listener.Addr().String(), id.Pin()
}

// TestTokenOnlyToVerifiedServer checks that a server with another key is
// reported with its pin and never receives the token, while the pinned one
// does.
func TestTokenOnlyToVerifiedServer(t *testing.T) {
	var sawToken atomic.Bool
	url, pin := pinnedServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawToken.Store(true)
		}
		w.Write([]byte(`{"version":"1.2.3","token":"laptop"}`))
	}))
	ctx := context.Background()

	other, err := tlsident.LoadOrCreate(t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := New(connect.Code{URL: url, Token: "secret", Pin: other.Pin()})
	for name, call := range map[string]func() error{
		"Check":  func() error { return wrong.Check(ctx) },
		"Status": func() error { _, err := wrong.Status(ctx); return err },
	} {
		var untrusted *UntrustedError
		if err := call(); !errors.As(err, &untrusted) || untrusted.ServerPin != pin {
			t.Fatalf("%s with another key's pin: %v, want an untrusted error naming the server's pin", name, err)
		}
	}
	if sawToken.Load() {
		t.Fatal("the token reached a server whose key did not match the pin")
	}

	right, _ := New(connect.Code{URL: url, Token: "secret", Pin: pin})
	if err := right.Check(ctx); err != nil {
		t.Fatalf("Check with the right pin: %v", err)
	}
	st, err := right.Status(ctx)
	if err != nil || st.Version != "1.2.3" || st.Token != "laptop" || !sawToken.Load() {
		t.Fatalf("Status with the right pin: %+v, %v", st, err)
	}
}

// TestPlainHTTPStaysOnThisMachine checks that plain HTTP to another machine
// is refused before any connection is made.
func TestPlainHTTPStaysOnThisMachine(t *testing.T) {
	c, _ := New(connect.Code{URL: "http://192.0.2.1:7979", Token: "secret"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for name, call := range map[string]func() error{
		"Check":  func() error { return c.Check(ctx) },
		"Status": func() error { _, err := c.Status(ctx); return err },
	} {
		var untrusted *UntrustedError
		if err := call(); !errors.As(err, &untrusted) || !errors.Is(err, ErrNotLoopback) {
			t.Fatalf("%s over plain HTTP to another machine: %v", name, err)
		}
	}

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"1.2.3"}`))
	}))
	defer local.Close()
	c, _ = New(connect.Code{URL: local.URL, Token: "secret"})
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("plain HTTP to this machine: %v", err)
	}
}

// TestNoRedirects checks that a redirect is reported, not followed, so the
// token goes nowhere else.
func TestNoRedirects(t *testing.T) {
	var followed atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/elsewhere", func(http.ResponseWriter, *http.Request) { followed.Store(true) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, _ := New(connect.Code{URL: srv.URL, Token: "secret"})
	var serverErr *ServerError
	if _, err := c.Status(context.Background()); !errors.As(err, &serverErr) || followed.Load() {
		t.Fatalf("redirect: %v, followed %v", err, followed.Load())
	}
}
