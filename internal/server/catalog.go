package server

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trucke/garner/internal/catalog"
	"github.com/trucke/garner/internal/importer"
	"github.com/trucke/garner/internal/monitor"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/selection"
	"github.com/trucke/garner/internal/store"
)

// SeriesDetail is a show with everything Garner knows about it, the
// problems found in imported data, and what Garner has of each episode.
type SeriesDetail struct {
	store.SeriesData
	Issues []catalog.Issue `json:"issues"`
	// Summary counts the show's episodes as the library does.
	Summary store.Show `json:"summary"`
	// States holds every episode's state; Best lists the tiers no upgrade
	// can beat, so clients can mark files below them.
	States []store.EpisodeStatus `json:"states"`
	Best   string                `json:"best_tier"`
	// Folder is where new files of the show go; URL is its TVDB page.
	Folder string `json:"folder,omitempty"`
	URL    string `json:"url"`
	// Rules describes the profile; Profiles lists the profiles to choose
	// from with their rules.
	Rules    string        `json:"rules"`
	Profiles []ProfileInfo `json:"profiles"`
	// LanguageName and LanguageNote say what the original language means
	// for the audio.
	LanguageName string `json:"language_name"`
	LanguageNote string `json:"language_note"`
	// MonitorOff says why the monitor does not run, whatever the show's
	// mode; empty when it runs.
	MonitorOff string `json:"monitor_off,omitempty"`
	// Searching is the search of every missing episode, while it is asked
	// for or runs.
	Searching *monitor.ShowProgress `json:"searching,omitempty"`
}

// ProfileInfo names a profile and its rules.
type ProfileInfo struct {
	Name  string `json:"name"`
	Rules string `json:"rules"`
}

func (s *Server) catalogRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/search", s.auth(s.needCatalog(s.handleSearch)))
	mux.Handle("GET /api/v1/search/{tvdb_id}", s.auth(s.needCatalog(s.handlePreview)))
	mux.Handle("GET /api/v1/series", s.auth(s.needCatalog(s.handleListSeries)))
	mux.Handle("POST /api/v1/series", s.auth(s.needCatalog(s.handleAddSeries)))
	mux.Handle("GET /api/v1/series/{id}", s.auth(s.needCatalog(s.handleGetSeries)))
	mux.Handle("POST /api/v1/series/{id}/refresh", s.auth(s.needCatalog(s.handleRefreshSeries)))
	mux.Handle("DELETE /api/v1/series/{id}", s.auth(http.HandlerFunc(s.handleDeleteSeries)))
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

// handlePreview describes a show on TVDB before it is added.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("tvdb_id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "expected a TVDB series ID")
		return
	}
	p, err := s.catalog.Preview(r.Context(), id)
	switch {
	case errors.Is(err, catalog.ErrUnknownSeries):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		s.upstreamError(w, "describe a TVDB series", err)
	default:
		writeJSON(w, http.StatusOK, p)
	}
}

// handleListSeries lists the shows with a summary of each: episode counts,
// size, next air date and the most urgent episode.
func (s *Server) handleListSeries(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.Library(r.Context(), time.Now(), s.monitoring())
	if err != nil {
		s.internalError(w, "list series", err)
		return
	}
	if list == nil {
		list = []store.Show{}
	}
	writeJSON(w, http.StatusOK, list)
}

// handleAddSeries adds a show. monitor says which of its episodes the
// monitor gets; new ones when left out.
func (s *Server) handleAddSeries(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TVDBID  int64  `json:"tvdb_id"`
		Monitor string `json:"monitor"`
	}
	if err := readJSON(r, &req); err != nil || req.TVDBID <= 0 {
		writeError(w, http.StatusBadRequest, `expected {"tvdb_id": <number>, "monitor": "new", "all" or "off"}`)
		return
	}
	req.Monitor = cmp.Or(req.Monitor, store.MonitorNew)
	if !slices.Contains(store.MonitorModes, req.Monitor) {
		writeError(w, http.StatusBadRequest, `monitor must be "new", "all" or "off"`)
		return
	}
	err := s.catalog.Add(r.Context(), req.TVDBID, req.Monitor)
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
	err = s.catalog.Refresh(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "the show was deleted while it was refreshed")
		return
	}
	if err != nil {
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
	summary, states, err := s.store.ShowStatus(r.Context(), id, time.Now(), s.monitoring())
	if err != nil {
		s.internalError(w, "summarise series", err)
		return
	}
	profile := selection.ProfileNamed(d.Series.Profile)
	original, _ := release.LanguageNamed(d.Series.OriginalLanguage)
	detail := SeriesDetail{
		SeriesData: d, Issues: catalog.Issues(d), Summary: summary, States: states, Best: profile.Tiers[0].String(),
		Rules: profile.Describe(), LanguageName: catalog.LanguageName(d.Series.OriginalLanguage), LanguageNote: selection.OriginalNote(original),
		URL: catalog.SeriesURL(d.Series.Slug, d.Series.ID), MonitorOff: s.monitorOff(),
	}
	for _, p := range []selection.Profile{selection.Anime, selection.TV} {
		detail.Profiles = append(detail.Profiles, ProfileInfo{Name: p.Name, Rules: p.Describe()})
	}
	if folder, err := importer.ShowFolder(s.cfg.Library, d.Series, profile); err == nil {
		detail.Folder = folder
	}
	if s.monitor != nil {
		if p, ok := s.monitor.ShowSearch(id); ok {
			detail.Searching = &p
		}
	}
	writeJSON(w, status, detail)
}

