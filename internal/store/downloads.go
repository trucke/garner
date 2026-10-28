package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/trucke/garner/internal/httpx"
	"github.com/trucke/garner/internal/store/query"
)

var (
	// ErrDownloadRunning means the episode already has a download in progress.
	ErrDownloadRunning = errors.New("a download for this episode is already running")
	// ErrCancelled means the download was cancelled; it changes no more.
	ErrCancelled = errors.New("the download was cancelled")
	// ErrNotRunning means the download has already ended.
	ErrNotRunning = errors.New("the download is no longer running")
	// ErrCannotAcknowledge means the attempt was superseded, is not a
	// failure, or has an import problem that must stay actionable.
	ErrCannotAcknowledge = errors.New("only the latest failed download can be cleared; import problems must be fixed or retried")
)

// StartDownload records a download before SABnzbd is asked, refusing a
// second running download for the same episode.
func (s *Store) StartDownload(ctx context.Context, d Download, now time.Time) (Download, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Download{}, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	n, err := q.CountActiveDownloads(ctx, d.EpisodeID)
	if err != nil {
		return Download{}, err
	}
	if n > 0 {
		return Download{}, ErrDownloadRunning
	}
	if d.Reasons == nil {
		d.Reasons = []string{}
	}
	d.Replaces, err = q.GetEpisodeFileTier(ctx, d.EpisodeID)
	if errors.Is(err, sql.ErrNoRows) {
		d.Replaces, err = "", nil
	}
	if err != nil {
		return Download{}, err
	}
	reasons, _ := json.Marshal(d.Reasons)
	id, err := q.InsertDownload(ctx, query.InsertDownloadParams{
		EpisodeID: d.EpisodeID, Title: d.Title, Indexer: d.Indexer, Link: d.Link, Posting: d.Posting, Size: d.Size, Verdict: d.Verdict,
		Tier: d.Tier, Reasons: string(reasons), Decisive: int64(d.Decisive), Manual: boolInt(d.Manual),
		Replaces: d.Replaces, SearchSeason: int64(d.SearchSeason), CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
	})
	if err != nil {
		return Download{}, fmt.Errorf("record download: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Download{}, err
	}
	return s.GetDownload(ctx, id)
}

// UpdateDownload saves a download's progress and outcome. It returns
// ErrCancelled, and saves nothing, when the download was cancelled or
// deleted meanwhile.
func (s *Store) UpdateDownload(ctx context.Context, d Download, now time.Time) error {
	stages, _ := json.Marshal(d.Stages)
	n, err := s.q.UpdateDownload(ctx, query.UpdateDownloadParams{
		State: d.State, Detail: d.Detail, JobID: nullString(d.JobID), Progress: d.Progress, TimeLeft: d.TimeLeft,
		Storage: d.Storage, Failure: d.Failure, Retryable: boolInt(d.Retryable), Stages: string(stages),
		UpdatedAt: now.Unix(), ID: d.ID,
	})
	if err == nil && n == 0 {
		return ErrCancelled
	}
	return err
}

// CancelDownload marks a running download cancelled. It returns
// ErrNotRunning when the download has ended meanwhile.
func (s *Store) CancelDownload(ctx context.Context, id int64, reason string, now time.Time) (Download, error) {
	n, err := s.q.CancelDownload(ctx, query.CancelDownloadParams{Failure: reason, UpdatedAt: now.Unix(), ID: id})
	if err != nil {
		return Download{}, err
	}
	if n == 0 {
		return Download{}, ErrNotRunning
	}
	return s.GetDownload(ctx, id)
}

// AcknowledgeDownload clears only the latest failed attempt's warning. It
// leaves its state, evidence, timestamps and failed-release exclusion alone.
func (s *Store) AcknowledgeDownload(ctx context.Context, id int64, now time.Time) (Download, error) {
	n, err := s.q.AcknowledgeDownload(ctx, query.AcknowledgeDownloadParams{ID: id, AcknowledgedAt: sql.NullInt64{Int64: now.Unix(), Valid: true}})
	if err != nil {
		return Download{}, err
	}
	d, err := s.GetDownload(ctx, id)
	if err != nil {
		return Download{}, err
	}
	if n == 0 {
		return Download{}, ErrCannotAcknowledge
	}
	return d, nil
}

// EpisodeDownloads lists an episode's downloads, newest first.
func (s *Store) EpisodeDownloads(ctx context.Context, episodeID int64) ([]Download, error) {
	rows, err := s.q.ListEpisodeDownloads(ctx, episodeID)
	if err != nil {
		return nil, err
	}
	return downloadsFromRows(rows)
}

// GetDownload returns one download.
func (s *Store) GetDownload(ctx context.Context, id int64) (Download, error) {
	row, err := s.q.GetDownload(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Download{}, fmt.Errorf("download %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Download{}, err
	}
	return downloadFromRow(row)
}

// ActiveDownloads lists the downloads still running.
func (s *Store) ActiveDownloads(ctx context.Context) ([]Download, error) {
	rows, err := s.q.ListActiveDownloads(ctx)
	if err != nil {
		return nil, err
	}
	return downloadsFromRows(rows)
}

// Downloads lists running downloads first, then the most recent.
func (s *Store) Downloads(ctx context.Context, limit int) ([]Download, error) {
	rows, err := s.q.ListDownloads(ctx, int64(limit))
	if err != nil {
		return nil, err
	}
	return downloadsFromRows(rows)
}

// Activity lists each episode's latest download that runs, waits for the
// user (an import that failed or was not confirmed), or ended since since;
// newest first.
func (s *Store) Activity(ctx context.Context, since time.Time) ([]ActivityItem, error) {
	rows, err := s.q.ListActivity(ctx, since.Unix())
	if err != nil {
		return nil, err
	}
	out := make([]ActivityItem, 0, len(rows))
	for _, r := range rows {
		d, err := downloadFromRow(r.Download)
		if err != nil {
			return nil, err
		}
		out = append(out, ActivityItem{Download: d, SeriesID: r.SeriesID, SeriesName: r.SeriesName,
			SeriesNameEng: r.SeriesNameEng.String, Season: int(r.Season.Int64), Number: int(r.Number.Int64)})
	}
	return out, nil
}

// FailedSince counts an episode's failed downloads since t.
func (s *Store) FailedSince(ctx context.Context, episodeID int64, t time.Time) (int, error) {
	n, err := s.q.CountFailedDownloadsSince(ctx, query.CountFailedDownloadsSinceParams{EpisodeID: episodeID, CreatedAt: t.Unix()})
	return int(n), err
}

// CandidateLinks lists the candidates of an episode's latest search, best
// first, with their download links filled in.
func (s *Store) CandidateLinks(ctx context.Context, episodeID int64) ([]Candidate, error) {
	rows, err := s.q.ListCandidateLinks(ctx, episodeID)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, len(rows))
	for i, r := range rows {
		out[i] = Candidate{ID: r.ID, Title: r.Title, Indexer: r.Indexer, Link: r.Link, Posting: r.Posting, Size: r.Size, Verdict: r.Verdict,
			Tier: r.Tier, Decisive: int(r.Decisive)}
		if err := json.Unmarshal([]byte(r.Reasons), &out[i].Reasons); err != nil {
			return nil, fmt.Errorf("candidate %d reasons: %w", r.ID, err)
		}
	}
	return out, nil
}

// DownloadSearch loads the latest results in the scope that started an
// attempt, with links for replacement selection. A newer search replaces
// its own result set, never the other scope's.
func (s *Store) DownloadSearch(ctx context.Context, d Download) (Search, error) {
	if d.SearchSeason == 0 {
		sr, err := s.LoadSearch(ctx, d.EpisodeID)
		if err != nil {
			return Search{}, err
		}
		sr.Candidates, err = s.CandidateLinks(ctx, d.EpisodeID)
		return sr, err
	}
	seriesID, err := s.SeriesOfEpisode(ctx, d.EpisodeID)
	if err != nil {
		return Search{}, err
	}
	row, err := s.q.GetSeasonSearch(ctx, query.GetSeasonSearchParams{SeriesID: seriesID, Season: int64(d.SearchSeason)})
	if errors.Is(err, sql.ErrNoRows) {
		return Search{}, fmt.Errorf("search for season %d: %w", d.SearchSeason, ErrNotFound)
	}
	if err != nil {
		return Search{}, err
	}
	// Bind the candidates to this search, not to a search refreshed while
	// these queries run: the retry budget must use the same search's time.
	rows, err := s.q.ListSeasonCandidateLinks(ctx, query.ListSeasonCandidateLinksParams{SearchID: row.ID, EpisodeID: d.EpisodeID})
	if err != nil {
		return Search{}, err
	}
	sr := Search{Season: d.SearchSeason, Searched: time.Unix(row.SearchedAt, 0), Candidates: make([]Candidate, 0, len(rows))}
	for _, r := range rows {
		c := Candidate{ID: r.ID, Title: r.Title, Indexer: r.Indexer, Link: r.Link, Posting: r.Posting, Size: r.Size, Verdict: r.Verdict,
			Tier: r.Tier, SearchSeason: d.SearchSeason, Decisive: int(r.Decisive)}
		if err := json.Unmarshal([]byte(r.Reasons), &c.Reasons); err != nil {
			return Search{}, fmt.Errorf("season candidate %d reasons: %w", r.ID, err)
		}
		sr.Candidates = append(sr.Candidates, c)
	}
	return sr, nil
}

// CandidateLink returns a candidate with its download link and the episode
// it was found for.
func (s *Store) CandidateLink(ctx context.Context, id int64) (Candidate, int64, error) {
	r, err := s.q.GetCandidateLink(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Candidate{}, 0, fmt.Errorf("candidate %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Candidate{}, 0, err
	}
	c := Candidate{ID: r.ID, Title: r.Title, Indexer: r.Indexer, Link: r.Link, Posting: r.Posting, Size: r.Size, Verdict: r.Verdict,
		Tier: r.Tier, Decisive: int(r.Decisive)}
	if err := json.Unmarshal([]byte(r.Reasons), &c.Reasons); err != nil {
		return Candidate{}, 0, fmt.Errorf("candidate %d reasons: %w", r.ID, err)
	}
	return c, r.EpisodeID, nil
}

func downloadsFromRows(rows []query.Download) ([]Download, error) {
	out := make([]Download, 0, len(rows))
	for _, r := range rows {
		d, err := downloadFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func downloadFromRow(r query.Download) (Download, error) {
	d := Download{
		ID: r.ID, EpisodeID: r.EpisodeID, SearchSeason: int(r.SearchSeason), Title: r.Title, Indexer: r.Indexer, Link: r.Link,
		Posting: r.Posting, Size: r.Size,
		Verdict: r.Verdict, Tier: r.Tier, Decisive: int(r.Decisive), Manual: r.Manual != 0, Replaces: r.Replaces, State: r.State, Detail: r.Detail, JobID: r.JobID.String,
		Progress: r.Progress, TimeLeft: r.TimeLeft, Storage: r.Storage, Failure: r.Failure, Retryable: r.Retryable != 0,
		Created: time.Unix(r.CreatedAt, 0), Updated: time.Unix(r.UpdatedAt, 0),
	}
	if r.AcknowledgedAt.Valid {
		d.Acknowledged = time.Unix(r.AcknowledgedAt.Int64, 0)
	}
	if err := json.Unmarshal([]byte(r.Stages), &d.Stages); err != nil {
		return d, fmt.Errorf("download %d stages: %w", r.ID, err)
	}
	if d.Stages == nil {
		d.Stages = []Stage{}
	}
	// Existing downloads still hold SABnzbd's HTML. Decode before redacting:
	// an HTML-escaped query separator must not hide a credential parameter.
	for i, stage := range d.Stages {
		actions := stage.Lines()
		for j, action := range actions {
			actions[j] = httpx.RedactText(action)
		}
		d.Stages[i].Actions = actions
	}
	if err := json.Unmarshal([]byte(r.Reasons), &d.Reasons); err != nil {
		return d, fmt.Errorf("download %d reasons: %w", r.ID, err)
	}
	d.Import = Import{State: r.ImportState, Detail: r.ImportDetail, Recovery: r.ImportRecovery, Files: []ImportFile{}, Notes: []string{}}
	if r.ImportUpdatedAt > 0 {
		d.Import.Updated = time.Unix(r.ImportUpdatedAt, 0)
	}
	d.FailureSummary = d.SummarizeFailure()
	if err := json.Unmarshal([]byte(r.ImportFiles), &d.Import.Files); err != nil {
		return d, fmt.Errorf("download %d import files: %w", r.ID, err)
	}
	if err := json.Unmarshal([]byte(r.ImportNotes), &d.Import.Notes); err != nil {
		return d, fmt.Errorf("download %d import notes: %w", r.ID, err)
	}
	return d, nil
}

// JobTaken reports whether a download other than downloadID holds
// SABnzbd's job jobID.
func (s *Store) JobTaken(ctx context.Context, jobID string, downloadID int64) (bool, error) {
	n, err := s.q.CountOtherDownloadsWithJob(ctx, query.CountOtherDownloadsWithJobParams{JobID: sql.NullString{String: jobID, Valid: true}, DownloadID: downloadID})
	return n > 0, err
}
