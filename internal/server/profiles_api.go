package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/profile"
	"github.com/trucke/garner/internal/store"
)

func (s *Server) profileRoutes(mux router) {
	mux.Handle("GET /api/v1/profiles", http.HandlerFunc(s.handleListProfiles))
	mux.Handle("GET /api/v1/profiles/{id}", http.HandlerFunc(s.handleGetProfile))
	mux.Handle("POST /api/v1/profiles", http.HandlerFunc(s.handleCreateProfile))
	mux.Handle("PUT /api/v1/profiles/{id}", http.HandlerFunc(s.handleReplaceProfile))
	mux.Handle("DELETE /api/v1/profiles/{id}", http.HandlerFunc(s.handleDeleteProfile))
	mux.Handle("PUT /api/v1/profiles/{id}/default", http.HandlerFunc(s.handleDefaultProfile))
}

// apiProfile is a stored profile as clients see it, with the shows that
// use it.
func (s *Server) apiProfile(ctx context.Context, p store.StoredProfile) (api.Profile, error) {
	shows, err := s.store.ProfileShowNames(ctx, p.ID)
	if err != nil {
		return api.Profile{}, err
	}
	return api.Profile{ID: p.ID, Name: p.Name, Text: p.Text, DefaultTV: p.DefaultTV, DefaultAnime: p.DefaultAnime, Shows: shows}, nil
}

func (s *Server) handleListProfiles(w http.ResponseWriter, r *http.Request) {
	stored, err := s.store.Profiles(r.Context())
	if err != nil {
		s.internalError(w, "list profiles", err)
		return
	}
	out := make([]api.Profile, 0, len(stored))
	for _, p := range stored {
		ap, err := s.apiProfile(r.Context(), p)
		if err != nil {
			s.internalError(w, "list profiles", err)
			return
		}
		out = append(out, ap)
	}
	writeJSON(w, http.StatusOK, out)
}

func profileID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid profile ID")
		return 0, false
	}
	return id, true
}

func (s *Server) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := profileID(w, r)
	if !ok {
		return
	}
	s.writeProfile(w, r, id, http.StatusOK)
}

func (s *Server) writeProfile(w http.ResponseWriter, r *http.Request, id int64, status int) {
	p, err := s.store.Profile(r.Context(), id)
	if errors.Is(err, store.ErrNoProfile) {
		writeError(w, http.StatusNotFound, "no such profile")
		return
	}
	if err != nil {
		s.internalError(w, "load profile", err)
		return
	}
	ap, err := s.apiProfile(r.Context(), p)
	if err != nil {
		s.internalError(w, "load profile", err)
		return
	}
	writeJSON(w, status, ap)
}

// profileText reads a request that carries a profile's text and checks
// it. On a problem it answers and returns false.
func profileText(w http.ResponseWriter, r *http.Request) (profile.Profile, bool) {
	var req struct {
		Text string `json:"text"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, `expected {"text": a profile}`)
		return profile.Profile{}, false
	}
	p, _, err := profile.Parse(req.Text)
	var pe *profile.Error
	if errors.As(err, &pe) {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "the profile has problems", Problems: pe.Problems})
		return profile.Profile{}, false
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return profile.Profile{}, false
	}
	return p, true
}

func (s *Server) handleCreateProfile(w http.ResponseWriter, r *http.Request) {
	p, ok := profileText(w, r)
	if !ok {
		return
	}
	id, err := s.saveProfile(r.Context(), 0, p)
	if !s.profileSaved(w, err) {
		return
	}
	s.writeProfile(w, r, id, http.StatusCreated)
}

func (s *Server) handleReplaceProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := profileID(w, r)
	if !ok {
		return
	}
	p, ok := profileText(w, r)
	if !ok {
		return
	}
	_, err := s.saveProfile(r.Context(), id, p)
	if !s.profileSaved(w, err) {
		return
	}
	s.writeProfile(w, r, id, http.StatusOK)
}

// saveProfile stores a checked profile in its canonical form: as a new
// one for id 0, or in place of profile id. Garner then judges by it at
// once, and the clients learn of it.
func (s *Server) saveProfile(ctx context.Context, id int64, p profile.Profile) (int64, error) {
	text := profile.Encode(p)
	var err error
	if id == 0 {
		id, err = s.store.CreateProfile(ctx, p.Name, text)
	} else {
		err = s.store.UpdateProfile(ctx, id, p.Name, text)
	}
	if err != nil {
		return 0, err
	}
	s.profilesChanged(ctx)
	return id, nil
}

// profilesChanged reloads the profiles Garner judges by and tells the
// clients. A changed profile drops the search results of its shows, so the
// monitor may have work.
func (s *Server) profilesChanged(ctx context.Context) {
	if err := s.loadProfiles(ctx); err != nil {
		s.log.Error("reload profiles", "err", err)
	}
	s.events.publish(Event{Type: "profiles.updated"})
	s.wakeMonitor()
}

// profileSaved answers a failed save and reports whether it succeeded.
func (s *Server) profileSaved(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "another profile has that name")
	case errors.Is(err, store.ErrNoProfile):
		writeError(w, http.StatusNotFound, "no such profile")
	default:
		s.internalError(w, "save profile", err)
	}
	return false
}

func (s *Server) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := profileID(w, r)
	if !ok {
		return
	}
	err := s.store.DeleteProfile(r.Context(), id)
	var inUse *store.ProfileInUseError
	switch {
	case errors.As(err, &inUse):
		writeError(w, http.StatusConflict, inUse.Error())
		return
	case errors.Is(err, store.ErrNoProfile):
		writeError(w, http.StatusNotFound, "no such profile")
		return
	case err != nil:
		s.internalError(w, "delete profile", err)
		return
	}
	s.profilesChanged(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// handleDefaultProfile makes a profile the one new shows of the types get.
func (s *Server) handleDefaultProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := profileID(w, r)
	if !ok {
		return
	}
	var req struct {
		Types []string `json:"types"`
	}
	err := readJSON(r, &req)
	if err != nil || len(req.Types) == 0 || slices.ContainsFunc(req.Types, func(t string) bool { return t != store.TypeTV && t != store.TypeAnime }) {
		writeError(w, http.StatusBadRequest, `expected {"types": ["tv"], ["anime"] or ["tv", "anime"]}`)
		return
	}
	for _, typ := range req.Types {
		if err := s.store.SetDefaultProfile(r.Context(), typ, id); err != nil {
			if !s.profileSaved(w, err) {
				return
			}
		}
	}
	s.profilesChanged(r.Context())
	s.writeProfile(w, r, id, http.StatusOK)
}
