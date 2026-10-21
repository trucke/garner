package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/download"
	"github.com/trucke/garner/internal/sabnzbd"
	"github.com/trucke/garner/internal/store"
)

func (s *Server) setupDownloads() error {
	c := s.cfg.SABnzbd
	if c.URL == "" || c.APIKey == "" {
		s.log.Warn("SABnzbd is not configured (sabnzbd.url and SABNZBD_API_KEY); downloading is unavailable")
		return nil
	}
	client, err := s.httpClient(c.CAFile, 30*time.Second)
	if err != nil {
		return err
	}
	s.sabnzbd = sabnzbd.New(c.URL, c.APIKey.Value(), client)
	svc := download.New(s.store, s.sabnzbd, c.Category, s.log)
	svc.Now = s.now
	svc.OnChange = func(d store.Download) { s.events.publish(Event{Type: "download.updated", Data: d}) }
	s.downloads = svc
	return nil
}

func (s *Server) downloadRoutes(mux router) {
	mux.Handle("POST /api/v1/candidates/{id}/download", s.needDownloads(s.handleDownloadCandidate))
	mux.Handle("POST /api/v1/episodes/{id}/download", s.needDownloads(s.handleDownloadBest))
	mux.Handle("GET /api/v1/downloads", http.HandlerFunc(s.handleListDownloads))
	mux.Handle("GET /api/v1/downloads/{id}", http.HandlerFunc(s.handleGetDownload))
	mux.Handle("GET /api/v1/activity", http.HandlerFunc(s.handleActivity))
	mux.Handle("POST /api/v1/season-candidates/{id}/download", s.needDownloads(s.handleDownloadSeasonCandidate))
	mux.Handle("POST /api/v1/downloads/{id}/cancel", s.needDownloads(s.handleCancelDownload))
	mux.Handle("GET /api/v1/episodes/{id}", http.HandlerFunc(s.handleGetEpisode))
}

func (s *Server) handleGetEpisode(w http.ResponseWriter, r *http.Request) {
	id, ok := episodeID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var out api.EpisodeDetail
	var err error
	out.Overview, out.OverviewLanguage, err = s.store.EpisodeOverview(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such episode")
		return
	} else if err != nil {
		s.internalError(w, "load episode", err)
		return
	}
	f, found, err := s.store.EpisodeFile(ctx, id)
	if err != nil {
		s.internalError(w, "load episode file", err)
		return
	}
	if found {
		out.File = &api.EpisodeFile{Path: f.Path, Size: f.Size}
	}
	if out.Downloads, err = s.store.EpisodeDownloads(ctx, id); err != nil {
		s.internalError(w, "list episode downloads", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCancelDownload stops a queued or downloading release: SABnzbd
// deletes what it has, and the episode counts as missing again.
func (s *Server) handleCancelDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "invalid download ID")
	if !ok {
		return
	}
	ctx := r.Context()
	d, err := s.store.GetDownload(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such download")
		return
	}
	if err != nil {
		s.internalError(w, "load download", err)
		return
	}
	switch d.State {
	case store.Queued, store.Downloading:
	case store.Submitting:
		writeError(w, http.StatusConflict, "SABnzbd has not confirmed the download yet; try again in a moment")
		return
	case store.Processing:
		writeError(w, http.StatusConflict, "SABnzbd is already repairing or unpacking it; it can no longer be cancelled")
		return
	default:
		writeError(w, http.StatusConflict, store.ErrNotRunning.Error())
		return
	}
	d, err = s.downloads.Cancel(ctx, d, "cancelled")
	switch {
	case errors.Is(err, store.ErrNotRunning):
		writeError(w, http.StatusConflict, "the download ended before it could be cancelled")
	case err != nil:
		s.log.Error("cancel download", "download", id, "err", err)
		writeError(w, http.StatusBadGateway, "SABnzbd could not cancel it: "+err.Error())
	default:
		writeJSON(w, http.StatusOK, d)
	}
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
func (s *Server) handleDownloadSeasonCandidate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "invalid candidate ID")
	if !ok {
		return
	}
	d, err := s.downloads.SeasonCandidate(r.Context(), id)
	s.writeDownload(w, d, err, "candidate is not part of a season search")
}

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

// activityFor is how long a finished download stays in the activity.
const activityFor = 24 * time.Hour

// handleActivity lists what runs, what waits for the user, and what ended
// within the last day: one row per episode, its latest download.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.Activity(r.Context(), s.now().Add(-activityFor))
	if err != nil {
		s.internalError(w, "list activity", err)
		return
	}
	writeJSON(w, http.StatusOK, items)
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
