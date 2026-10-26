package store

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/trucke/garner/internal/profile"
)

// shippedText is a shipped profile's text, as the migration writes it.
func shippedText(t *testing.T, name string) string {
	t.Helper()
	for _, p := range profile.Shipped() {
		if p.Name == name {
			return profile.Encode(p)
		}
	}
	t.Fatalf("no shipped profile %s", name)
	return ""
}

// TestMigrationProfiles checks migration 19 on an installation from
// before: the tv and anime profiles become German and German, English
// fallback, the defaults of their types, and every show keeps its rules.
// A show never loaded from TVDB stays without type and profile until a
// refresh, which gives it the defaults.
func TestMigrationProfiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openAt(t, dir, 18)
	for _, s := range []struct {
		id      int64
		profile any
	}{{42, "tv"}, {43, "anime"}, {44, nil}} {
		exec(t, db, `INSERT INTO series (id, slug, name, original_language, original_country, status, default_order, tvdb_updated,
			added_at, fetched_at, profile, monitor, monitor_since) VALUES (?, 's', 'Harbor Precinct', 'eng', 'usa', 'Ended', 'official',
			'2026-01-01', 1, 1, ?, 'new', 1)`, s.id, s.profile)
	}
	db.Close()
	st, err := Open(ctx, dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ps, err := st.Profiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("profiles %+v, want German and German, English fallback", ps)
	}
	german, gef := ps[0], ps[1]
	if german.Name != "German" || !german.DefaultTV || german.DefaultAnime || german.Text != shippedText(t, "German") || german.Shows != 1 {
		t.Errorf("German: %+v", german)
	}
	if gef.Name != "German, English fallback" || gef.DefaultTV || !gef.DefaultAnime || gef.Text != shippedText(t, gef.Name) || gef.Shows != 1 {
		t.Errorf("German, English fallback: %+v", gef)
	}
	for id, want := range map[int64]struct {
		typ     string
		profile int64
	}{42: {TypeTV, german.ID}, 43: {TypeAnime, gef.ID}, 44: {"", 0}} {
		d, err := st.LoadSeries(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if d.Series.Type != want.typ || d.Series.ProfileID != want.profile {
			t.Errorf("show %d: type %q, profile %d; want %q, %d", id, d.Series.Type, d.Series.ProfileID, want.typ, want.profile)
		}
	}
	d, _ := st.LoadSeries(ctx, 44)
	d.Series.Type = TypeAnime
	if err := st.SaveSeries(ctx, d, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.LoadSeries(ctx, 44); d.Series.Type != TypeAnime || d.Series.ProfileID != gef.ID {
		t.Errorf("refreshed show: type %q, profile %d", d.Series.Type, d.Series.ProfileID)
	}
}

// TestFreshProfiles starts a new installation: one profile, Original
// language, the default for both types.
func TestFreshProfiles(t *testing.T) {
	st := openStore(t)
	ps, err := st.Profiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Name != "Original language" || !ps[0].DefaultTV || !ps[0].DefaultAnime || ps[0].Text != shippedText(t, "Original language") {
		t.Errorf("profiles %+v", ps)
	}
}

// TestProfiles keeps the profile rules: names stay unique, a profile that
// shows use or that is a default is not deleted, each type keeps exactly
// one default, and changing a profile's rules drops the search results
// they judged.
func TestProfiles(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	german := shippedText(t, "German")
	id, err := st.CreateProfile(ctx, "German", german)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProfile(ctx, "German", german); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a second German: %v", err)
	}
	original, err := st.ProfileByName(ctx, "Original language")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateProfile(ctx, id, "Original language", german); !errors.Is(err, ErrNameTaken) {
		t.Errorf("renamed to a taken name: %v", err)
	}

	// A show uses German; its search was judged by German's rules.
	saveShow(t, st, 42)
	if err := st.ChangeProfile(ctx, 42, id); err != nil {
		t.Fatal(err)
	}
	if err := st.ChangeProfile(ctx, 42, 999); !errors.Is(err, ErrNoProfile) {
		t.Errorf("unknown profile: %v", err)
	}
	if err := st.SaveSearch(ctx, Search{EpisodeID: 101, Searched: time.Now(), Outcome: "nothing", Summary: "-"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateProfile(ctx, id, "German", german); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadSearch(ctx, 101); err == nil {
		t.Error("the search judged by the old rules survived the change")
	}

	var inUse *ProfileInUseError
	if err := st.DeleteProfile(ctx, id); !errors.As(err, &inUse) || !slices.Equal(inUse.Shows, []string{"Harbor Precinct"}) || inUse.DefaultTV {
		t.Errorf("delete a profile a show uses: %v", err)
	}
	if err := st.DeleteProfile(ctx, original.ID); !errors.As(err, &inUse) || !inUse.DefaultTV || !inUse.DefaultAnime || len(inUse.Shows) > 0 {
		t.Errorf("delete a default: %v", err)
	}

	// German becomes the TV default in place of Original language.
	if err := st.SetDefaultProfile(ctx, TypeTV, id); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultProfile(ctx, TypeTV, 999); !errors.Is(err, ErrNoProfile) {
		t.Errorf("unknown default: %v", err)
	}
	if def, err := st.DefaultProfile(ctx, TypeTV); err != nil || def.ID != id {
		t.Errorf("TV default: %+v, %v", def, err)
	}
	ps, err := st.Profiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.DefaultTV != (p.ID == id) {
			t.Errorf("%s: TV default %v", p.Name, p.DefaultTV)
		}
	}

	// Once the show uses Original language, German is still the TV default.
	if err := st.ChangeProfile(ctx, 42, original.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteProfile(ctx, id); !errors.As(err, &inUse) || !inUse.DefaultTV || len(inUse.Shows) > 0 {
		t.Errorf("delete the TV default: %v", err)
	}
	spare, err := st.CreateProfile(ctx, "Spare", shippedText(t, "English"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteProfile(ctx, spare); err != nil {
		t.Errorf("delete an unused profile: %v", err)
	}
	if _, err := st.Profile(ctx, spare); !errors.Is(err, ErrNoProfile) {
		t.Errorf("deleted profile: %v", err)
	}
}

// TestChangeType switches a show between TV and anime: its search results
// go, as they were matched with the old numbering.
func TestChangeType(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	saveShow(t, st, 42)
	if err := st.SaveSearch(ctx, Search{EpisodeID: 101, Searched: time.Now(), Outcome: "nothing", Summary: "-"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ChangeType(ctx, 42, "cartoon"); err == nil {
		t.Error("an unknown type was accepted")
	}
	if err := st.ChangeType(ctx, 42, TypeAnime); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.LoadSeries(ctx, 42); d.Series.Type != TypeAnime {
		t.Errorf("type %q", d.Series.Type)
	}
	if _, err := st.LoadSearch(ctx, 101); err == nil {
		t.Error("the search matched with the old numbering survived the change")
	}
	if err := st.ChangeType(ctx, 999, TypeTV); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown show: %v", err)
	}
}

// saveShow adds a TV show with one episode, 101.
func saveShow(t *testing.T, st *Store, id int64) {
	t.Helper()
	if err := st.SaveSeries(context.Background(), SeriesData{
		Series: Series{ID: id, Slug: "harbor-precinct", Name: "Harbor Precinct", OriginalLanguage: "eng", OriginalCountry: "usa",
			Status: "Ended", DefaultOrder: "official", TVDBUpdated: "2026-01-01", Type: TypeTV},
		Episodes: []Episode{{ID: 101, Aired: "2026-01-01", Numbers: map[string]Position{"official": {Season: 1, Number: 1}}}},
	}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// openStore opens a new database in a temporary folder.
func openStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
