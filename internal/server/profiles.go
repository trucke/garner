package server

import (
	"cmp"
	"context"
	"fmt"
	"sync"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/profile"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/selection"
	"github.com/trucke/garner/internal/store"
)

// profileCache holds the stored profiles parsed, so applying one needs no
// database query: the store asks for a show's upgrade rule while it reads
// rows. Every change to a profile goes through the server, which reloads
// the cache after it.
type profileCache struct {
	mu       sync.RWMutex
	byID     map[int64]profile.Profile
	order    []int64          // by name, as the store lists them
	defaults map[string]int64 // type → profile ID
}

// loadProfiles reads the stored profiles into the cache. Garner saves only
// profiles that pass the check, so one that does not is a bug.
func (s *Server) loadProfiles(ctx context.Context) error {
	stored, err := s.store.Profiles(ctx)
	if err != nil {
		return err
	}
	byID, defaults := map[int64]profile.Profile{}, map[string]int64{}
	order := make([]int64, 0, len(stored))
	for _, sp := range stored {
		p, _, err := profile.Parse(sp.Text)
		if err != nil {
			return fmt.Errorf("profile %q in the database: %w", sp.Name, err)
		}
		byID[sp.ID] = p
		order = append(order, sp.ID)
		if sp.DefaultTV {
			defaults[api.TypeTV] = sp.ID
		}
		if sp.DefaultAnime {
			defaults[api.TypeAnime] = sp.ID
		}
	}
	for _, typ := range []string{api.TypeTV, api.TypeAnime} {
		if _, ok := defaults[typ]; !ok {
			return fmt.Errorf("the database has no default profile for new %s shows", typ)
		}
	}
	s.profiles.mu.Lock()
	defer s.profiles.mu.Unlock()
	s.profiles.byID, s.profiles.order, s.profiles.defaults = byID, order, defaults
	return nil
}

// storedProfile returns a show's profile. A show loaded before it had one,
// and not refreshed since, gets the default for its type.
func (s *Server) storedProfile(sr store.Series) profile.Profile {
	s.profiles.mu.RLock()
	defer s.profiles.mu.RUnlock()
	if p, ok := s.profiles.byID[sr.ProfileID]; ok {
		return p
	}
	return s.profiles.byID[s.profiles.defaults[cmp.Or(sr.Type, api.TypeTV)]]
}

// profileFor applies a show's profile to the show.
func (s *Server) profileFor(sr store.Series) selection.Profile {
	return selection.FromProfile(s.storedProfile(sr), selection.OriginalLanguage(sr.OriginalLanguage))
}

// defaultProfileFor applies the profile new shows of a type get to a show
// whose original language is original.
func (s *Server) defaultProfileFor(typ string, original release.Language) selection.Profile {
	return selection.FromProfile(s.storedProfile(store.Series{Type: typ}), original)
}

// profileInfos lists every profile as applied to a show, for its profile
// choice.
func (s *Server) profileInfos(sr store.Series) []api.ProfileInfo {
	s.profiles.mu.RLock()
	defer s.profiles.mu.RUnlock()
	original := selection.OriginalLanguage(sr.OriginalLanguage)
	out := make([]api.ProfileInfo, 0, len(s.profiles.order))
	for _, id := range s.profiles.order {
		p := s.profiles.byID[id]
		applied := selection.FromProfile(p, original)
		out = append(out, api.ProfileInfo{ID: id, Name: p.Name, Rules: applied.Describe(), Preferences: applied.Preferences()})
	}
	return out
}
