-- name: MarkImportPending :exec
UPDATE downloads SET import_state = 'pending', import_updated_at = ? WHERE id = ? AND import_state = '';

-- name: UpdateImport :exec
UPDATE downloads SET import_state = ?, import_detail = ?, import_recovery = ?, import_files = ?, import_notes = ?,
    import_updated_at = ?
WHERE id = ?;

-- name: ListDueImports :many
SELECT * FROM downloads WHERE import_state IN ('pending', 'moving', 'scanning') ORDER BY id;

-- name: GetEpisodeFile :one
SELECT * FROM episode_files WHERE episode_id = ?;

-- name: UpsertEpisodeFile :exec
INSERT INTO episode_files (episode_id, path, size, tier, download_id, imported_at) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (episode_id) DO UPDATE SET
    path = excluded.path, size = excluded.size, tier = excluded.tier, download_id = excluded.download_id,
    imported_at = excluded.imported_at;

-- A file Garner took over from the library, unless the episode has one.
-- name: InsertTakenFile :execrows
INSERT INTO episode_files (episode_id, path, size, tier, download_id, imported_at) VALUES (?, ?, ?, ?, NULL, ?)
ON CONFLICT (episode_id) DO NOTHING;

-- A file Garner moved within the library, for every episode it holds.
-- name: MoveEpisodeFile :exec
UPDATE episode_files SET path = sqlc.arg(new_path) WHERE path = sqlc.arg(old_path);

-- The files of a show Garner took over from the library, with the
-- episodes each holds.
-- name: ListSeriesTakenFiles :many
SELECT f.episode_id, f.path, f.size, f.tier FROM episode_files f JOIN episodes e ON e.id = f.episode_id
WHERE e.series_id = ? AND f.download_id IS NULL
ORDER BY f.path, f.episode_id;

-- name: CountEpisodeFilesAt :one
SELECT count(*) FROM episode_files WHERE path = ?;
