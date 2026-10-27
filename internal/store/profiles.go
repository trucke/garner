package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/trucke/garner/internal/store/query"
)

// StoredProfile is a profile as the database keeps it: its text in the
// format users share (docs/profiles.md), which the caller checks before
// saving, and whether new TV or anime shows get it.
type StoredProfile struct {
	ID           int64
	Name, Text   string
	DefaultTV    bool
	DefaultAnime bool
	// Shows counts the shows that use the profile; Profiles fills it in.
	Shows int64
}

var (
	// ErrNoProfile means no profile has that ID.
	ErrNoProfile = errors.New("no such profile")
	// ErrNameTaken means another profile has that name.
	ErrNameTaken = errors.New("another profile has that name")
)

// ProfileInUseError says why a profile cannot be deleted: shows use it, or
// new shows of a type get it.
type ProfileInUseError struct {
	Name                    string
	Shows                   []string
	DefaultTV, DefaultAnime bool
}

func (e *ProfileInUseError) Error() string {
	var why []string
	if len(e.Shows) > 0 {
		why = append(why, "shows use it: "+strings.Join(e.Shows, ", "))
	}
	if e.DefaultTV {
		why = append(why, "it is the default for new TV shows")
	}
	if e.DefaultAnime {
		why = append(why, "it is the default for new anime shows")
	}
	return fmt.Sprintf("%s can't be deleted: %s", e.Name, strings.Join(why, "; "))
}

func storedProfile(id int64, name, text string, defaultTV, defaultAnime int64) StoredProfile {
	return StoredProfile{ID: id, Name: name, Text: text, DefaultTV: defaultTV == 1, DefaultAnime: defaultAnime == 1}
}

// Profiles lists every profile by name, with how many shows use each.
func (s *Store) Profiles(ctx context.Context) ([]StoredProfile, error) {
	rows, err := s.q.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]StoredProfile, len(rows))
	for i, r := range rows {
		out[i] = storedProfile(r.ID, r.Name, r.Text, r.DefaultTv, r.DefaultAnime)
		out[i].Shows = r.Shows
	}
	return out, nil
}

// Profile returns one profile, or ErrNoProfile.
func (s *Store) Profile(ctx context.Context, id int64) (StoredProfile, error) {
	r, err := s.q.GetProfile(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredProfile{}, fmt.Errorf("profile %d: %w", id, ErrNoProfile)
	}
	if err != nil {
		return StoredProfile{}, err
	}
	return storedProfile(r.ID, r.Name, r.Text, r.DefaultTv, r.DefaultAnime), nil
}

// ProfileByName returns the profile with that name, or ErrNoProfile.
func (s *Store) ProfileByName(ctx context.Context, name string) (StoredProfile, error) {
	r, err := s.q.GetProfileByName(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredProfile{}, fmt.Errorf("profile %q: %w", name, ErrNoProfile)
	}
	if err != nil {
		return StoredProfile{}, err
	}
	return storedProfile(r.ID, r.Name, r.Text, r.DefaultTv, r.DefaultAnime), nil
}

// ProfileShowNames names the shows that use a profile, by name.
func (s *Store) ProfileShowNames(ctx context.Context, id int64) ([]string, error) {
	rows, err := s.q.ProfileShows(ctx, sql.NullInt64{Int64: id, Valid: true})
	if err != nil {
		return nil, err
	}
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Name
	}
	return names, nil
}

// DefaultProfile returns the profile new shows of a type get.
func (s *Store) DefaultProfile(ctx context.Context, typ string) (StoredProfile, error) {
	return defaultProfile(ctx, s.q, typ)
}

// defaultProfile returns the profile new shows of a type get. Every type
// has one: the migration sets both, and a default can be moved but not
// deleted.
func defaultProfile(ctx context.Context, q *query.Queries, typ string) (StoredProfile, error) {
	var r query.Profile
	var err error
	switch typ {
	case TypeTV:
		r, err = q.GetDefaultTVProfile(ctx)
	case TypeAnime:
		r, err = q.GetDefaultAnimeProfile(ctx)
	default:
		return StoredProfile{}, fmt.Errorf("type %q is neither %s nor %s", typ, TypeTV, TypeAnime)
	}
	if errors.Is(err, sql.ErrNoRows) {
		panic(fmt.Sprintf("store: every type has a default profile, but %s has none", typ))
	}
	if err != nil {
		return StoredProfile{}, err
	}
	return storedProfile(r.ID, r.Name, r.Text, r.DefaultTv, r.DefaultAnime), nil
}

// CreateProfile saves a new profile, or fails with ErrNameTaken.
func (s *Store) CreateProfile(ctx context.Context, name, text string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	if _, err := q.GetProfileByName(ctx, name); err == nil {
		return 0, fmt.Errorf("profile %q: %w", name, ErrNameTaken)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	id, err := q.InsertProfile(ctx, query.InsertProfileParams{Name: name, Text: text})
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// UpdateProfile replaces a profile's name and text, and drops the search
// results of the shows that use it: they were judged by the old rules.
func (s *Store) UpdateProfile(ctx context.Context, id int64, name, text string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	if _, err := q.GetProfile(ctx, id); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("profile %d: %w", id, ErrNoProfile)
	} else if err != nil {
		return err
	}
	if other, err := q.GetProfileByName(ctx, name); err == nil && other.ID != id {
		return fmt.Errorf("profile %q: %w", name, ErrNameTaken)
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := q.UpdateProfile(ctx, query.UpdateProfileParams{Name: name, Text: text, ID: id}); err != nil {
		return err
	}
	shows, err := q.ProfileShows(ctx, sql.NullInt64{Int64: id, Valid: true})
	if err != nil {
		return err
	}
	for _, sh := range shows {
		if err := clearJudgements(ctx, q, sh.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteProfile deletes a profile that no show uses and that is no
// default; otherwise it fails with a *ProfileInUseError. Shows switching
// to another profile without notice could change the language Garner
// downloads.
func (s *Store) DeleteProfile(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	p, err := q.GetProfile(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("profile %d: %w", id, ErrNoProfile)
	}
	if err != nil {
		return err
	}
	shows, err := q.ProfileShows(ctx, sql.NullInt64{Int64: id, Valid: true})
	if err != nil {
		return err
	}
	if len(shows) > 0 || p.DefaultTv == 1 || p.DefaultAnime == 1 {
		e := &ProfileInUseError{Name: p.Name, DefaultTV: p.DefaultTv == 1, DefaultAnime: p.DefaultAnime == 1}
		for _, sh := range shows {
			e.Shows = append(e.Shows, sh.Name)
		}
		return e
	}
	if _, err := q.DeleteProfile(ctx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetDefaultProfile makes a profile the one new shows of a type get, in
// place of the type's earlier default.
func (s *Store) SetDefaultProfile(ctx context.Context, typ string, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	var n int64
	switch typ {
	case TypeTV:
		if err = q.ClearDefaultTV(ctx); err == nil {
			n, err = q.SetDefaultTV(ctx, id)
		}
	case TypeAnime:
		if err = q.ClearDefaultAnime(ctx); err == nil {
			n, err = q.SetDefaultAnime(ctx, id)
		}
	default:
		return fmt.Errorf("type %q is neither %s nor %s", typ, TypeTV, TypeAnime)
	}
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("profile %d: %w", id, ErrNoProfile)
	}
	return tx.Commit()
}
