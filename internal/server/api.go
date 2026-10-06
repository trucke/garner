package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/trucke/garner/internal/store"
)

type tokenKey struct{}

// Routes is the client API. Every route requires a token.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/status", s.auth(http.HandlerFunc(s.handleStatus)))
	mux.Handle("GET /api/v1/events", s.auth(http.HandlerFunc(s.handleEvents)))
	s.catalogRoutes(mux)
	s.searchRoutes(mux)
	s.downloadRoutes(mux)
	return mux
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plain, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="garner"`)
			writeError(w, http.StatusUnauthorized, "missing token")
			return
		}
		tok, err := s.store.Authenticate(r.Context(), strings.TrimSpace(plain))
		if errors.Is(err, store.ErrUnauthorized) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="garner", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "invalid or revoked token")
			return
		}
		if err != nil {
			s.log.Error("authenticate", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey{}, tok)))
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.version})
}

// handleEvents streams server-sent events. Clients reload state after
// reconnecting instead of expecting missed events to be replayed.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	tok := r.Context().Value(tokenKey{}).(store.Token)
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")

	sub := s.events.subscribe(tok.ID)
	defer s.events.remove(sub)
	// A revocation between authenticating and subscribing would miss this
	// stream; check again now that revocations can reach it.
	if ok, err := s.store.StillValid(r.Context(), tok); err != nil || !ok {
		writeError(w, http.StatusUnauthorized, "invalid or revoked token")
		return
	}
	write := func(text string) error {
		rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := fmt.Fprint(w, text); err != nil {
			return err
		}
		return rc.Flush()
	}
	if write("retry: 3000\n\n"+sse(Event{Type: "hello", Data: map[string]string{"version": s.version}})) != nil {
		return
	}
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-sub.done:
			return
		case e := <-sub.ch:
			err = write(sse(e))
		case <-ping.C:
			err = write(": ping\n\n")
		}
		if err != nil {
			return
		}
	}
}

func sse(e Event) string {
	data, err := json.Marshal(e.Data)
	if err != nil {
		data = []byte("null")
	}
	return "event: " + e.Type + "\ndata: " + string(data) + "\n\n"
}

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
