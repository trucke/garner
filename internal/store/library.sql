-- Every episode with its position in its show's default order; season and
-- number are NULL when the episode has none there.
-- name: ListLibraryEpisodes :many
SELECT e.id, e.series_id, e.aired, n.season, n.number
FROM episodes e
JOIN series s ON s.id = e.series_id
LEFT JOIN episode_numbers n ON n.episode_id = e.id AND n.order_type = s.default_order;

-- Every imported file with the tier of the release it came from.
-- name: ListEpisodeFiles :many
SELECT f.episode_id, f.path, f.size, coalesce(d.tier, '') AS tier
FROM episode_files f LEFT JOIN downloads d ON d.id = f.download_id;

-- name: CountDownloadsByEpisode :many
SELECT episode_id, count(*) AS attempts FROM downloads GROUP BY episode_id;

-- The latest download of every episode.
-- name: ListLatestDownloads :many
SELECT * FROM downloads WHERE id IN (SELECT max(id) FROM downloads GROUP BY episode_id);

-- name: ListSearchOutcomes :many
SELECT episode_id, searched_at, outcome, summary, next_retry_at FROM searches;

-- Episodes with a file, and the tier of the release it came from.
-- name: ListSeriesFileTiers :many
SELECT f.episode_id, coalesce(d.tier, '') AS tier
FROM episode_files f JOIN episodes e ON e.id = f.episode_id LEFT JOIN downloads d ON d.id = f.download_id
WHERE e.series_id = ?;

-- name: ListSeriesFiles :many
SELECT f.path, f.size FROM episode_files f JOIN episodes e ON e.id = f.episode_id WHERE e.series_id = ?;

-- Downloads of a show that are running or whose files are being moved.
-- name: ListSeriesBusyDownloads :many
SELECT d.* FROM downloads d JOIN episodes e ON e.id = d.episode_id
WHERE e.series_id = ?
    AND (d.state IN ('submitting', 'queued', 'downloading', 'processing') OR d.import_state IN ('pending', 'moving'));

-- A changed mode counts from now: new episodes are those airing from now on.
-- name: SetSeriesMonitor :execrows
UPDATE series SET
    monitor_since = CASE WHEN monitor = sqlc.arg(monitor) THEN monitor_since ELSE sqlc.arg(since) END,
    monitor = sqlc.arg(monitor)
WHERE id = sqlc.arg(id);

-- name: SaveMonitorSearch :exec
INSERT INTO monitor_searches (episode_id, searched_at, outcome) VALUES (?, ?, ?)
ON CONFLICT (episode_id) DO UPDATE SET searched_at = excluded.searched_at, outcome = excluded.outcome;

-- name: ListMonitorSearches :many
SELECT episode_id, searched_at, outcome FROM monitor_searches;

-- name: ClearSeriesMonitorSearches :exec
DELETE FROM monitor_searches WHERE episode_id IN (SELECT id FROM episodes WHERE series_id = ?);

-- name: DeleteSeries :execrows
DELETE FROM series WHERE id = ?;

-- Search results of a show's episodes, except those a running download
-- may still need to pick a replacement.
-- name: ClearSeriesSearches :exec
DELETE FROM searches
WHERE episode_id IN (SELECT id FROM episodes WHERE series_id = ?)
    AND episode_id NOT IN (SELECT episode_id FROM downloads WHERE state IN ('submitting', 'queued', 'downloading', 'processing'));

-- name: CountSeriesBusyDownloads :one
SELECT count(*) FROM downloads d JOIN episodes e ON e.id = d.episode_id
WHERE e.series_id = ?
    AND (d.state IN ('submitting', 'queued', 'downloading', 'processing') OR d.import_state IN ('pending', 'moving'));
