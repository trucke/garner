package httpx

import (
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCAFile(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the rejected handshake below
	srv.StartTLS()
	defer srv.Close()

	plain, err := New("test", "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Get(srv.URL); err == nil {
		t.Error("trusted a private certificate without ca_file")
	}

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	withCA, err := New("test", caFile, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := withCA.Get(srv.URL)
	if err != nil {
		t.Fatalf("ca_file not trusted: %v", err)
	}
	resp.Body.Close()
}

func TestRedact(t *testing.T) {
	link := "http://user:pass@prowlarr:9696/1/download?apikey=SECRET&link=ENCRYPTED&file=Show.S01E01"
	got := RedactURL(link)
	if strings.Contains(got, "SECRET") || strings.Contains(got, "ENCRYPTED") || strings.Contains(got, "pass") || !strings.Contains(got, "file=Show.S01E01") {
		t.Errorf("RedactURL = %s", got)
	}
	if got := RedactText("Fetched " + link + "<br/>in 2 seconds"); strings.Contains(got, "SECRET") || !strings.HasSuffix(got, "<br/>in 2 seconds") {
		t.Errorf("RedactText = %s", got)
	}
	err := Redact(&url.Error{Op: "Get", URL: link, Err: errors.New("connection refused")})
	if strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("Redact = %v", err)
	}
}

func TestRedirects(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, other.URL+"/?apikey=secret", http.StatusFound)
		case "/here":
			http.Redirect(w, r, "/done", http.StatusFound)
		}
	}))
	defer srv.Close()

	client, err := New("test", "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(srv.URL + "/away"); err == nil || reached.Load() != 0 {
		t.Errorf("followed a redirect to another origin (err %v)", err)
	}
	resp, err := client.Get(srv.URL + "/here")
	if err != nil {
		t.Fatalf("same-origin redirect: %v", err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/done" {
		t.Errorf("ended at %s", resp.Request.URL)
	}
}
