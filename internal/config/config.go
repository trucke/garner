// Package config loads Garner's settings from a TOML file and its secrets
// from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultPath is used when neither --config nor GARNER_CONFIG names a file.
const DefaultPath = "garner.toml"

// Config holds all settings. Secrets never come from the file.
type Config struct {
	DataDir string `toml:"data_dir"`
	// FFprobe is the ffprobe program that checks downloaded files.
	FFprobe  string  `toml:"ffprobe"`
	Server   Server  `toml:"server"`
	Library  Library `toml:"library"`
	TVDB     TVDB    `toml:"tvdb"`
	Prowlarr Service `toml:"prowlarr"`
	SABnzbd  SABnzbd `toml:"sabnzbd"`
	Jellyfin Service `toml:"jellyfin"`
	Monitor  Monitor `toml:"monitor"`
	// Profiles change the built-in profiles' rules; package selection
	// checks them.
	Profiles Profiles `toml:"profiles"`
}

// Monitor is the background search: it searches the episodes monitored
// shows miss and downloads the best release it accepts, and looks for
// better releases of files below their profile's best tier.
type Monitor struct {
	// Enabled turns it on for every show; each show's own mode then says
	// which of its episodes it gets.
	Enabled bool `toml:"enabled"`
	// RecentDays is how long after airing an episode counts as recent:
	// recent ones are searched first and more often.
	RecentDays int `toml:"recent_days"`
	// MissingRecent and MissingOlder are the waits between searches for
	// an episode not in the library.
	MissingRecent Duration `toml:"missing_recent"`
	MissingOlder  Duration `toml:"missing_older"`
	// Upgrades turns on the search for better releases; UpgradeRecent and
	// UpgradeOlder are the waits between those searches.
	Upgrades      bool     `toml:"upgrades"`
	UpgradeRecent Duration `toml:"upgrade_recent"`
	UpgradeOlder  Duration `toml:"upgrade_older"`
}

// Profiles holds the settings of the two profiles shows choose from.
type Profiles struct {
	TV    Profile `toml:"tv"`
	Anime Profile `toml:"anime"`
}

// Profile changes a profile's rules; unset fields keep the built-in ones.
type Profile struct {
	// Tiers are the acceptable audio language and resolution pairs, best
	// first, such as "en 1080p".
	Tiers []string `toml:"tiers"`
	// PreferDual ranks releases with the tier's language plus the show's
	// original audio above the tier's language alone.
	PreferDual *bool `toml:"prefer_dual"`
}

// Recent is how long after airing an episode counts as recent.
func (m Monitor) Recent() time.Duration { return time.Duration(m.RecentDays) * 24 * time.Hour }

// Duration is a whole number of hours or days, such as "6h" or "7d".
type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	s := string(b)
	units := map[byte]time.Duration{'h': time.Hour, 'd': 24 * time.Hour}
	var unit time.Duration
	n, err := 0, error(nil)
	if s != "" {
		unit = units[s[len(s)-1]]
		n, err = strconv.Atoi(s[:len(s)-1])
	}
	if unit == 0 || err != nil || n < 1 {
		return fmt.Errorf("%q is not a duration such as \"6h\" or \"7d\"", s)
	}
	*d = Duration(time.Duration(n) * unit)
	return nil
}

// Server configures how clients reach Garner.
type Server struct {
	// Listen is the TCP address to listen on.
	Listen string `toml:"listen"`
	// URL is the address clients use. Required unless Listen is loopback.
	URL string `toml:"url"`
	// TLSCert and TLSKey name certificate files. Without them, Garner
	// creates its own certificate when it listens beyond loopback.
	TLSCert string `toml:"tls_cert"`
	TLSKey  string `toml:"tls_key"`
	// Proxied means a reverse proxy serves URL with a publicly trusted
	// certificate and forwards to Listen. Garner then serves plain HTTP,
	// which only the proxy may reach, and its connection codes let clients
	// verify the proxy's certificate the usual way.
	Proxied bool `toml:"proxied"`
}

// Library names the folders shows are imported into, as Garner reaches
// them. Garner creates folders below them but never the folders themselves,
// so an unmounted share fails instead of filling the local disk.
type Library struct {
	TV    string `toml:"tv"`
	Anime string `toml:"anime"`
}

