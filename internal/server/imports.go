package server

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/trucke/garner/internal/httpx"
	"github.com/trucke/garner/internal/importer"
	"github.com/trucke/garner/internal/inspect"
	"github.com/trucke/garner/internal/jellyfin"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/store"
)

func (s *Server) setupImports() error {
	lib := s.cfg.Library
	if lib.TV == "" && lib.Anime == "" {
		s.log.Warn("library.tv and library.anime are not set; importing is unavailable")
		return nil
	}
	for _, dir := range []string{lib.TV, lib.Anime} {
		if st, err := os.Stat(dir); dir != "" && (err != nil || !st.IsDir()) {
			s.log.Warn("library folder is missing; imports fail until it is there (is the share mounted?)", "path", dir)
		}
	}
	prober, err := inspect.NewProber(s.cfg.FFprobe)
	if err != nil {
		s.log.Warn(err.Error() + "; importing is unavailable")
		return nil
	}
	var jf *jellyfin.Client
	if j := s.cfg.Jellyfin; j.URL != "" && j.APIKey != "" {
		client, err := httpx.New("Garner/"+s.version, j.CAFile, 30*time.Second)
		if err != nil {
			return err
		}
		jf = jellyfin.New(j.URL, j.APIKey.Value(), client)
	} else {
		s.log.Warn("Jellyfin is not configured (jellyfin.url and JELLYFIN_API_KEY); imports are not confirmed")
	}
	var rules func(int64) map[release.Service]string
	if s.catalog != nil {
		rules = s.catalog.ServiceOrders
	}
	imp := importer.New(s.store, prober, jf, importer.Options{
		SABnzbdPaths: s.cfg.SABnzbd.Paths, JellyfinPaths: s.cfg.Jellyfin.Paths, Library: lib, Rules: rules,
	}, s.log)
	imp.OnChange = func(d store.Download) { s.events.publish(Event{Type: "download.updated", Data: d}) }
	if s.downloads != nil {
		imp.OnRejected = s.downloads.Reject
		s.downloads.OnCompleted = func(store.Download) { imp.Wake() }
	}
	s.imports = imp
	return nil
}

func (s *Server) importRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/downloads/{id}/import", s.auth(http.HandlerFunc(s.handleRetryImport)))
}

// handleRetryImport repeats the import step that failed, after the problem
// its recovery step names was fixed.
func (s *Server) handleRetryImport(w http.ResponseWriter, r *http.Request) {
	if s.imports == nil {
		writeError(w, http.StatusServiceUnavailable, "importing needs library.tv or library.anime and ffprobe")
		return
	}
	id, ok := pathID(w, r, "invalid download ID")
	if !ok {
		return
	}
	d, err := s.imports.Retry(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such download")
	case errors.Is(err, importer.ErrNotRetryable):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		s.internalError(w, "retry import", err)
	default:
		writeJSON(w, http.StatusAccepted, d)
	}
}
