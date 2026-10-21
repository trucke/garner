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

// Routes is the client API. The token check stands in front of all of it,
// so no route answers without a valid token, however it was added.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	s.register(mux)
	return s.auth(mux)
}

// router takes the client API's routes: a ServeMux, or a list of them in
// tests.
type router interface {
	Handle(pattern string, handler http.Handler)
}

// register adds every route of the client API.
func (s *Server) register(mux router) {
	mux.Handle("GET /api/v1/status", http.HandlerFunc(s.handleStatus))
	mux.Handle("GET /api/v1/status/services", http.HandlerFunc(s.handleServices))
	mux.Handle("GET /api/v1/events", http.HandlerFunc(s.handleEvents))
	s.catalogRoutes(mux)
	s.searchRoutes(mux)
	s.downloadRoutes(mux)
	s.importRoutes(mux)
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
	tok := r.Context().Value(tokenKey{}).(store.Token)
	out := map[string]any{"version": s.version, "token": tok.Name, "started": s.started}
	if tokens, err := s.store.Tokens(r.Context()); err == nil {
		for _, t := range tokens {
			if t.Name == tok.Name {
				out["token_created"] = t.Created
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
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
