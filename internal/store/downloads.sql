-- name: CountActiveDownloads :one
SELECT count(*) FROM downloads
WHERE episode_id = ? AND state IN ('submitting', 'queued', 'downloading', 'processing');

-- name: InsertDownload :one
INSERT INTO downloads (episode_id, title, indexer, link, size, verdict, tier, reasons, decisive, manual, state, detail,
    progress, time_left, storage, failure, retryable, stages, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'submitting', '', 0, '', '', '', 0, '[]', ?, ?)
RETURNING id;

-- A cancelled download stays cancelled, whatever a poll that started
-- before the cancel saves.
-- name: UpdateDownload :execrows
UPDATE downloads SET state = ?, detail = ?, job_id = ?, progress = ?, time_left = ?, storage = ?, failure = ?,
    retryable = ?, stages = ?, updated_at = ?
WHERE id = ? AND state != 'cancelled';

-- name: CancelDownload :execrows
UPDATE downloads SET state = 'cancelled', failure = ?, retryable = 0, time_left = '', updated_at = ?
WHERE id = ? AND state IN ('submitting', 'queued', 'downloading', 'processing');

-- name: ListEpisodeDownloads :many
SELECT * FROM downloads WHERE episode_id = ? ORDER BY id DESC;

-- name: GetDownload :one
SELECT * FROM downloads WHERE id = ?;

-- name: ListActiveDownloads :many
SELECT * FROM downloads WHERE state IN ('submitting', 'queued', 'downloading', 'processing') ORDER BY id;

-- Running downloads first, then the most recent.
-- name: ListDownloads :many
SELECT * FROM downloads
ORDER BY state IN ('submitting', 'queued', 'downloading', 'processing') DESC, id DESC
LIMIT ?;

-- Releases Garner does not pick again on its own: failed or cancelled.
-- name: ListFailedTitles :many
SELECT title FROM downloads WHERE episode_id = ? AND state IN ('failed', 'cancelled');

-- name: CountFailedDownloadsSince :one
SELECT count(*) FROM downloads WHERE episode_id = ? AND state = 'failed' AND created_at >= ?;

-- Candidates of the latest search with their download links, best first.
-- name: ListCandidateLinks :many
SELECT c.id, s.episode_id, c.title, c.indexer, c.link, c.size, c.verdict, c.tier, c.reasons, c.decisive
FROM candidates c JOIN searches s ON s.id = c.search_id
WHERE s.episode_id = ? ORDER BY c.rank;

-- name: GetCandidateLink :one
SELECT c.id, s.episode_id, c.title, c.indexer, c.link, c.size, c.verdict, c.tier, c.reasons, c.decisive
FROM candidates c JOIN searches s ON s.id = c.search_id
WHERE c.id = ?;
