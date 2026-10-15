package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/trucke/garner/internal/httpx"
	"github.com/trucke/garner/internal/prowlarr"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/search"
	"github.com/trucke/garner/internal/selection"
	"github.com/trucke/garner/internal/store"
)

func (s *Server) setupSearch() error {
	p := s.cfg.Prowlarr
	if p.URL == "" || p.APIKey == "" {
		s.log.Warn("Prowlarr is not configured (prowlarr.url and PROWLARR_API_KEY); searching is unavailable")
		return nil
	}
	// Prowlarr waits for slow indexers, so allow more than a usual request.
	client, err := httpx.New("Garner/"+s.version, p.CAFile, 2*time.Minute)
	if err != nil {
		return err
	}
	var rules func(int64) map[release.Service]string
	if s.catalog != nil {
		rules = s.catalog.ServiceOrders
	}
	svc := search.New(s.store, prowlarr.New(p.URL, p.APIKey.Value(), client), rules, s.log)
	svc.OnProgress = func(p search.Progress) { s.events.publish(Event{Type: "search.progress", Data: p}) }
	svc.OnDone = func(sr store.Search) {
		s.events.publish(Event{Type: "search.finished", Data: map[string]any{"episode_id": sr.EpisodeID, "outcome": sr.Outcome}})
	}
	s.search = svc
	return nil
}

func (s *Server) searchRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/episodes/{id}/search", s.auth(http.HandlerFunc(s.handleSearchEpisode)))
	mux.Handle("GET /api/v1/episodes/{id}/search", s.auth(http.HandlerFunc(s.handleGetSearch)))
	mux.Handle("PATCH /api/v1/series/{id}", s.auth(http.HandlerFunc(s.handleUpdateSeries)))
}

func (s *Server) handleSearchEpisode(w http.ResponseWriter, r *http.Request) {
	if s.search == nil {
		writeError(w, http.StatusServiceUnavailable, "searching needs Prowlarr: set prowlarr.url and PROWLARR_API_KEY")
		return
	}
	id, ok := episodeID(w, r)
	if !ok {
		return
	}
	sr, err := s.search.Episode(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "episode is not in the catalogue")
		return
	}
	if err != nil {
		s.internalError(w, "search episode", err)
		return
	}
	writeJSON(w, http.StatusOK, sr)
}

func (s *Server) handleGetSearch(w http.ResponseWriter, r *http.Request) {
	id, ok := episodeID(w, r)
	if !ok {
		return
	}
	sr, err := s.store.LoadSearch(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "this episode has not been searched yet")
		return
	}
	if err != nil {
		s.internalError(w, "load search", err)
		return
	}
	writeJSON(w, http.StatusOK, sr)
}

func (s *Server) handleUpdateSeries(w http.ResponseWriter, r *http.Request) {
	id, ok := seriesID(w, r)
	if !ok {
		return
	}
	var req struct {
		Profile string `json:"profile"`
	}
	if err := readJSON(r, &req); err != nil || req.Profile != selection.TV.Name && req.Profile != selection.Anime.Name {
		writeError(w, http.StatusBadRequest, `expected {"profile": "tv"} or {"profile": "anime"}`)
		return
	}
	err := s.store.ChangeProfile(r.Context(), id, req.Profile)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "series is not in the catalogue")
		return
	}
	if err != nil {
		s.internalError(w, "update series", err)
		return
	}
	s.writeSeries(w, r, id, http.StatusOK)
}

func episodeID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid episode ID")
		return 0, false
	}
	return id, true
}
