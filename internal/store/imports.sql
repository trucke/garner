-- name: MarkImportPending :exec
UPDATE downloads SET import_state = 'pending', import_updated_at = ? WHERE id = ? AND import_state = '';

-- name: UpdateImport :exec
UPDATE downloads SET import_state = ?, import_detail = ?, import_recovery = ?, import_files = ?, import_updated_at = ?
WHERE id = ?;

-- name: ListDueImports :many
SELECT * FROM downloads WHERE import_state IN ('pending', 'moving', 'scanning') ORDER BY id;

-- name: GetEpisodeFile :one
SELECT * FROM episode_files WHERE episode_id = ?;

-- name: UpsertEpisodeFile :exec
INSERT INTO episode_files (episode_id, path, size, download_id, imported_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (episode_id) DO UPDATE SET
    path = excluded.path, size = excluded.size, download_id = excluded.download_id, imported_at = excluded.imported_at;

-- name: CountEpisodeFilesAt :one
SELECT count(*) FROM episode_files WHERE path = ?;
