package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"github.com/trucke/garner/internal/connect"
	"github.com/trucke/garner/internal/profile"
	"github.com/trucke/garner/internal/store"
)

// The admin API is served only on the Unix socket in the data directory.
// Access is limited by file permissions to the user running Garner, so it
// needs no token.

// CreatedToken is returned once when a token is created.
type CreatedToken struct {
	Name  string `json:"name"`
	Token string `json:"token"`
	Code  string `json:"code"`
}

// TokenInfo describes a token without revealing it.
type TokenInfo struct {
	Name     string     `json:"name"`
	Last4    string     `json:"last4"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"last_used,omitempty"`
}

// RevokedToken reports a revocation.
type RevokedToken struct {
	Name          string `json:"name"`
	ClosedStreams int    `json:"closed_streams"`
}

// BackupInfo describes a database snapshot.
type BackupInfo struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Reason  string    `json:"reason"`
	Created time.Time `json:"created"`
}

// Status describes the running server.
type Status struct {
	Version   string    `json:"version"`
	URL       string    `json:"url"`
	TLS       string    `json:"tls"`
	Pin       string    `json:"pin,omitempty"`
	Started   time.Time `json:"started"`
	Database  string    `json:"database"`
	Size      int64     `json:"size"`
	Integrity string    `json:"integrity"`
	Tokens    int64     `json:"tokens"`
	Streams   int       `json:"streams"`
	// Work is what a restart would interrupt.
	Work       Work        `json:"work"`
	LastBackup *BackupInfo `json:"last_backup,omitempty"`
}

// AdminRoutes is the admin API.
func (s *Server) AdminRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/tokens", s.handleCreateToken)
	mux.HandleFunc("GET /admin/tokens", s.handleListTokens)
	mux.HandleFunc("DELETE /admin/tokens/{name}", s.handleRevokeToken)
	mux.HandleFunc("POST /admin/backup", s.handleBackup)
	mux.HandleFunc("GET /admin/status", s.handleAdminStatus)
	mux.HandleFunc("POST /admin/profiles", s.handleImportProfile)
	return mux
}

// ImportedProfile is what an import from the command line did.
type ImportedProfile struct {
	Name string `json:"name"`
	// Replaced means a profile of that name existed and took the new
	// settings; Shows are the shows that use it.
	Replaced bool     `json:"replaced"`
	Shows    []string `json:"shows"`
	Warnings []string `json:"warnings"`
}

