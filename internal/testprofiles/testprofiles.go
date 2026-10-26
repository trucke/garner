// Package testprofiles gives tests the rules of the tv and anime profiles
// from before profiles moved into the database: the shipped profiles
// "German" and "German, English fallback". Only tests import it.
package testprofiles

import (
	"context"
	"testing"

	"github.com/trucke/garner/internal/profile"
	"github.com/trucke/garner/internal/selection"
	"github.com/trucke/garner/internal/store"
)

// German and GermanEnglish are the shipped profiles.
var (
	German        = shipped("German")
	GermanEnglish = shipped("German, English fallback")
)

func shipped(name string) profile.Profile {
	for _, p := range profile.Shipped() {
		if p.Name == name {
			return p
		}
	}
	panic("testprofiles: no shipped profile " + name)
}

// ByType applies German to TV shows and GermanEnglish to anime, as the
// server applies a show's stored profile.
func ByType(sr store.Series) selection.Profile {
	p := German
	if sr.Type == store.TypeAnime {
		p = GermanEnglish
	}
	return selection.FromProfile(p, selection.OriginalLanguage(sr.OriginalLanguage))
}

// Install saves both profiles into a store as the defaults for new TV and
// anime shows, as the migration does for an installation from before.
func Install(t testing.TB, st *store.Store) (tv, anime int64) {
	t.Helper()
	ctx := context.Background()
	for _, p := range []struct {
		profile profile.Profile
		typ     string
		id      *int64
	}{{German, store.TypeTV, &tv}, {GermanEnglish, store.TypeAnime, &anime}} {
		id, err := st.CreateProfile(ctx, p.profile.Name, profile.Encode(p.profile))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetDefaultProfile(ctx, p.typ, id); err != nil {
			t.Fatal(err)
		}
		*p.id = id
	}
	return tv, anime
}
