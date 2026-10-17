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

// Search is the latest search for an episode.
type Search struct {
	EpisodeID int64 `json:"episode_id"`
	// Season is set instead for a season search, which keeps only packs.
	Season   int       `json:"season,omitempty"`
	Searched time.Time `json:"searched"`
	Outcome  string    `json:"outcome"` // found, fallback, nothing or failed
	Summary  string    `json:"summary"`
	// NextRetry is stored as the search's own retry, for the record; the
	// server answers with the monitor's next search instead, unset when
	// the monitor does not get the episode.
	NextRetry  *time.Time      `json:"next_retry,omitempty"`
	Indexers   []SearchIndexer `json:"indexers"`
	Candidates []Candidate     `json:"candidates"`
}

// SearchIndexer is what one indexer returned.
type SearchIndexer struct {
	Name    string `json:"name"`
	Results int    `json:"results"`
	Error   string `json:"error,omitempty"`
}

// Candidate is a release a search found, with the verdict on it.
type Candidate struct {
	ID      int64  `json:"id"`
	Rank    int    `json:"rank"`
	Indexer string `json:"indexer"`
	Title   string `json:"title"`
	// Link downloads the release through Prowlarr and holds its API key. It
	// is stored but never loaded for or sent to clients.
	Link      string     `json:"-"`
	Size      int64      `json:"size"`
	Published *time.Time `json:"published,omitempty"`
	Grabs     int        `json:"grabs"`
	Verdict   string     `json:"verdict"`
	Tier      string     `json:"tier,omitempty"`
	Audio     []string   `json:"audio"`
	Reasons   []string   `json:"reasons"`
	// Decisive counts the leading reasons that decided the verdict: why it
	// was rejected, or why it is only a last resort. The rest are facts.
	Decisive int     `json:"decisive"`
	Episodes []int64 `json:"episodes"`
}

// SeriesOfEpisode returns the show an episode belongs to.
func (s *Store) SeriesOfEpisode(ctx context.Context, episodeID int64) (int64, error) {
	id, err := s.q.SeriesOfEpisode(ctx, episodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("episode %d: %w", episodeID, ErrNotFound)
	}
	return id, err
}

// SaveSearch replaces the episode's previous search. Candidates are saved
// in the order given; their IDs are assigned here.
func (s *Store) SaveSearch(ctx context.Context, sr Search) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	if err := q.DeleteSearch(ctx, sr.EpisodeID); err != nil {
		return err
	}
	var retry sql.NullInt64
	if sr.NextRetry != nil {
		retry = sql.NullInt64{Int64: sr.NextRetry.Unix(), Valid: true}
	}
	id, err := q.InsertSearch(ctx, query.InsertSearchParams{
		EpisodeID: sr.EpisodeID, SearchedAt: sr.Searched.Unix(), Outcome: sr.Outcome, Summary: sr.Summary, NextRetryAt: retry,
	})
	if err != nil {
		return fmt.Errorf("save search: %w", err)
	}
	for _, ix := range sr.Indexers {
		if err := q.InsertSearchIndexer(ctx, query.InsertSearchIndexerParams{
			SearchID: id, Indexer: ix.Name, Results: int64(ix.Results), Error: nullString(ix.Error),
		}); err != nil {
			return err
		}
	}
	for i, c := range sr.Candidates {
		reasons, _ := json.Marshal(c.Reasons)
		episodes, _ := json.Marshal(c.Episodes)
		var published sql.NullInt64
		if c.Published != nil {
			published = sql.NullInt64{Int64: c.Published.Unix(), Valid: true}
		}
		if err := q.InsertCandidate(ctx, query.InsertCandidateParams{
			SearchID: id, Rank: int64(i + 1), Indexer: c.Indexer, Title: c.Title, Link: c.Link, Size: c.Size,
			Published: published, Grabs: int64(c.Grabs), Verdict: c.Verdict, Tier: c.Tier,
			Audio: strings.Join(c.Audio, ","), Reasons: string(reasons), Episodes: string(episodes),
			Decisive: int64(c.Decisive),
		}); err != nil {
			return fmt.Errorf("save candidate %q: %w", c.Title, err)
		}
	}
	return tx.Commit()
}

// LoadSearch returns the latest search for an episode, without download links.
func (s *Store) LoadSearch(ctx context.Context, episodeID int64) (Search, error) {
	row, err := s.q.GetSearch(ctx, episodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return Search{}, fmt.Errorf("search for episode %d: %w", episodeID, ErrNotFound)
	}
	if err != nil {
		return Search{}, err
	}
	sr := Search{
		EpisodeID: row.EpisodeID, Searched: time.Unix(row.SearchedAt, 0), Outcome: row.Outcome, Summary: row.Summary,
		Indexers: []SearchIndexer{}, Candidates: []Candidate{},
	}
	if row.NextRetryAt.Valid {
		t := time.Unix(row.NextRetryAt.Int64, 0)
		sr.NextRetry = &t
	}
	indexers, err := s.q.ListSearchIndexers(ctx, row.ID)
	if err != nil {
		return sr, err
	}
	for _, ix := range indexers {
		sr.Indexers = append(sr.Indexers, SearchIndexer{Name: ix.Indexer, Results: int(ix.Results), Error: ix.Error.String})
	}
	candidates, err := s.q.ListCandidates(ctx, row.ID)
	if err != nil {
		return sr, err
	}
	for _, c := range candidates {
		cand := Candidate{
			ID: c.ID, Rank: int(c.Rank), Indexer: c.Indexer, Title: c.Title, Size: c.Size, Grabs: int(c.Grabs),
			Verdict: c.Verdict, Tier: c.Tier, Audio: []string{}, Reasons: []string{}, Episodes: []int64{},
			Decisive: int(c.Decisive),
		}
		if c.Published.Valid {
			t := time.Unix(c.Published.Int64, 0)
			cand.Published = &t
		}
		if c.Audio != "" {
			cand.Audio = strings.Split(c.Audio, ",")
		}
		if err := json.Unmarshal([]byte(c.Reasons), &cand.Reasons); err != nil {
			return sr, fmt.Errorf("candidate %d reasons: %w", c.ID, err)
		}
		if err := json.Unmarshal([]byte(c.Episodes), &cand.Episodes); err != nil {
			return sr, fmt.Errorf("candidate %d episodes: %w", c.ID, err)
		}
		sr.Candidates = append(sr.Candidates, cand)
	}
	return sr, nil
}