// handleDeleteSeries removes a show and cancels its running downloads.
// files=delete also deletes the files Garner imported for it; files=keep
// leaves them in the library. One of them is required, so a bare request
// never deletes files.
func (s *Server) handleDeleteSeries(w http.ResponseWriter, r *http.Request) {
	id, ok := seriesID(w, r)
	if !ok {
		return
	}
	var keep bool
	switch r.URL.Query().Get("files") {
	case "keep":
		keep = true
	case "delete":
	default:
		writeError(w, http.StatusBadRequest, "say what happens to the show's files: files=keep or files=delete")
		return
	}
	ctx := r.Context()
	busy, err := s.store.BusyDownloads(ctx, id)
	if err != nil {
		s.internalError(w, "delete series", err)
		return
	}
	for _, d := range busy {
		if !d.Active() {
			writeError(w, http.StatusConflict, "Garner is moving files of this show into the library; try again when that has finished")
			return
		}
	}
	// Read before cancelling, so nothing is cancelled when this fails.
	files, err := s.store.SeriesFiles(ctx, id)
	if err != nil {
		s.internalError(w, "delete series", err)
		return
	}
	// A file holding several episodes is listed once per episode.
	paths := make([]string, 0, len(files))
	seen := make(map[string]bool, len(files))
	var size int64
	for _, f := range files {
		if !seen[f.Path] {
			seen[f.Path] = true
			paths = append(paths, f.Path)
			size += f.Size
		}
	}
	if len(busy) > 0 && s.downloads == nil {
		writeError(w, http.StatusConflict, "the show has running downloads, and SABnzbd is not configured to cancel them")
		return
	}
	for i, d := range busy {
		// One that ended meanwhile needs no cancelling.
		if _, err := s.downloads.Cancel(ctx, d, "cancelled: the show was deleted"); err != nil && !errors.Is(err, store.ErrNotRunning) {
			s.log.Error("cancel download", "download", d.ID, "err", err)
			msg := "could not cancel a download in SABnzbd, so the show stays: " + err.Error()
			if i > 0 {
				msg = fmt.Sprintf("cancelled %d of %d downloads, then SABnzbd failed, so the show stays; delete it again to finish: %v", i, len(busy), err)
			}
			writeError(w, http.StatusBadGateway, msg)
			return
		}
	}
	// Records first: if a download started meanwhile, nothing is touched.
	// Files left on disk are visible and can be removed by hand; records of
	// files that are gone would not be.
	err = s.store.DeleteSeries(ctx, id)
	cancelled := ""
	if len(busy) > 0 {
		cancelled = fmt.Sprintf(" Its %d running downloads were already cancelled.", len(busy))
	}
	if errors.Is(err, store.ErrBusy) {
		writeError(w, http.StatusConflict, "a download or import of this show started while deleting it; delete it again."+cancelled)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "series is not in the catalogue")
		return
	}
	if err != nil {
		s.log.Error("delete series", "series", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not delete the show; it stays."+cancelled)
		return
	}
	if keep {
		s.log.Info("deleted a show, kept its files", "series", id, "files", len(paths))
		s.events.publish(Event{Type: "series.deleted", Data: map[string]int64{"id": id}})
		writeJSON(w, http.StatusOK, map[string]any{"files": 0, "kept": len(paths), "bytes": size, "problems": []string{}})
		return
	}
	removed, problems := importer.DeleteFiles(id, paths)
	if len(problems) > 0 {
		s.log.Error("delete series files", "series", id, "problems", problems)
	}
	if s.imports != nil {
		s.imports.Removed(ctx, removed)
	}
	s.log.Info("deleted a show", "series", id, "files", len(removed))
	s.events.publish(Event{Type: "series.deleted", Data: map[string]int64{"id": id}})
	if problems == nil {
		problems = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": len(removed), "kept": 0, "bytes": size, "problems": problems})
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
