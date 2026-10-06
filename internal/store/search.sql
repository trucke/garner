-- name: SeriesOfEpisode :one
SELECT series_id FROM episodes WHERE id = ?;

-- name: SetSeriesProfile :execrows
UPDATE series SET profile = ? WHERE id = ?;

-- name: DeleteSearch :exec
DELETE FROM searches WHERE episode_id = ?;

-- name: InsertSearch :one
INSERT INTO searches (episode_id, searched_at, outcome, summary, next_retry_at) VALUES (?, ?, ?, ?, ?)
RETURNING id;

-- name: InsertSearchIndexer :exec
INSERT INTO search_indexers (search_id, indexer, results, error) VALUES (?, ?, ?, ?);

-- name: InsertCandidate :exec
INSERT INTO candidates (search_id, rank, indexer, title, link, size, published, grabs, verdict, tier, audio, reasons, episodes)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetSearch :one
SELECT * FROM searches WHERE episode_id = ?;

-- name: ListSearchIndexers :many
SELECT indexer, results, error FROM search_indexers WHERE search_id = ? ORDER BY indexer;

-- Candidates without their download links.
-- name: ListCandidates :many
SELECT id, rank, indexer, title, size, published, grabs, verdict, tier, audio, reasons, episodes
FROM candidates WHERE search_id = ? ORDER BY rank;