// TVDB holds catalogue access. Secrets: TVDB_API_KEY, TVDB_PIN (optional).
type TVDB struct {
	APIKey Secret `toml:"-"`
	PIN    Secret `toml:"-"`
}

// Service is an HTTP service Garner talks to.
type Service struct {
	URL string `toml:"url"`
	// CAFile trusts an extra certificate authority for an https:// URL.
	CAFile string `toml:"ca_file"`
	// Paths translates folders as the service reports them to where Garner
	// reaches them, such as "/data/usenet" to "/Volumes/media/usenet".
	Paths  PathMap `toml:"paths"`
	APIKey Secret  `toml:"-"`
}

// PathMap maps folders as another host sees them (keys) to where Garner
// reaches them (values), like Sonarr's remote path mappings. An empty map
// means both see the same paths.
type PathMap map[string]string

// Local translates a path the service reported to Garner's view.
func (m PathMap) Local(p string) (string, bool) { return m.translate(p, false) }

// Remote translates a path in Garner's view to the service's view.
func (m PathMap) Remote(p string) (string, bool) { return m.translate(p, true) }

func (m PathMap) translate(p string, reverse bool) (string, bool) {
	p = path.Clean(p)
	if len(m) == 0 {
		return p, true
	}
	best, bestTo := "", ""
	for remote, local := range m {
		from, to := path.Clean(remote), path.Clean(local)
		if reverse {
			from, to = to, from
		}
		if (p == from || strings.HasPrefix(p, from+"/")) && len(from) > len(best) {
			best, bestTo = from, to
		}
	}
	if best == "" {
		return p, false
	}
	return bestTo + strings.TrimPrefix(p, best), true
}

// SABnzbd is the download client.
type SABnzbd struct {
	Service
	Category string `toml:"category"`
}

// Secret hides its value from logs and formatted output.
type Secret string

func (s Secret) String() string               { return redacted(s) }
func (s Secret) GoString() string             { return redacted(s) }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(redacted(s)) }
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted(s)), nil }

// Value returns the secret itself.
func (s Secret) Value() string { return string(s) }

func redacted(s Secret) string {
	if s == "" {
		return ""
	}
	return "[redacted]"
}

// Load reads path and the secrets in the environment. A missing file is
// fine only for the default path; then defaults apply.
func Load(path string, explicit bool) (*Config, error) {
	cfg := &Config{
		DataDir: "data",
		FFprobe: "ffprobe",
		Server:  Server{Listen: "127.0.0.1:7979"},
		SABnzbd: SABnzbd{Category: "tv"},
		Monitor: Monitor{
			Enabled: true, RecentDays: 14,
			MissingRecent: Duration(6 * time.Hour), MissingOlder: Duration(24 * time.Hour),
			Upgrades: true, UpgradeRecent: Duration(24 * time.Hour), UpgradeOlder: Duration(7 * 24 * time.Hour),
		},
	}
	base := "."
	meta, err := toml.DecodeFile(path, cfg)
	switch {
	case errors.Is(err, os.ErrNotExist) && !explicit:
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	default:
		if undecoded := meta.Undecoded(); len(undecoded) > 0 {
			keys := make([]string, len(undecoded))
			for i, k := range undecoded {
				keys[i] = k.String()
			}
			return nil, fmt.Errorf("%s: unknown settings: %s", path, strings.Join(keys, ", "))
		}
		base = filepath.Dir(path)
	}
	if !filepath.IsAbs(cfg.DataDir) {
		cfg.DataDir = filepath.Join(base, cfg.DataDir)
	}
	if cfg.DataDir, err = filepath.Abs(cfg.DataDir); err != nil {
		return nil, err
	}

	secrets := []struct {
		name string
		dst  *Secret
	}{
		{"TVDB_API_KEY", &cfg.TVDB.APIKey},
		{"TVDB_PIN", &cfg.TVDB.PIN},
		{"PROWLARR_API_KEY", &cfg.Prowlarr.APIKey},
		{"SABNZBD_API_KEY", &cfg.SABnzbd.APIKey},
		{"JELLYFIN_API_KEY", &cfg.Jellyfin.APIKey},
	}
	for _, s := range secrets {
		if *s.dst, err = secret(s.name); err != nil {
			return nil, err
		}
	}
	return cfg, cfg.validate()
}

