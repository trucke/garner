package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/trucke/garner/internal/download"
	"github.com/trucke/garner/internal/httpx"
	"github.com/trucke/garner/internal/sabnzbd"
	"github.com/trucke/garner/internal/store"
)

func (s *Server) setupDownloads() error {
	c := s.cfg.SABnzbd
	if c.URL == "" || c.APIKey == "" {
		s.log.Warn("SABnzbd is not configured (sabnzbd.url and SABNZBD_API_KEY); downloading is unavailable")
		return nil
	}
	client, err := httpx.New("Garner/"+s.version, c.CAFile, 30*time.Second)
	if err != nil {
		return err
	}
	svc := download.New(s.store, sabnzbd.New(c.URL, c.APIKey.Value(), client), c.Category, s.log)
	svc.OnChange = func(d store.Download) { s.events.publish(Event{Type: "download.updated", Data: d}) }
	s.downloads = svc
	return nil
}

func (s *Server) downloadRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/candidates/{id}/download", s.auth(s.needDownloads(s.handleDownloadCandidate)))
	mux.Handle("POST /api/v1/episodes/{id}/download", s.auth(s.needDownloads(s.handleDownloadBest)))
	mux.Handle("GET /api/v1/downloads", s.auth(http.HandlerFunc(s.handleListDownloads)))
	mux.Handle("GET /api/v1/downloads/{id}", s.auth(http.HandlerFunc(s.handleGetDownload)))
}

func (s *Server) needDownloads(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.downloads == nil {
			writeError(w, http.StatusServiceUnavailable, "downloading needs SABnzbd: set sabnzbd.url and SABNZBD_API_KEY")
			return
		}
		h(w, r)
	})
}

// handleDownloadCandidate sends any candidate of a search to SABnzbd: the
// manual choice.
func (s *Server) handleDownloadCandidate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "invalid candidate ID")
	if !ok {
		return
	}
	d, err := s.downloads.Candidate(r.Context(), id)
	s.writeDownload(w, d, err, "candidate is not part of a search")
}

// handleDownloadBest sends the best release of the episode's latest search.
func (s *Server) handleDownloadBest(w http.ResponseWriter, r *http.Request) {
	id, ok := episodeID(w, r)
	if !ok {
		return
	}
	d, err := s.downloads.Best(r.Context(), id)
	s.writeDownload(w, d, err, "this episode has not been searched yet")
}

func (s *Server) writeDownload(w http.ResponseWriter, d store.Download, err error, notFound string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, notFound)
	case errors.Is(err, store.ErrDownloadRunning), errors.Is(err, download.ErrNoCandidate):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		s.internalError(w, "start download", err)
	default:
		writeJSON(w, http.StatusCreated, d)
	}
}

func (s *Server) handleListDownloads(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.Downloads(r.Context(), 100)
	if err != nil {
		s.internalError(w, "list downloads", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "invalid download ID")
	if !ok {
		return
	}
	d, err := s.store.GetDownload(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such download")
		return
	}
	if err != nil {
		s.internalError(w, "load download", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func pathID(w http.ResponseWriter, r *http.Request, invalid string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, invalid)
		return 0, false
	}
	return id, true
}
