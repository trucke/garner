package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Services is how the services the server uses are doing, and what the
// library holds. Board "About" in the Paper file.
type Services struct {
	Services []ServiceCheck `json:"services"`
	Library  LibraryTotals  `json:"library"`
}

// ServiceCheck is how one service is doing.
type ServiceCheck struct {
	Name string `json:"name"`
	// OK is false when the service is not set up or did not answer.
	OK bool `json:"ok"`
	// Detail is one line: what the service reports, or what went wrong.
	Detail string `json:"detail"`
	// At dates the detail, such as TVDB's last catalogue sync.
	At *time.Time `json:"at,omitempty"`
}

// LibraryTotals sums up the library: shows by profile, aired episodes in
// it, their size, and the library folders as the server sees them.
type LibraryTotals struct {
	Shows    int            `json:"shows"`
	Profiles map[string]int `json:"profiles"`
	Imported int            `json:"imported"`
	Aired    int            `json:"aired"`
	Bytes    int64          `json:"bytes"`
	Folders  []string       `json:"folders"`
}

// checkTimeout bounds each service check, so a hanging service cannot hold
// up the others.
const checkTimeout = 5 * time.Second

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	checks := []func(context.Context) ServiceCheck{s.checkTVDB, s.checkProwlarr, s.checkSABnzbd, s.checkJellyfin}
	out := Services{Services: make([]ServiceCheck, len(checks))}
	var wg sync.WaitGroup
	for i, check := range checks {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, checkTimeout)
			defer cancel()
			out.Services[i] = check(ctx)
		})
	}
	shows, err := s.store.Library(ctx, time.Now())
	wg.Wait()
	if err != nil {
		s.internalError(w, "library totals", err)
		return
	}
	out.Library = LibraryTotals{Shows: len(shows), Profiles: map[string]int{}, Folders: []string{}}
	for _, sh := range shows {
		out.Library.Profiles[sh.Profile]++
		out.Library.Imported += sh.Imported
		out.Library.Aired += sh.Aired
		out.Library.Bytes += sh.Bytes
	}
	for _, f := range []string{s.cfg.Library.TV, s.cfg.Library.Anime} {
		if f != "" {
			out.Library.Folders = append(out.Library.Folders, f)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) checkTVDB(ctx context.Context) ServiceCheck {
	c := ServiceCheck{Name: "TVDB"}
	if s.catalog == nil {
		c.Detail = "not set up: TVDB_API_KEY is missing"
		return c
	}
	last, err := s.catalog.LastSync(ctx)
	switch {
	case err != nil:
		c.Detail = err.Error()
	case last.IsZero():
		c.OK, c.Detail = true, "catalogue not synced yet"
	default:
		c.OK, c.Detail, c.At = true, "catalogue synced", &last
	}
	return c
}

func (s *Server) checkProwlarr(ctx context.Context) ServiceCheck {
	c := ServiceCheck{Name: "Prowlarr"}
	if s.prowlarr == nil {
		c.Detail = "not set up: prowlarr.url and PROWLARR_API_KEY"
		return c
	}
	indexers, err := s.prowlarr.Indexers(ctx)
	if err != nil {
		c.Detail = err.Error()
		return c
	}
	names := make([]string, len(indexers))
	for i, ix := range indexers {
		names[i] = ix.Name
	}
	c.OK, c.Detail = true, fmt.Sprintf("%s: %s", count(len(indexers), "indexer"), strings.Join(names, ", "))
	if len(indexers) == 0 {
		c.OK, c.Detail = false, "no enabled Usenet indexers"
	}
	return c
}

func (s *Server) checkSABnzbd(ctx context.Context) ServiceCheck {
	c := ServiceCheck{Name: "SABnzbd"}
	if s.sabnzbd == nil {
		c.Detail = "not set up: sabnzbd.url and SABNZBD_API_KEY"
		return c
	}
	q, err := s.sabnzbd.State(ctx)
	switch {
	case err != nil:
		c.Detail = err.Error()
	case q.Paused:
		c.OK, c.Detail = true, "paused · "+count(q.Jobs, "job")
	case q.Jobs == 0:
		c.OK, c.Detail = true, "idle"
	default:
		c.OK, c.Detail = true, fmt.Sprintf("%s · %.1f MB/s", count(q.Jobs, "job"), q.Speed/1e6)
	}
	return c
}

func (s *Server) checkJellyfin(ctx context.Context) ServiceCheck {
	c := ServiceCheck{Name: "Jellyfin"}
	if s.jellyfin == nil {
		c.Detail = "not set up: jellyfin.url and JELLYFIN_API_KEY"
		return c
	}
	version, shows, err := s.jellyfin.Info(ctx)
	if err != nil {
		c.Detail = err.Error()
		return c
	}
	c.OK, c.Detail = true, fmt.Sprintf("%s · %s in its libraries", version, count(shows, "show"))
	return c
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
