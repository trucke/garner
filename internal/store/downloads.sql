-- name: CountActiveDownloads :one
SELECT count(*) FROM downloads
WHERE episode_id = ? AND state IN ('submitting', 'queued', 'downloading', 'processing');

-- name: InsertDownload :one
INSERT INTO downloads (episode_id, title, indexer, link, size, verdict, tier, reasons, decisive, manual, replaces, search_season, state, detail,
    progress, time_left, storage, failure, retryable, stages, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'submitting', '', 0, '', '', '', 0, '[]', ?, ?)
RETURNING id;

-- name: GetEpisodeFileTier :one
SELECT tier FROM episode_files WHERE episode_id = ?;

-- A cancelled download stays cancelled, whatever a poll that started
-- before the cancel saves.
-- name: UpdateDownload :execrows
UPDATE downloads SET state = ?, detail = ?, job_id = ?, progress = ?, time_left = ?, storage = ?, failure = ?,
    retryable = ?, stages = ?, updated_at = ?
WHERE id = ? AND state != 'cancelled';

-- name: CancelDownload :execrows
UPDATE downloads SET state = 'cancelled', failure = ?, retryable = 0, time_left = '', updated_at = ?
WHERE id = ? AND state IN ('submitting', 'queued', 'downloading', 'processing');

-- Only the latest failed attempt can be cleared. Keeping the first time
-- makes repeated requests harmless, without changing any failure timing.
-- name: AcknowledgeDownload :execrows
UPDATE downloads SET acknowledged_at = coalesce(downloads.acknowledged_at, sqlc.arg(acknowledged_at))
WHERE downloads.id = sqlc.arg(id) AND downloads.state = 'failed' AND downloads.import_state IN ('', 'rejected')
  AND downloads.id IN (SELECT max(latest.id) FROM downloads AS latest GROUP BY latest.episode_id);

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

-- Each episode's latest download, when it runs, waits for the user, or
-- ended since the given time; with its show and its place in the show's
-- default order.
-- name: ListActivity :many
SELECT sqlc.embed(d), e.series_id, s.name AS series_name, s.name_eng AS series_name_eng,
    n.season AS season, n.number AS number
FROM downloads d
JOIN episodes e ON e.id = d.episode_id
JOIN series s ON s.id = e.series_id
LEFT JOIN episode_numbers n ON n.episode_id = e.id AND n.order_type = s.default_order
WHERE d.id IN (SELECT max(id) FROM downloads GROUP BY episode_id)
  AND (d.state IN ('submitting', 'queued', 'downloading', 'processing')
    OR d.import_state IN ('pending', 'moving', 'scanning', 'failed', 'unverified')
    OR d.updated_at >= sqlc.arg(since) OR d.import_updated_at >= sqlc.arg(since))
ORDER BY d.id DESC;
