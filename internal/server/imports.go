package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

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
	prober := s.deps.Prober
	if prober == nil {
		p, err := inspect.NewProber(s.cfg.FFprobe)
		if err != nil {
			s.log.Warn(err.Error() + "; importing is unavailable")
			return nil
		}
		prober = p
	}
	var jf *jellyfin.Client
	if j := s.cfg.Jellyfin; j.URL != "" && j.APIKey != "" {
		client, err := s.httpClient(j.CAFile, 30*time.Second)
		if err != nil {
			return err
		}
		jf = jellyfin.New(j.URL, j.APIKey.Value(), client)
		s.jellyfin = jf
	} else {
		s.log.Warn("Jellyfin is not configured (jellyfin.url and JELLYFIN_API_KEY); imports are not confirmed")
	}
	var rules func(int64) map[release.Service]string
	if s.catalog != nil {
		rules = s.catalog.ServiceOrders
	}
	imp := importer.New(s.store, prober, jf, importer.Options{
		Profiles:     s.profiles,
		SABnzbdPaths: s.cfg.SABnzbd.Paths, JellyfinPaths: s.cfg.Jellyfin.Paths, Library: lib, Rules: rules,
	}, s.log)
	imp.Now = s.now
	imp.OnChange = func(d store.Download) { s.events.publish(Event{Type: "download.updated", Data: d}) }
	if s.downloads != nil {
		imp.OnRejected = s.downloads.Reject
		s.downloads.OnCompleted = func(store.Download) { imp.Wake() }
	}
	s.imports = imp
	return nil
}

func (s *Server) importRoutes(mux router) {
	mux.Handle("POST /api/v1/downloads/{id}/import", http.HandlerFunc(s.handleRetryImport))
	mux.Handle("POST /api/v1/series/{id}/files/take-over", http.HandlerFunc(s.handleTakeOver))
	mux.Handle("POST /api/v1/series/{id}/files/rename", http.HandlerFunc(s.handleRename))
}

// handleTakeOver looks for a show's files in the library and takes over
// those that hold episodes without a file.
func (s *Server) handleTakeOver(w http.ResponseWriter, r *http.Request) {
	if id, ok := s.filesRequest(w, r); ok {
		t, err := s.imports.TakeOver(r.Context(), id)
		if err != nil {
			s.internalError(w, "take over files", err)
			return
		}
		writeJSON(w, http.StatusOK, t)
	}
}

// handleRename moves a show's taken-over files into Garner's folder for it
// under Garner's names.
func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	if id, ok := s.filesRequest(w, r); ok {
		// Files may have moved already: finish even when the client stops
		// waiting.
		res, err := s.imports.Rename(context.WithoutCancel(r.Context()), id)
		if err != nil {
			s.internalError(w, "rename files", err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

// filesRequest checks a request about a show's files: the importer runs,
// the show exists and none of its downloads or imports runs, which could
// record a file meanwhile.
func (s *Server) filesRequest(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if s.imports == nil {
		writeError(w, http.StatusServiceUnavailable, "managing files needs library.tv or library.anime and ffprobe")
		return 0, false
	}
	id, ok := seriesID(w, r)
	if !ok {
		return 0, false
	}
	exists, err := s.store.HasSeries(r.Context(), id)
	if err != nil {
		s.internalError(w, "find series", err)
		return 0, false
	}
	if !exists {
		writeError(w, http.StatusNotFound, "series is not in the catalogue")
		return 0, false
	}
	busy, err := s.store.BusyDownloads(r.Context(), id)
	if err != nil {
		s.internalError(w, "list running downloads", err)
		return 0, false
	}
	if len(busy) > 0 {
		writeError(w, http.StatusConflict, "a download or import of this show is running; try again when it is done")
		return 0, false
	}
	return id, true
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
