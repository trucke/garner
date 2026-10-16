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

	"github.com/trucke/garner/internal/catalog"
	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/download"
	"github.com/trucke/garner/internal/httpx"
	"github.com/trucke/garner/internal/importer"
	"github.com/trucke/garner/internal/jellyfin"
	"github.com/trucke/garner/internal/prowlarr"
	"github.com/trucke/garner/internal/sabnzbd"
	"github.com/trucke/garner/internal/search"
	"github.com/trucke/garner/internal/store"
	"github.com/trucke/garner/internal/tlsident"
)

// TLS modes reported in the status.
const (
	TLSOff   = "off"
	TLSFiles = "certificate files"
	TLSOwn   = "own certificate"
)

// Server serves the client API and the admin socket.
type Server struct {
	cfg       *config.Config
	store     *store.Store
	catalog   *catalog.Catalog  // nil without TVDB access
	search    *search.Service   // nil without Prowlarr
	downloads *download.Service // nil without SABnzbd
	imports   *importer.Service // nil without library folders or ffprobe
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
}

// Run opens the database and serves until ctx is cancelled.
func Run(ctx context.Context, cfg *config.Config, version string, log *slog.Logger) error {
	st, err := store.Open(ctx, cfg.DataDir, log)
	if err != nil {
		return err
	}
	defer st.Close()

	s := &Server{cfg: cfg, store: st, log: log, version: version, events: newBroker(), started: time.Now()}
	if err := s.setupCatalog(); err != nil {
		return err
	}
	if err := s.setupSearch(); err != nil {
		return err
	}
	if err := s.setupDownloads(); err != nil {
		return err
	}
	if err := s.setupImports(); err != nil {
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

func (s *Server) setupCatalog() error {
	if s.cfg.TVDB.APIKey == "" {
		s.log.Warn("TVDB_API_KEY is not set; the catalogue is unavailable")
		return nil
	}
	client, err := httpx.New("Garner/"+s.version, "", 30*time.Second)
	if err != nil {
		return err
	}
	tvdb := catalog.NewTVDB(catalog.TVDBBaseURL, s.cfg.TVDB.APIKey.Value(), s.cfg.TVDB.PIN.Value(), client)
	c, err := catalog.New(s.store, tvdb, catalog.NewXEM(catalog.XEMBaseURL, client), s.log)
	if err != nil {
		return err
	}
	c.OnChange = func(id int64) { s.events.publish(Event{Type: "series.updated", Data: map[string]int64{"id": id}}) }
	s.catalog = c
	return nil
}

func (s *Server) tlsConfig() (*tls.Config, error) {
	srv := s.cfg.Server
	host, _, _ := net.SplitHostPort(srv.Listen)
	switch {
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
