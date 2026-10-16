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
INSERT INTO candidates (search_id, rank, indexer, title, link, size, published, grabs, verdict, tier, audio, reasons, episodes, decisive)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetSearch :one
SELECT * FROM searches WHERE episode_id = ?;

-- name: ListSearchIndexers :many
SELECT indexer, results, error FROM search_indexers WHERE search_id = ? ORDER BY indexer;

-- Candidates without their download links.
-- name: ListCandidates :many
SELECT id, rank, indexer, title, size, published, grabs, verdict, tier, audio, reasons, episodes, decisive
FROM candidates WHERE search_id = ? ORDER BY rank;

-- name: DeleteSeasonSearch :exec
DELETE FROM season_searches WHERE series_id = ? AND season = ?;

-- name: ClearSeriesSeasonSearches :exec
DELETE FROM season_searches WHERE series_id = ?;

-- name: InsertSeasonSearch :one
INSERT INTO season_searches (series_id, season, searched_at, outcome, summary, indexers) VALUES (?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: InsertSeasonCandidate :exec
INSERT INTO season_candidates (search_id, episode_id, rank, indexer, title, link, size, published, verdict, tier, audio,
    reasons, decisive, episodes)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetSeasonSearch :one
SELECT * FROM season_searches WHERE series_id = ? AND season = ?;

-- Candidates without their download links.
-- name: ListSeasonCandidates :many
SELECT id, rank, indexer, title, size, published, verdict, tier, audio, reasons, decisive, episodes
FROM season_candidates WHERE search_id = ? ORDER BY rank;

-- name: GetSeasonCandidateLink :one
SELECT id, episode_id, title, indexer, link, size, verdict, tier, reasons, decisive
FROM season_candidates WHERE id = ?;
