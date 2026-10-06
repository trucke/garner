-- name: CreateToken :one
INSERT INTO api_tokens (name, hash, last4, created_at) VALUES (?, ?, ?, ?) RETURNING id;

-- name: TokenByHash :one
SELECT id, name, last_used_at FROM api_tokens WHERE hash = ?;

-- name: TokenStillValid :one
SELECT count(*) FROM api_tokens WHERE id = ? AND hash = ?;

-- name: TouchToken :exec
UPDATE api_tokens SET last_used_at = ? WHERE id = ?;

-- name: ListTokens :many
SELECT name, last4, created_at, last_used_at FROM api_tokens ORDER BY created_at, id;

-- name: DeleteToken :one
DELETE FROM api_tokens WHERE name = ? RETURNING id;

-- name: CountTokens :one
SELECT count(*) FROM api_tokens;

-- name: RecordBackup :exec
INSERT INTO backups (path, size, reason, created_at) VALUES (?, ?, ?, ?);

-- name: LastBackup :one
SELECT path, size, reason, created_at FROM backups ORDER BY created_at DESC, id DESC LIMIT 1;
