package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/trucke/garner/internal/store/query"
)

// MarkImportPending queues a completed download for import, once.
func (s *Store) MarkImportPending(ctx context.Context, downloadID int64, now time.Time) error {
	return s.q.MarkImportPending(ctx, query.MarkImportPendingParams{ImportUpdatedAt: now.Unix(), ID: downloadID})
}

// UpdateImport saves an import's progress.
func (s *Store) UpdateImport(ctx context.Context, downloadID int64, im Import, now time.Time) error {
	if im.Files == nil {
		im.Files = []ImportFile{}
	}
	if im.Notes == nil {
		im.Notes = []string{}
	}
	files, err := json.Marshal(im.Files)
	if err != nil {
		return err
	}
	notes, err := json.Marshal(im.Notes)
	if err != nil {
		return err
	}
	return s.q.UpdateImport(ctx, query.UpdateImportParams{
		ImportState: im.State, ImportDetail: im.Detail, ImportRecovery: im.Recovery, ImportFiles: string(files),
		ImportNotes: string(notes), ImportUpdatedAt: now.Unix(), ID: downloadID,
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
	// Tier is the file's tier, such as "German 720p"; empty when unknown.
	Tier string
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
	return EpisodeFile{EpisodeID: r.EpisodeID, Path: r.Path, Size: r.Size, Tier: r.Tier}, true, nil
}

// SetEpisodeFile records the file of an episode, replacing the previous
// one: one Garner imported from the download, or, with downloadID 0, one
// it took over from the library.
func (s *Store) SetEpisodeFile(ctx context.Context, f EpisodeFile, downloadID int64, now time.Time) error {
	return s.q.UpsertEpisodeFile(ctx, query.UpsertEpisodeFileParams{
		EpisodeID: f.EpisodeID, Path: f.Path, Size: f.Size, Tier: f.Tier,
		DownloadID: sql.NullInt64{Int64: downloadID, Valid: downloadID > 0}, ImportedAt: now.Unix(),
	})
}

// TakeOverFile records a file Garner found in the library for an episode,
// unless the episode has a file meanwhile, such as from an import that
// finished. It reports whether it recorded the file.
func (s *Store) TakeOverFile(ctx context.Context, f EpisodeFile, now time.Time) (bool, error) {
	n, err := s.q.InsertTakenFile(ctx, query.InsertTakenFileParams{
		EpisodeID: f.EpisodeID, Path: f.Path, Size: f.Size, Tier: f.Tier, ImportedAt: now.Unix(),
	})
	return n > 0, err
}

// RecordFiles records the files of an import's episodes, replacing their
// previous records, and plans to remove the replaced files that no record
// uses any more, in one step: a crash after it leaves the plan, so the
// removal still happens. It returns the files planned for removal; the
// caller removes each and then drops its plan.
func (s *Store) RecordFiles(ctx context.Context, files []EpisodeFile, downloadID int64, now time.Time, replaced []string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	for _, f := range files {
		if err := q.UpsertEpisodeFile(ctx, query.UpsertEpisodeFileParams{
			EpisodeID: f.EpisodeID, Path: f.Path, Size: f.Size, Tier: f.Tier,
			DownloadID: sql.NullInt64{Int64: downloadID, Valid: downloadID > 0}, ImportedAt: now.Unix(),
		}); err != nil {
			return nil, err
		}
	}
	var planned []string
	for _, p := range replaced {
		n, err := q.CountEpisodeFilesAt(ctx, p)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			continue // another episode's file still
		}
		if err := q.InsertFileChange(ctx, query.InsertFileChangeParams{Path: p}); err != nil {
			return nil, err
		}
		planned = append(planned, p)
	}
	return planned, tx.Commit()
}

// FileChange is a change to a file that Garner planned: a move to NewPath,
// or a removal when NewPath is empty.
type FileChange = query.FileChange

// PlanMove notes that Garner is about to move a recorded file within the
// library; FinishMove ends the plan.
func (s *Store) PlanMove(ctx context.Context, oldPath, newPath string) error {
	return s.q.InsertFileChange(ctx, query.InsertFileChangeParams{Path: oldPath, NewPath: newPath})
}

// FinishMove records that a file moved, for every episode it holds, and
// drops the plan to move it, in one step.
func (s *Store) FinishMove(ctx context.Context, oldPath, newPath string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	if err := q.MoveEpisodeFile(ctx, query.MoveEpisodeFileParams{OldPath: oldPath, NewPath: newPath}); err != nil {
		return err
	}
	if err := q.DeleteFileChange(ctx, oldPath); err != nil {
		return err
	}
	return tx.Commit()
}

// DropFileChange drops the plan to change the file at path, once it is
// carried out or did not happen.
func (s *Store) DropFileChange(ctx context.Context, path string) error {
	return s.q.DeleteFileChange(ctx, path)
}

// FileChanges lists the planned changes to files, by path.
func (s *Store) FileChanges(ctx context.Context) ([]FileChange, error) {
	return s.q.ListFileChanges(ctx)
}

// PlannedRemoval reports whether Garner planned to remove the file at
// path.
func (s *Store) PlannedRemoval(ctx context.Context, path string) (bool, error) {
	n, err := s.q.CountPlannedRemovals(ctx, path)
	return n > 0, err
}

// TakenFiles lists the files of a show Garner took over from the library,
// one entry per episode, ordered by path.
func (s *Store) TakenFiles(ctx context.Context, seriesID int64) ([]EpisodeFile, error) {
	rows, err := s.q.ListSeriesTakenFiles(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	out := make([]EpisodeFile, len(rows))
	for i, r := range rows {
		out[i] = EpisodeFile{EpisodeID: r.EpisodeID, Path: r.Path, Size: r.Size, Tier: r.Tier}
	}
	return out, nil
}

// FileInUse reports whether any episode's imported file is at path.
func (s *Store) FileInUse(ctx context.Context, path string) (bool, error) {
	n, err := s.q.CountEpisodeFilesAt(ctx, path)
	return n > 0, err
}
