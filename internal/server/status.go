package server

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/catalog"
	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/store"
)

// checkTimeout bounds each service check, so a hanging service cannot hold
// up the others.
const checkTimeout = 5 * time.Second

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	checks := []func(context.Context) api.ServiceCheck{s.checkTVDB, s.checkProwlarr, s.checkSABnzbd, s.checkJellyfin}
	out := api.Services{Services: make([]api.ServiceCheck, len(checks))}
	var wg sync.WaitGroup
	for i, check := range checks {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, checkTimeout)
			defer cancel()
			out.Services[i] = check(ctx)
		})
	}
	shows, err := s.store.Library(ctx, s.now(), s.schedule())
	wg.Wait()
	if err != nil {
		s.internalError(w, "library totals", err)
		return
	}
	out.Library = api.LibraryTotals{Shows: len(shows), Types: map[string]int{}, Folders: []string{}, MonitorOff: s.monitorOff()}
	for _, sh := range shows {
		out.Library.Types[cmp.Or(sh.Type, store.TypeTV)]++
		if sh.Monitor != store.MonitorOff {
			out.Library.Monitored++
		}
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

func (s *Server) checkTVDB(ctx context.Context) api.ServiceCheck {
	c := api.ServiceCheck{Name: "TVDB"}
	if s.catalog == nil {
		c.Detail = "not set up: TVDB_API_KEY is missing, and this build has no built-in key"
		return c
	}
	key := "your TVDB_API_KEY"
	if s.cfg.TVDB.BuiltIn {
		key = "Garner's built-in key"
	}
	if until, reason := s.catalog.Paused(); !until.IsZero() {
		c.Detail = (&catalog.PausedError{Until: until, Reason: reason}).Error()
		return c
	}
	last, err := s.catalog.LastSync(ctx)
	switch {
	case err != nil:
		c.Detail = err.Error()
	case last.IsZero():
		c.OK, c.Detail = true, "catalogue not synced yet, with "+key
	default:
		c.OK, c.Detail, c.At = true, "catalogue synced, with "+key, &last
	}
	return c
}

func (s *Server) checkProwlarr(ctx context.Context) api.ServiceCheck {
	c := api.ServiceCheck{Name: "Prowlarr"}
	if s.prowlarr == nil {
		c.Detail = "not set up: " + lacking(s.prowlarrMissing())
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

func (s *Server) checkSABnzbd(ctx context.Context) api.ServiceCheck {
	c := api.ServiceCheck{Name: "SABnzbd"}
	if s.sabnzbd == nil {
		c.Detail = "not set up: " + lacking(s.sabnzbdMissing())
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

func (s *Server) checkJellyfin(ctx context.Context) api.ServiceCheck {
	c := api.ServiceCheck{Name: "Jellyfin"}
	if s.jellyfin == nil {
		c.Detail = "not used: importing is unavailable, and Garner's log says why"
		if m := s.jellyfinMissing(); len(m) > 0 {
			c.Detail = "not set up: " + lacking(m)
		}
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

// missing names the settings a service lacks: its url in garner.toml, its
// API key or both. Garner sets up Prowlarr, SABnzbd and Jellyfin only
// with both.
func missing(c config.Service, section, key string) []string {
	var m []string
	if c.URL == "" {
		m = append(m, section+".url")
	}
	if c.APIKey == "" {
		m = append(m, key)
	}
	return m
}

func (s *Server) prowlarrMissing() []string {
	return missing(s.cfg.Prowlarr, "prowlarr", "PROWLARR_API_KEY")
}

func (s *Server) sabnzbdMissing() []string {
	return missing(s.cfg.SABnzbd.Service, "sabnzbd", "SABNZBD_API_KEY")
}

func (s *Server) jellyfinMissing() []string {
	return missing(s.cfg.Jellyfin, "jellyfin", "JELLYFIN_API_KEY")
}

// lacking says which settings are missing, such as "PROWLARR_API_KEY is
// missing".
func lacking(names []string) string {
	switch len(names) {
	case 0:
		return "nothing is missing"
	case 1:
		return names[0] + " is missing"
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " are missing"
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
