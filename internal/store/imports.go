package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/trucke/garner/internal/store/query"
)

// Import states.
const (
	ImportPending    = "pending"    // waiting for the file check
	ImportMoving     = "moving"     // checked; files are being moved
	ImportScanning   = "scanning"   // moved; waiting for Jellyfin to list them
	ImportDone       = "imported"   // Jellyfin identified every file as intended
	ImportUnverified = "unverified" // moved, but Jellyfin did not confirm the episodes
	ImportRejected   = "rejected"   // the file check found the release bad
	ImportFailed     = "failed"     // a path or permission problem; see the recovery step
)

// Import is the move of a finished download into the library.
type Import struct {
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Recovery says what to do when the import failed.
	Recovery string       `json:"recovery,omitempty"`
	Files    []ImportFile `json:"files"`
	Updated  time.Time    `json:"updated,omitzero"`
}

// ImportFile is one video file of a download.
type ImportFile struct {
	// Source and Target are as Garner reaches them.
	Source   string  `json:"source"`
	Target   string  `json:"target,omitempty"`
	Episodes []int64 `json:"episodes"`
	Size     int64   `json:"size"`
	// Check is the file check's outcome, as inspect.Result.
	Check json.RawMessage `json:"check,omitempty"`
	// Jellyfin is how Jellyfin identified the file.
	Jellyfin string `json:"jellyfin,omitempty"`
	Verified bool   `json:"verified,omitempty"`
}

// MarkImportPending queues a completed download for import, once.
func (s *Store) MarkImportPending(ctx context.Context, downloadID int64, now time.Time) error {
	return s.q.MarkImportPending(ctx, query.MarkImportPendingParams{ImportUpdatedAt: now.Unix(), ID: downloadID})
}

// UpdateImport saves an import's progress.
func (s *Store) UpdateImport(ctx context.Context, downloadID int64, im Import, now time.Time) error {
	if im.Files == nil {
		im.Files = []ImportFile{}
	}
	files, err := json.Marshal(im.Files)
	if err != nil {
		return err
	}
	return s.q.UpdateImport(ctx, query.UpdateImportParams{
		ImportState: im.State, ImportDetail: im.Detail, ImportRecovery: im.Recovery, ImportFiles: string(files),
		ImportUpdatedAt: now.Unix(), ID: downloadID,
	})
}

// DueImports lists downloads whose import has work left.
func (s *Store) DueImports(ctx context.Context) ([]Download, error) {
	rows, err := s.q.ListDueImports(ctx)
	if err != nil {
		return nil, err
	}
	return downloadsFromRows(rows)
}

// EpisodeFile is the file Garner imported for an episode.
type EpisodeFile struct {
	EpisodeID int64
	Path      string
	Size      int64
}

// EpisodeFile returns the episode's imported file, or found=false.
func (s *Store) EpisodeFile(ctx context.Context, episodeID int64) (f EpisodeFile, found bool, err error) {
	r, err := s.q.GetEpisodeFile(ctx, episodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return f, false, nil
	}
	if err != nil {
		return f, false, err
	}
	return EpisodeFile{EpisodeID: r.EpisodeID, Path: r.Path, Size: r.Size}, true, nil
}

// SetEpisodeFile records the file imported for an episode, replacing the
// previous one.
func (s *Store) SetEpisodeFile(ctx context.Context, f EpisodeFile, downloadID int64, now time.Time) error {
	return s.q.UpsertEpisodeFile(ctx, query.UpsertEpisodeFileParams{
		EpisodeID: f.EpisodeID, Path: f.Path, Size: f.Size,
		DownloadID: sql.NullInt64{Int64: downloadID, Valid: downloadID > 0}, ImportedAt: now.Unix(),
	})
}

// FileInUse reports whether any episode's imported file is at path.
func (s *Store) FileInUse(ctx context.Context, path string) (bool, error) {
	n, err := s.q.CountEpisodeFilesAt(ctx, path)
	return n > 0, err
}
