package config

import (
	"errors"
	"net"
	"testing"
)

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