// secret reads NAME, or the file named by NAME_FILE (systemd credentials,
// Infisical Agent output).
func secret(name string) (Secret, error) {
	value, file := os.Getenv(name), os.Getenv(name+"_FILE")
	switch {
	case value != "" && file != "":
		return "", fmt.Errorf("set %s or %s_FILE, not both", name, name)
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return Secret(strings.TrimSpace(string(b))), nil
	}
	return Secret(strings.TrimSpace(value)), nil
}

func (c *Config) validate() error {
	host, _, err := net.SplitHostPort(c.Server.Listen)
	if err != nil {
		return fmt.Errorf("server.listen: %w", err)
	}
	if (c.Server.TLSCert == "") != (c.Server.TLSKey == "") {
		return errors.New("server.tls_cert and server.tls_key must be set together")
	}
	if c.Server.Proxied {
		if c.Server.TLSCert != "" {
			return errors.New("server.proxied: the proxy holds the certificate; remove server.tls_cert and server.tls_key")
		}
		if u, err := url.Parse(c.Server.URL); err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("server.proxied needs server.url: the proxy's https:// address, such as https://garner.example.com")
		}
	} else if c.Server.URL == "" {
		if !IsLoopback(host) {
			return errors.New("server.url is required when Garner listens beyond loopback: set the address clients use, such as https://garner.home.lan:7979")
		}
		scheme := "http"
		if c.Server.TLSCert != "" {
			scheme = "https"
		}
		c.Server.URL = scheme + "://" + c.Server.Listen
	}
	u, err := url.Parse(c.Server.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("server.url %q must be an http:// or https:// address", c.Server.URL)
	}
	if u.Scheme == "http" && (!IsLoopback(host) || c.Server.TLSCert != "") {
		return errors.New("server.url must use https:// when Garner listens beyond loopback or has a certificate")
	}
	if u.Scheme == "https" && IsLoopback(host) && c.Server.TLSCert == "" && !c.Server.Proxied {
		return errors.New("server.url uses https://, but Garner listens on loopback without a certificate; set server.tls_cert and server.tls_key or use http://")
	}
	if c.Monitor.RecentDays < 0 {
		return errors.New("monitor.recent_days must not be negative")
	}
	for name, dir := range map[string]string{"library.tv": c.Library.TV, "library.anime": c.Library.Anime} {
		if dir != "" && !filepath.IsAbs(dir) {
			return fmt.Errorf("%s %q must be an absolute path", name, dir)
		}
	}
	for name, svc := range c.services() {
		for remote, local := range svc.Paths {
			if !path.IsAbs(remote) || !filepath.IsAbs(local) {
				return fmt.Errorf("%s.paths: %q = %q must map an absolute path to an absolute path", name, remote, local)
			}
		}
		if svc.URL == "" {
			continue
		}
		u, err := url.Parse(svc.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%s.url %q must be an http:// or https:// address", name, svc.URL)
		}
		if svc.CAFile != "" && u.Scheme != "https" {
			return fmt.Errorf("%s.ca_file only applies to an https:// url", name)
		}
	}
	return nil
}

func (c *Config) services() map[string]Service {
	return map[string]Service{"prowlarr": c.Prowlarr, "sabnzbd": c.SABnzbd.Service, "jellyfin": c.Jellyfin}
}

// Warnings lists risky but allowed settings, such as API keys sent over
// plain HTTP to another machine.
func (c *Config) Warnings() []string {
	var out []string
	for _, name := range []string{"prowlarr", "sabnzbd", "jellyfin"} {
		svc := c.services()[name]
		u, err := url.Parse(svc.URL)
		if err != nil || u.Scheme != "http" || staysOnHost(u.Hostname(), net.InterfaceAddrs) {
			continue
		}
		out = append(out, fmt.Sprintf("%s is reached over plain HTTP at %s; its API key crosses the network unencrypted", name, u.Host))
	}
	return out
}

// staysOnHost reports whether traffic to host stays on this machine: a
// loopback name or address, or an address of one of its own network
// interfaces, as when Garner shares the host's network with the service.
// Other host names are not resolved, so they count as remote.
func staysOnHost(host string, own func() ([]net.Addr, error)) bool {
	if IsLoopback(host) {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	addrs, err := own()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// IsLoopback reports whether host is a loopback name or address.
func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
