-- Every episode with its position in its show's default order; season and
-- number are NULL when the episode has none there.
-- name: ListLibraryEpisodes :many
SELECT e.id, e.series_id, e.aired, n.season, n.number
FROM episodes e
JOIN series s ON s.id = e.series_id
LEFT JOIN episode_numbers n ON n.episode_id = e.id AND n.order_type = s.default_order;

-- name: ListEpisodeFiles :many
SELECT episode_id, path, size FROM episode_files;

-- The latest download of every episode.
-- name: ListLatestDownloads :many
SELECT * FROM downloads WHERE id IN (SELECT max(id) FROM downloads GROUP BY episode_id);

-- name: ListSearchOutcomes :many
SELECT episode_id, searched_at, outcome, summary, next_retry_at FROM searches;
