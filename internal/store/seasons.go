package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/trucke/garner/internal/store/query"
)

// SaveSeasonSearch replaces the season's previous search. owners name, for
// each candidate, the episode a download of it belongs to.
func (s *Store) SaveSeasonSearch(ctx context.Context, seriesID int64, sr Search, owners []int64) error {
	if len(owners) != len(sr.Candidates) {
		return fmt.Errorf("season search: %d owners for %d candidates", len(owners), len(sr.Candidates))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	season := int64(sr.Season)
	if err := q.DeleteSeasonSearch(ctx, query.DeleteSeasonSearchParams{SeriesID: seriesID, Season: season}); err != nil {
		return err
	}
	indexers, _ := json.Marshal(sr.Indexers)
	id, err := q.InsertSeasonSearch(ctx, query.InsertSeasonSearchParams{
		SeriesID: seriesID, Season: season, SearchedAt: sr.Searched.Unix(), Outcome: sr.Outcome, Summary: sr.Summary,
		Indexers: string(indexers),
	})
	if err != nil {
		return fmt.Errorf("save season search: %w", err)
	}
	for i, c := range sr.Candidates {
		reasons, _ := json.Marshal(c.Reasons)
		episodes, _ := json.Marshal(c.Episodes)
		var published sql.NullInt64
		if c.Published != nil {
			published = sql.NullInt64{Int64: c.Published.Unix(), Valid: true}
		}
		if err := q.InsertSeasonCandidate(ctx, query.InsertSeasonCandidateParams{
			SearchID: id, EpisodeID: owners[i], Rank: int64(i + 1), Indexer: c.Indexer, Title: c.Title, Link: c.Link,
			Size: c.Size, Published: published, Verdict: c.Verdict, Tier: c.Tier, Audio: strings.Join(c.Audio, ","),
			Reasons: string(reasons), Decisive: int64(c.Decisive), Episodes: string(episodes),
		}); err != nil {
			return fmt.Errorf("save season candidate %q: %w", c.Title, err)
		}
	}
	return tx.Commit()
}

// LoadSeasonSearch returns a season's latest search.
func (s *Store) LoadSeasonSearch(ctx context.Context, seriesID int64, season int) (Search, error) {
	row, err := s.q.GetSeasonSearch(ctx, query.GetSeasonSearchParams{SeriesID: seriesID, Season: int64(season)})
	if errors.Is(err, sql.ErrNoRows) {
		return Search{}, fmt.Errorf("search for season %d: %w", season, ErrNotFound)
	}
	if err != nil {
		return Search{}, err
	}
	sr := Search{Season: season, Searched: time.Unix(row.SearchedAt, 0), Outcome: row.Outcome, Summary: row.Summary,
		Indexers: []SearchIndexer{}, Candidates: []Candidate{}}
	if err := json.Unmarshal([]byte(row.Indexers), &sr.Indexers); err != nil {
		return sr, fmt.Errorf("season search %d indexers: %w", row.ID, err)
	}
	rows, err := s.q.ListSeasonCandidates(ctx, row.ID)
	if err != nil {
		return sr, err
	}
	for _, c := range rows {
		cand := Candidate{
			ID: c.ID, Rank: int(c.Rank), Indexer: c.Indexer, Title: c.Title, Size: c.Size, Verdict: c.Verdict, Tier: c.Tier,
			Audio: []string{}, Reasons: []string{}, Episodes: []int64{}, Decisive: int(c.Decisive),
		}
		if c.Published.Valid {
			t := time.Unix(c.Published.Int64, 0)
			cand.Published = &t
		}
		if c.Audio != "" {
			cand.Audio = strings.Split(c.Audio, ",")
		}
		if err := json.Unmarshal([]byte(c.Reasons), &cand.Reasons); err != nil {
			return sr, fmt.Errorf("season candidate %d reasons: %w", c.ID, err)
		}
		if err := json.Unmarshal([]byte(c.Episodes), &cand.Episodes); err != nil {
			return sr, fmt.Errorf("season candidate %d episodes: %w", c.ID, err)
		}
		sr.Candidates = append(sr.Candidates, cand)
	}
	return sr, nil
}

// SeasonCandidateLink returns a season search's candidate with its
// download link and the episode a download of it belongs to.
func (s *Store) SeasonCandidateLink(ctx context.Context, id int64) (Candidate, int64, error) {
	r, err := s.q.GetSeasonCandidateLink(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Candidate{}, 0, fmt.Errorf("season candidate %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Candidate{}, 0, err
	}
	c := Candidate{ID: r.ID, Title: r.Title, Indexer: r.Indexer, Link: r.Link, Size: r.Size, Verdict: r.Verdict, Tier: r.Tier,
		SearchSeason: int(r.Season), Decisive: int(r.Decisive)}
	if err := json.Unmarshal([]byte(r.Reasons), &c.Reasons); err != nil {
		return Candidate{}, 0, fmt.Errorf("season candidate %d reasons: %w", r.ID, err)
	}
	return c, r.EpisodeID, nil
}
