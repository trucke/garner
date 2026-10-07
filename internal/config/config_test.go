package config

import "testing"

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
