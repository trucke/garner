package tlsident

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPinnedConnection checks that a client trusts the server only by its
// pin, and that reloading or reissuing the certificate keeps the pin.
func TestPinnedConnection(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreate(dir, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	// Serve like the real server: a TLS listener using only GetCertificate.
	// httptest's StartTLS would add its own certificate.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Listener = tls.NewListener(srv.Listener, &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: id.GetCertificate})
	srv.Start()
	defer srv.Close()
	url := "https://" + srv.Listener.Addr().String()

	get := func(pin string, host ...string) error {
		h := "127.0.0.1"
		if len(host) > 0 {
			h = host[0]
		}
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: ClientConfig(pin, h)}}
		resp, err := c.Get(url)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get(id.Pin()); err != nil {
		t.Fatalf("matching pin: %v", err)
	}
	if get(id.Pin(), "garner.example") == nil {
		t.Fatal("connection succeeded for a host the certificate does not cover")
	}
	other, err := LoadOrCreate(t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if get(other.Pin()) == nil {
		t.Fatal("connection succeeded with another key's pin")
	}

	// Different hosts force a new certificate; the key and pin stay.
	again, err := LoadOrCreate(dir, []string{"127.0.0.1", "garner.example"})
	if err != nil {
		t.Fatal(err)
	}
	if again.Pin() != id.Pin() {
		t.Fatal("reissuing the certificate changed the pin")
	}
	if again.cert.Leaf.VerifyHostname("garner.example") != nil {
		t.Fatal("reissued certificate does not cover the new host")
	}
}
