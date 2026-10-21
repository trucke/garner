package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// TestSecretsStayHidden formats a configuration that holds secrets the
// ways logs and error reports do, which users paste into issues: no secret
// shows.
func TestSecretsStayHidden(t *testing.T) {
	secrets := []string{"tvdb-key-1", "tvdb-pin-2", "prowlarr-key-3", "sabnzbd-key-4", "jellyfin-key-5"}
	cfg := &Config{
		TVDB:     TVDB{APIKey: Secret(secrets[0]), PIN: Secret(secrets[1])},
		Prowlarr: Service{URL: "http://prowlarr:9696", APIKey: Secret(secrets[2])},
		SABnzbd:  SABnzbd{Service: Service{URL: "http://sabnzbd:8080", APIKey: Secret(secrets[3])}},
		Jellyfin: Service{URL: "http://jellyfin:8096", APIKey: Secret(secrets[4])},
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "%v %+v %#v %v %s", cfg, *cfg, *cfg, cfg.TVDB.APIKey, cfg.SABnzbd.APIKey)
	fmt.Fprint(&out, fmt.Errorf("wrapped: %v", cfg.Jellyfin))
	for _, h := range []slog.Handler{slog.NewTextHandler(&out, nil), slog.NewJSONHandler(&out, nil)} {
		slog.New(h).Info("config", "config", cfg, "key", cfg.Prowlarr.APIKey, "tvdb", cfg.TVDB)
	}
	if err := json.NewEncoder(&out).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if strings.Contains(out.String(), s) {
			t.Errorf("%s shows in:\n%s", s, out.String())
		}
	}
	if cfg.TVDB.APIKey.Value() != secrets[0] {
		t.Error("Value does not return the secret itself")
	}
}

func TestPathMap(t *testing.T) {
	m := PathMap{"/data/usenet": "/Volumes/media/usenet", "/data": "/Volumes/media"}
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"/data/usenet/complete/tv/Show.S01E01", "/Volumes/media/usenet/complete/tv/Show.S01E01", true},
		{"/data/library/tv", "/Volumes/media/library/tv", true},
		{"/data", "/Volumes/media", true},
		{"/datasets/x", "", false}, // a prefix only counts at a folder boundary
		{"/srv/other", "", false},
	} {
		got, ok := m.Local(tc.in)
		if ok != tc.ok || ok && got != tc.want {
			t.Errorf("Local(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	if got, ok := m.Remote("/Volumes/media/library/tv/Show/x.mkv"); !ok || got != "/data/library/tv/Show/x.mkv" {
		t.Errorf("Remote = %q, %v", got, ok)
	}
	if got, ok := (PathMap{}).Local("/data/x/../y"); !ok || got != "/data/y" {
		t.Errorf("empty map: %q, %v; want the path itself", got, ok)
	}
}

func TestStaysOnHost(t *testing.T) {
	own := func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
			&net.IPNet{IP: net.ParseIP("192.168.1.20"), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.ParseIP("fd00::32"), Mask: net.CIDRMask(64, 128)},
		}, nil
	}
	for host, want := range map[string]bool{
		"localhost":    true,
		"127.0.0.1":    true,
		"192.168.1.20": true, // this machine's LAN address
		"fd00::32":     true,
		"192.168.1.30": false, // another machine on the same network
		"server.lan":   false, // names are not resolved
	} {
		if got := staysOnHost(host, own); got != want {
			t.Errorf("staysOnHost(%q) = %v, want %v", host, got, want)
		}
	}
	failing := func() ([]net.Addr, error) { return nil, errors.New("no interfaces") }
	if staysOnHost("192.168.1.20", failing) {
		t.Error("an unknown interface list must count as remote")
	}
}

// TestProxied accepts a proxy's https:// address in front of a plain
// listener beyond loopback, and refuses what would not work behind it.
func TestProxied(t *testing.T) {
	for _, tc := range []struct {
		name string
		srv  Server
		ok   bool
	}{
		{"behind a proxy", Server{Listen: "192.168.1.20:7979", URL: "https://garner.example.com", Proxied: true}, true},
		{"no proxy address", Server{Listen: "192.168.1.20:7979", Proxied: true}, false},
		{"plain proxy address", Server{Listen: "192.168.1.20:7979", URL: "http://garner.example.com", Proxied: true}, false},
		{"own certificate too", Server{Listen: "192.168.1.20:7979", URL: "https://garner.example.com", Proxied: true,
			TLSCert: "/c.pem", TLSKey: "/k.pem"}, false},
		{"not proxied, plain beyond loopback", Server{Listen: "192.168.1.20:7979", URL: "http://garner.example.com"}, false},
	} {
		c := Config{Server: tc.srv}
		if err := c.validate(); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour,
		"": 0, "0h": 0, "-1d": 0, "6": 0, "1.5h": 0, "90m": 0, "6dh": 0, "h": 0,
	} {
		var d Duration
		err := d.UnmarshalText([]byte(in))
		if want == 0 && err == nil || want != 0 && (err != nil || time.Duration(d) != want) {
			t.Errorf("%q: %v, %v; want %v", in, time.Duration(d), err, want)
		}
	}
}
