// Package server runs Garner's HTTP API for clients and the local admin
// socket the command line uses.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/catalog"
	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/download"
	"github.com/trucke/garner/internal/httpx"
	"github.com/trucke/garner/internal/importer"
	"github.com/trucke/garner/internal/jellyfin"
	"github.com/trucke/garner/internal/monitor"
	"github.com/trucke/garner/internal/prowlarr"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/sabnzbd"
	"github.com/trucke/garner/internal/search"
	"github.com/trucke/garner/internal/selection"
	"github.com/trucke/garner/internal/store"
	"github.com/trucke/garner/internal/tlsident"
)

// TLS modes reported in the status.
const (
	TLSOff   = "off"
	TLSFiles = "certificate files"
	TLSOwn   = "own certificate"
	TLSProxy = "at the proxy"
)

// Server serves the client API and the admin socket.
type Server struct {
	cfg       *config.Config
	store     *store.Store
	catalog   *catalog.Catalog  // nil without TVDB access
	search    *search.Service   // nil without Prowlarr
	downloads *download.Service // nil without SABnzbd
	imports   *importer.Service // nil without library folders or ffprobe
	monitor   *monitor.Service  // nil without searching and downloading; runs show searches even when the monitor is off
	profiles  selection.Profiles
	// The services' clients, for the status checks; nil when not set up.
	prowlarr *prowlarr.Client
	sabnzbd  *sabnzbd.Client
	jellyfin *jellyfin.Client
	log      *slog.Logger
	version  string
	events   *broker
	tlsMode  string
	pin      string // set for TLSOwn
	started  time.Time
	deps     Deps
}

// Deps stand in for what Garner reaches outside its own code, so the
// simulation can run it against a simulated world. A zero field means the
// real one.
type Deps struct {
	// Now tells the time, and Sleep waits on the same clock.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// Transport carries every request to Prowlarr, SABnzbd and Jellyfin.
	Transport http.RoundTripper
	// Prober reads video files instead of ffprobe.
	Prober importer.Prober
	// Rename moves files into the library.
	Rename func(oldpath, newpath string) error
	// Before runs before each change to a file or folder; see
	// guard.Options.Before.
	Before func(op, path string)
}

// Run opens the database and serves until ctx is cancelled.
func Run(ctx context.Context, cfg *config.Config, version string, log *slog.Logger) error {
	st, err := store.Open(ctx, cfg.DataDir, log)
	if err != nil {
		return err
	}
	defer st.Close()

	s, err := New(cfg, st, version, Deps{}, log)
	if err != nil {
		return err
	}
	tlsConf, err := s.tlsConfig()
	if err != nil {
		return err
	}
	errorLog := slog.NewLogLogger(log.Handler(), slog.LevelWarn)
	public := &http.Server{
		Handler:           s.Routes(),
		TLSConfig:         tlsConf,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          errorLog,
	}
	admin := &http.Server{Handler: s.AdminRoutes(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: errorLog}

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Server.Listen, err)
	}
	if tlsConf != nil {
		ln = tls.NewListener(ln, tlsConf)
	}
	sock, err := SocketPath(cfg.DataDir)
	if err != nil {
		return err
	}
	// The database lock already guarantees no other server runs here, so a
	// leftover socket is stale.
	os.Remove(sock)
	adminLn, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("admin socket: %w", err)
	}
	defer os.Remove(sock)
	if err := os.Chmod(sock, 0o600); err != nil {
		return err
	}

	attrs := []any{"url", cfg.Server.URL, "tls", s.tlsMode}
	if s.pin != "" {
		attrs = append(attrs, "pin", s.pin)
	}
	log.Info("Garner is running", attrs...)

	if s.catalog != nil {
		go s.catalog.Run(ctx)
	}
	if s.downloads != nil {
		go s.downloads.Run(ctx)
	}
	if s.imports != nil {
		go s.imports.Run(ctx)
	}
	if s.monitor != nil {
		go s.monitor.Run(ctx)
	}
	errc := make(chan error, 2)
	go func() { errc <- public.Serve(ln) }()
	go func() { errc <- admin.Serve(adminLn) }()
	select {
	case <-ctx.Done():
		err = nil
	case err = <-errc:
	}
	s.events.closeAll()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	public.Shutdown(shutdown)
	admin.Shutdown(shutdown)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	log.Info("Garner stopped")
	return err
}

// New sets Garner's services up on an open database, without serving or
// starting their loops: Run does both, the simulation drives the steps
// itself.
func New(cfg *config.Config, st *store.Store, version string, deps Deps, log *slog.Logger) (*Server, error) {
	profiles, err := selection.NewProfiles(cfg.Profiles)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, store: st, log: log, version: version, events: newBroker(), profiles: profiles, deps: deps}
	s.started = s.now()
	for _, setup := range []func() error{s.setupCatalog, s.setupSearch, s.setupDownloads, s.setupImports} {
		if err := setup(); err != nil {
			return nil, err
		}
	}
	s.setupMonitor()
	return s, nil
}

// now is the time on Garner's clock: the real one unless Deps set another.
func (s *Server) now() time.Time {
	if s.deps.Now == nil {
		return time.Now()
	}
	return s.deps.Now()
}

