package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/trucke/garner/internal/catalog"
	"github.com/trucke/garner/internal/store"
)

// SeriesDetail is a show with everything Garner knows about it and the
// problems found in imported data.
type SeriesDetail struct {
	store.SeriesData
	Issues []catalog.Issue `json:"issues"`
}

func (s *Server) catalogRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/search", s.auth(s.needCatalog(s.handleSearch)))
	mux.Handle("GET /api/v1/series", s.auth(s.needCatalog(s.handleListSeries)))
	mux.Handle("POST /api/v1/series", s.auth(s.needCatalog(s.handleAddSeries)))
	mux.Handle("GET /api/v1/series/{id}", s.auth(s.needCatalog(s.handleGetSeries)))
	mux.Handle("POST /api/v1/series/{id}/refresh", s.auth(s.needCatalog(s.handleRefreshSeries)))
}

func (s *Server) needCatalog(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.catalog == nil {
			writeError(w, http.StatusServiceUnavailable, "the catalogue needs TVDB access: set TVDB_API_KEY")
			return
		}
		h(w, r)
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, "missing search text: ?q=")
		return
	}
	results, err := s.catalog.Search(r.Context(), q)
	if err != nil {
		s.upstreamError(w, "search TVDB", err)
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func (s *Server) handleListSeries(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListSeries(r.Context())
	if err != nil {
		s.internalError(w, "list series", err)
		return
	}
	if list == nil {
		list = []store.Series{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleAddSeries(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TVDBID int64 `json:"tvdb_id"`
	}
	if err := readJSON(r, &req); err != nil || req.TVDBID <= 0 {
		writeError(w, http.StatusBadRequest, `expected {"tvdb_id": <number>}`)
		return
	}
	err := s.catalog.Add(r.Context(), req.TVDBID)
	switch {
	case errors.Is(err, catalog.ErrExists):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, catalog.ErrUnknownSeries):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		s.upstreamError(w, "add series", err)
		return
	}
	s.writeSeries(w, r, req.TVDBID, http.StatusCreated)
}

func (s *Server) handleGetSeries(w http.ResponseWriter, r *http.Request) {
	id, ok := seriesID(w, r)
	if ok {
		s.writeSeries(w, r, id, http.StatusOK)
	}
}

func (s *Server) handleRefreshSeries(w http.ResponseWriter, r *http.Request) {
	id, ok := seriesID(w, r)
	if !ok {
		return
	}
	exists, err := s.store.HasSeries(r.Context(), id)
	if err != nil {
		s.internalError(w, "refresh series", err)
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "series is not in the catalogue")
		return
	}
	if err := s.catalog.Refresh(r.Context(), id); err != nil {
		s.upstreamError(w, "refresh series", err)
		return
	}
	s.writeSeries(w, r, id, http.StatusOK)
}

func (s *Server) writeSeries(w http.ResponseWriter, r *http.Request, id int64, status int) {
	d, err := s.store.LoadSeries(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "series is not in the catalogue")
		return
	}
	if err != nil {
		s.internalError(w, "load series", err)
		return
	}
	writeJSON(w, status, SeriesDetail{SeriesData: d, Issues: catalog.Issues(d)})
}

func seriesID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid series ID")
		return 0, false
	}
	return id, true
}

// upstreamError reports a failure that likely comes from TVDB or TheXEM.
func (s *Server) upstreamError(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "err", err)
	writeError(w, http.StatusBadGateway, err.Error())
}
