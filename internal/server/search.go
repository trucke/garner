package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/monitor"
	"github.com/trucke/garner/internal/prowlarr"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/search"
	"github.com/trucke/garner/internal/store"
)

func (s *Server) setupSearch() error {
	p := s.cfg.Prowlarr
	if m := s.prowlarrMissing(); len(m) > 0 {
		s.log.Warn("Prowlarr is not set up: " + lacking(m) + "; searching is unavailable")
		return nil
	}
	// Prowlarr waits for slow indexers, so allow more than a usual request.
	client, err := s.httpClient(p.CAFile, 2*time.Minute)
	if err != nil {
		return err
	}
	var rules func(int64) map[release.Service]string
	if s.catalog != nil {
		rules = s.catalog.ServiceOrders
	}
	s.prowlarr = prowlarr.New(p.URL, p.APIKey.Value(), client)
	s.prowlarr.Now = s.now
	if s.deps.Sleep != nil {
		s.prowlarr.Sleep = s.deps.Sleep
	}
	svc := search.New(s.store, s.prowlarr, rules, s.profileFor, s.log)
	svc.Now = s.now
	svc.OnProgress = func(p api.SearchProgress) { s.events.publish(Event{Type: "search.progress", Data: p}) }
	svc.OnDone = func(sr store.Search) {
		s.events.publish(Event{Type: "search.finished", Data: map[string]any{"episode_id": sr.EpisodeID, "outcome": sr.Outcome}})
	}
	s.search = svc
	return nil
}

func (s *Server) searchRoutes(mux router) {
	mux.Handle("POST /api/v1/episodes/{id}/search", http.HandlerFunc(s.handleSearchEpisode))
	mux.Handle("GET /api/v1/episodes/{id}/search", http.HandlerFunc(s.handleGetSearch))
	mux.Handle("POST /api/v1/series/{id}/seasons/{season}/search", http.HandlerFunc(s.handleSearchSeason))
	mux.Handle("POST /api/v1/series/{id}/search", http.HandlerFunc(s.handleSearchShow))
	mux.Handle("PATCH /api/v1/series/{id}", http.HandlerFunc(s.handleUpdateSeries))
}

func (s *Server) handleSearchEpisode(w http.ResponseWriter, r *http.Request) {
	if s.search == nil {
		writeError(w, http.StatusServiceUnavailable, "searching needs Prowlarr: "+lacking(s.prowlarrMissing()))
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
	sr.NextRetry = s.nextSearch(r.Context(), id)
	writeJSON(w, http.StatusOK, sr)
}

// handleSearchSeason searches a whole season for packs.
func (s *Server) handleSearchSeason(w http.ResponseWriter, r *http.Request) {
	if s.search == nil {
		writeError(w, http.StatusServiceUnavailable, "searching needs Prowlarr: "+lacking(s.prowlarrMissing()))
		return
	}
	id, ok := seriesID(w, r)
	if !ok {
		return
	}
	season, err := strconv.Atoi(r.PathValue("season"))
	if err != nil || season < 1 {
		writeError(w, http.StatusBadRequest, "invalid season; specials have no season packs")
		return
	}
	sr, err := s.search.Season(r.Context(), id, season)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "the show or season is not in the catalogue")
		return
	}
	if err != nil {
		s.internalError(w, "search season", err)
		return
	}
	writeJSON(w, http.StatusOK, sr)
}

// handleSearchShow asks for every missing episode of a show to be searched
// and the best release of each that meets its rules to be downloaded. It
// answers at once with how many episodes are missing; series.search events
// follow the search, which runs after the ones asked for before.
func (s *Server) handleSearchShow(w http.ResponseWriter, r *http.Request) {
	if s.monitor == nil {
		writeError(w, http.StatusServiceUnavailable, "searching and downloading need Prowlarr and SABnzbd: "+lacking(append(s.prowlarrMissing(), s.sabnzbdMissing()...)))
		return
	}
	id, ok := seriesID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	exists, err := s.store.HasSeries(ctx, id)
	if err != nil {
		s.internalError(w, "search show", err)
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "series is not in the catalogue")
		return
	}
	missing, err := s.store.Missing(ctx, s.now(), id)
	if err != nil {
		s.internalError(w, "search show", err)
		return
	}
	if len(missing) == 0 {
		writeJSON(w, http.StatusOK, map[string]int{"episodes": 0})
		return
	}
	if err := s.monitor.SearchShow(id); errors.Is(err, monitor.ErrSearching) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"episodes": len(missing)})
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
	sr.NextRetry = s.nextSearch(r.Context(), id)
	writeJSON(w, http.StatusOK, sr)
}

// nextSearch is when the monitor searches an episode again; nil when it
// does not get the episode, or the answer is unknown.
func (s *Server) nextSearch(ctx context.Context, episodeID int64) *time.Time {
	seriesID, err := s.store.SeriesOfEpisode(ctx, episodeID)
	if err != nil {
		return nil
	}
	_, states, err := s.store.ShowStatus(ctx, seriesID, s.now(), s.schedule())
	if err != nil {
		s.log.Warn("next search", "episode", episodeID, "err", err)
		return nil
	}
	for _, st := range states {
		if st.EpisodeID == episodeID {
			return st.NextRetry
		}
	}
	return nil
}

// handleUpdateSeries changes a show's profile, its monitor mode, or both.
func (s *Server) handleUpdateSeries(w http.ResponseWriter, r *http.Request) {
	id, ok := seriesID(w, r)
	if !ok {
		return
	}
	var req struct {
		ProfileID int64  `json:"profile_id"`
		Type      string `json:"type"`
		Monitor   string `json:"monitor"`
	}
	err := readJSON(r, &req)
	switch {
	case err != nil || req.ProfileID == 0 && req.Type == "" && req.Monitor == "",
		req.ProfileID < 0,
		req.Type != "" && req.Type != store.TypeTV && req.Type != store.TypeAnime,
		req.Monitor != "" && !slices.Contains(store.MonitorModes, req.Monitor):
		writeError(w, http.StatusBadRequest, `expected any of {"profile_id": a profile's ID, "type": "tv" or "anime", "monitor": "new", "all" or "off"}`)
		return
	}
	ctx := r.Context()
	if req.ProfileID != 0 {
		err = s.store.ChangeProfile(ctx, id, req.ProfileID)
	}
	if err == nil && req.Type != "" {
		err = s.store.ChangeType(ctx, id, req.Type)
	}
	if err == nil && req.Monitor != "" {
		err = s.store.SetMonitor(ctx, id, req.Monitor, s.now())
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "series is not in the catalogue")
		return
	case errors.Is(err, store.ErrNoProfile):
		writeError(w, http.StatusBadRequest, "no profile has that ID")
		return
	case err != nil:
		s.internalError(w, "update series", err)
		return
	}
	s.events.publish(Event{Type: "series.updated", Data: map[string]int64{"id": id}})
	// Episodes may be due now: a new mode, or cleared searches.
	s.wakeMonitor()
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
