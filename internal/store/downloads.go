package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/trucke/garner/internal/store/query"
)

// ErrDownloadRunning means the episode already has a download in progress.
var ErrDownloadRunning = errors.New("a download for this episode is already running")

// Download states.
const (
	Submitting  = "submitting" // recorded, SABnzbd not yet confirmed
	Queued      = "queued"
	Downloading = "downloading"
	Processing  = "processing" // SABnzbd repairs, unpacks or moves the files
	Completed   = "completed"
	Failed      = "failed"
)

// Download is a release handed to SABnzbd.
type Download struct {
	ID        int64  `json:"id"`
	EpisodeID int64  `json:"episode_id"`
	Title     string `json:"title"`
	Indexer   string `json:"indexer"`
	// Link holds Prowlarr's API key; it is never sent to clients.
	Link    string `json:"-"`
	Size    int64  `json:"size"`
	Verdict string `json:"verdict"`
	Tier    string `json:"tier,omitempty"`
	Manual  bool   `json:"manual"`
	State   string `json:"state"`
	// Detail is SABnzbd's own status or running step, such as "Repairing".
	Detail   string  `json:"detail,omitempty"`
	JobID    string  `json:"job_id,omitempty"`
	Progress float64 `json:"progress"`
	TimeLeft string  `json:"time_left,omitempty"`
	// Storage is where SABnzbd put the files, as SABnzbd sees its disk.
	Storage string `json:"storage,omitempty"`
	Failure string `json:"failure,omitempty"`
	// Retryable means another release may succeed where this one failed.
	Retryable bool      `json:"retryable,omitempty"`
	Stages    []Stage   `json:"stages"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

// Stage is one of SABnzbd's post-processing steps and what happened in it.
type Stage struct {
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// Active reports whether the download is still running.
func (d Download) Active() bool {
	switch d.State {
	case Submitting, Queued, Downloading, Processing:
		return true
	}
	return false
}

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
	id, err := q.InsertDownload(ctx, query.InsertDownloadParams{
		EpisodeID: d.EpisodeID, Title: d.Title, Indexer: d.Indexer, Link: d.Link, Size: d.Size, Verdict: d.Verdict,
		Tier: d.Tier, Manual: boolInt(d.Manual), CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
	})
	if err != nil {
		return Download{}, fmt.Errorf("record download: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Download{}, err
	}
	return s.GetDownload(ctx, id)
}

// UpdateDownload saves a download's progress and outcome.
func (s *Store) UpdateDownload(ctx context.Context, d Download, now time.Time) error {
	stages, _ := json.Marshal(d.Stages)
	return s.q.UpdateDownload(ctx, query.UpdateDownloadParams{
		State: d.State, Detail: d.Detail, JobID: nullString(d.JobID), Progress: d.Progress, TimeLeft: d.TimeLeft,
		Storage: d.Storage, Failure: d.Failure, Retryable: boolInt(d.Retryable), Stages: string(stages),
		UpdatedAt: now.Unix(), ID: d.ID,
	})
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

// FailedTitles lists the releases that failed for an episode.
func (s *Store) FailedTitles(ctx context.Context, episodeID int64) ([]string, error) {
	return s.q.ListFailedTitles(ctx, episodeID)
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
		out[i] = Candidate{ID: r.ID, Title: r.Title, Indexer: r.Indexer, Link: r.Link, Size: r.Size, Verdict: r.Verdict, Tier: r.Tier}
	}
	return out, nil
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
	return Candidate{ID: r.ID, Title: r.Title, Indexer: r.Indexer, Link: r.Link, Size: r.Size, Verdict: r.Verdict, Tier: r.Tier}, r.EpisodeID, nil
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
		ID: r.ID, EpisodeID: r.EpisodeID, Title: r.Title, Indexer: r.Indexer, Link: r.Link, Size: r.Size,
		Verdict: r.Verdict, Tier: r.Tier, Manual: r.Manual != 0, State: r.State, Detail: r.Detail, JobID: r.JobID.String,
		Progress: r.Progress, TimeLeft: r.TimeLeft, Storage: r.Storage, Failure: r.Failure, Retryable: r.Retryable != 0,
		Created: time.Unix(r.CreatedAt, 0), Updated: time.Unix(r.UpdatedAt, 0),
	}
	if err := json.Unmarshal([]byte(r.Stages), &d.Stages); err != nil {
		return d, fmt.Errorf("download %d stages: %w", r.ID, err)
	}
	if d.Stages == nil {
		d.Stages = []Stage{}
	}
	return d, nil
}