// handleImportProfile saves a profile's text, as garnerd profile import
// reads it from standard input. A profile of the same name is replaced
// only when the request says so.
func (s *Server) handleImportProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text    string `json:"text"`
		Replace bool   `json:"replace"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	p, warnings, err := profile.Parse(req.Text)
	var pe *profile.Error
	if errors.As(err, &pe) {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "the profile has problems; nothing was imported", Problems: pe.Problems})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	out := ImportedProfile{Name: p.Name, Warnings: warnings}
	existing, err := s.store.ProfileByName(ctx, p.Name)
	switch {
	case err == nil && !req.Replace:
		writeError(w, http.StatusConflict, fmt.Sprintf("a profile named %q exists; add --replace to replace it", p.Name))
		return
	case err == nil:
		out.Replaced = true
		_, err = s.saveProfile(ctx, existing.ID, p)
	case errors.Is(err, store.ErrNoProfile):
		existing.ID, err = s.saveProfile(ctx, 0, p)
	}
	if err != nil {
		s.internalError(w, "import profile", err)
		return
	}
	if out.Shows, err = s.store.ProfileShowNames(ctx, existing.ID); err != nil {
		s.internalError(w, "import profile", err)
		return
	}
	s.log.Info("imported profile", "name", p.Name, "replaced", out.Replaced)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	plain, err := s.store.CreateToken(r.Context(), req.Name)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrExists) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	s.log.Info("created token", "name", req.Name)
	code := connect.Code{URL: s.cfg.Server.URL, Token: plain, Pin: s.pin}
	writeJSON(w, http.StatusCreated, CreatedToken{Name: req.Name, Token: plain, Code: code.String()})
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.Tokens(r.Context())
	if err != nil {
		s.internalError(w, "list tokens", err)
		return
	}
	out := make([]TokenInfo, len(tokens))
	for i, t := range tokens {
		out[i] = TokenInfo{Name: t.Name, Last4: t.Last4, Created: t.Created}
		if !t.LastUsed.IsZero() {
			out[i].LastUsed = &t.LastUsed
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	id, err := s.store.RevokeToken(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, "revoke token", err)
		return
	}
	closed := s.events.closeToken(id)
	s.log.Info("revoked token", "name", name, "closed_streams", closed)
	writeJSON(w, http.StatusOK, RevokedToken{Name: name, ClosedStreams: closed})
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	b, err := s.store.Backup(r.Context(), req.Path)
	if err != nil {
		s.log.Error("backup", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("backed up database", "path", b.Path, "size", b.Size)
	writeJSON(w, http.StatusOK, BackupInfo(b))
}

// Work counts what runs: downloads with SABnzbd, imports that check or
// move files, and searches of a show's missing episodes. Idle means a
// restart interrupts none of it: downloads do not count, as SABnzbd runs
// them and Garner follows them again after a restart, as it does an import
// waiting for Jellyfin.
type Work struct {
	Downloads int  `json:"downloads"`
	Imports   int  `json:"imports"`
	Searches  int  `json:"searches"`
	Idle      bool `json:"idle"`
}

func (s *Server) work(ctx context.Context) (Work, error) {
	downloads, err := s.store.ActiveDownloads(ctx)
	if err != nil {
		return Work{}, err
	}
	imports, err := s.store.DueImports(ctx)
	if err != nil {
		return Work{}, err
	}
	w := Work{Downloads: len(downloads)}
	for _, d := range imports {
		if d.Import.State == store.ImportPending || d.Import.State == store.ImportMoving {
			w.Imports++
		}
	}
	if s.monitor != nil {
		w.Searches = s.monitor.ShowSearches()
	}
	w.Idle = w.Imports+w.Searches == 0
	return w, nil
}

func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Status(r.Context())
	if err != nil {
		s.internalError(w, "status", err)
		return
	}
	work, err := s.work(r.Context())
	if err != nil {
		s.internalError(w, "status", err)
		return
	}
	out := Status{
		Version: s.version, URL: s.cfg.Server.URL, TLS: s.tlsMode, Pin: s.pin, Started: s.started,
		Database: st.Path, Size: st.Size, Integrity: st.Integrity, Tokens: st.Tokens, Streams: s.events.count(),
		Work: work,
	}
	if st.LastBackup != nil {
		b := BackupInfo(*st.LastBackup)
		out.LastBackup = &b
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) internalError(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// ErrNotRunning means no server answers on the admin socket.
var ErrNotRunning = errors.New("Garner isn't running")

// Admin talks to a running server over its admin socket.
type Admin struct {
	client *http.Client
}

// NewAdmin connects to the server whose data directory is dataDir.
func NewAdmin(dataDir string) (*Admin, error) {
	sock, err := SocketPath(dataDir)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	return &Admin{client: &http.Client{Transport: transport, Timeout: 10 * time.Minute}}, nil
}

func (a *Admin) CreateToken(ctx context.Context, name string) (CreatedToken, error) {
	var out CreatedToken
	return out, a.do(ctx, http.MethodPost, "/admin/tokens", map[string]string{"name": name}, &out)
}

func (a *Admin) Tokens(ctx context.Context) ([]TokenInfo, error) {
	var out []TokenInfo
	return out, a.do(ctx, http.MethodGet, "/admin/tokens", nil, &out)
}

func (a *Admin) RevokeToken(ctx context.Context, name string) (RevokedToken, error) {
	var out RevokedToken
	return out, a.do(ctx, http.MethodDelete, "/admin/tokens/"+url.PathEscape(name), nil, &out)
}

func (a *Admin) Backup(ctx context.Context, path string) (BackupInfo, error) {
	var out BackupInfo
	return out, a.do(ctx, http.MethodPost, "/admin/backup", map[string]string{"path": path}, &out)
}

// ImportProfile saves a profile's text, replacing a profile of the same
// name only when replace is set. A profile with problems fails with a
// *profile.Error that lists them.
func (a *Admin) ImportProfile(ctx context.Context, text string, replace bool) (ImportedProfile, error) {
	var out ImportedProfile
	return out, a.do(ctx, http.MethodPost, "/admin/profiles", map[string]any{"text": text, "replace": replace}, &out)
}

func (a *Admin) Status(ctx context.Context) (Status, error) {
	var out Status
	return out, a.do(ctx, http.MethodGet, "/admin/status", nil, &out)
}

func (a *Admin) do(ctx context.Context, method, path string, body, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://garner"+path, &buf)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return ErrNotRunning
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e apiError
		if json.NewDecoder(resp.Body).Decode(&e) != nil || e.Error == "" {
			return fmt.Errorf("server answered %s", resp.Status)
		}
		if len(e.Problems) > 0 {
			return &profile.Error{Problems: e.Problems}
		}
		return errors.New(e.Error)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