// httpClient is the client for one of the services Garner uses.
func (s *Server) httpClient(caFile string, timeout time.Duration) (*http.Client, error) {
	if s.deps.Transport != nil {
		return &http.Client{Transport: s.deps.Transport, Timeout: timeout}, nil
	}
	return httpx.New("Garner/"+s.version, caFile, timeout)
}

// PollDownloads, WorkImports and StepMonitor run one step of a background
// loop, for the simulation. Each does nothing when its service is not set
// up.
func (s *Server) PollDownloads(ctx context.Context) {
	if s.downloads != nil {
		s.downloads.Poll(ctx)
	}
}

func (s *Server) WorkImports(ctx context.Context) {
	if s.imports != nil {
		s.imports.Work(ctx)
	}
}

func (s *Server) StepMonitor(ctx context.Context) {
	if s.monitor != nil {
		s.monitor.Step(ctx)
	}
}

func (s *Server) setupCatalog() error {
	if s.cfg.TVDB.APIKey == "" {
		s.log.Warn("TVDB_API_KEY is not set; the catalogue is unavailable")
		return nil
	}
	client, err := s.httpClient("", 30*time.Second)
	if err != nil {
		return err
	}
	tvdb := catalog.NewTVDB(catalog.TVDBBaseURL, s.cfg.TVDB.APIKey.Value(), s.cfg.TVDB.PIN.Value(), client)
	c, err := catalog.New(s.store, tvdb, catalog.NewXEM(catalog.XEMBaseURL, client), s.log)
	if err != nil {
		return err
	}
	c.Profiles = s.profiles
	c.OnChange = func(id int64) {
		s.events.publish(Event{Type: "series.updated", Data: map[string]int64{"id": id}})
		// New air dates may make episodes due.
		s.wakeMonitor()
	}
	s.catalog = c
	return nil
}

// monitorOff says why the monitor does not run, to follow "The monitor
// is"; empty when it runs.
func (s *Server) monitorOff() string {
	switch {
	case !s.cfg.Monitor.Enabled:
		return "turned off in garner.toml"
	case s.search == nil || s.downloads == nil:
		return "off until Prowlarr and SABnzbd are set up"
	}
	return ""
}

// schedule is when the monitor searches episodes on its own; nil when it
// does not run.
func (s *Server) schedule() *store.Schedule {
	if s.monitorOff() != "" {
		return nil
	}
	m := s.cfg.Monitor
	return &store.Schedule{
		Recent:        m.Recent(),
		MissingRecent: time.Duration(m.MissingRecent), MissingOlder: time.Duration(m.MissingOlder),
		Upgrades:      m.Upgrades,
		UpgradeRecent: time.Duration(m.UpgradeRecent), UpgradeOlder: time.Duration(m.UpgradeOlder),
		Best: func(profile string) string { return s.profiles.Named(profile).Tiers[0].String() },
	}
}

func (s *Server) setupMonitor() {
	if off := s.monitorOff(); off != "" {
		s.log.Info("the monitor is off: Garner searches and downloads only when asked", "reason", off)
	}
	if s.search == nil || s.downloads == nil {
		return
	}
	var rules func(int64) map[release.Service]string
	if s.catalog != nil {
		rules = s.catalog.ServiceOrders
	}
	s.monitor = monitor.New(s.store, s.search, s.downloads, s.schedule(), rules, s.log)
	s.monitor.Now = s.now
	s.monitor.OnShowProgress = func(p api.ShowProgress) { s.events.publish(Event{Type: "series.search", Data: p}) }
}

// wakeMonitor has the monitor look now, when it runs.
func (s *Server) wakeMonitor() {
	if s.monitor != nil {
		s.monitor.Wake()
	}
}

func (s *Server) tlsConfig() (*tls.Config, error) {
	srv := s.cfg.Server
	host, _, _ := net.SplitHostPort(srv.Listen)
	switch {
	case srv.Proxied:
		// The proxy holds the certificate; only it may reach this listener.
		s.tlsMode = TLSProxy
		return nil, nil
	case srv.TLSCert != "":
		cert, err := tls.LoadX509KeyPair(srv.TLSCert, srv.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("load certificate: %w", err)
		}
		s.tlsMode = TLSFiles
		return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}, nil
	case config.IsLoopback(host):
		s.tlsMode = TLSOff
		return nil, nil
	}
	u, err := url.Parse(srv.URL)
	if err != nil {
		return nil, err
	}
	id, err := tlsident.LoadOrCreate(filepath.Join(s.cfg.DataDir, "tls"), []string{u.Hostname()})
	if err != nil {
		return nil, fmt.Errorf("own certificate: %w", err)
	}
	s.tlsMode, s.pin = TLSOwn, id.Pin()
	return &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: id.GetCertificate}, nil
}

// SocketPath is the admin socket in dataDir.
func SocketPath(dataDir string) (string, error) {
	p := filepath.Join(dataDir, "garner.sock")
	// Unix socket paths are limited to about 104 bytes on macOS.
	if len(p) > 100 {
		return "", fmt.Errorf("data directory path is too long for the admin socket (%d bytes): %s", len(p), dataDir)
	}
	return p, nil
}
