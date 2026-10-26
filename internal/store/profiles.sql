-- name: ListProfiles :many
SELECT p.id, p.name, p.text, p.default_tv, p.default_anime,
    (SELECT count(*) FROM series s WHERE s.profile_id = p.id) AS shows
FROM profiles p ORDER BY p.name;

-- name: GetProfile :one
SELECT id, name, text, default_tv, default_anime FROM profiles WHERE id = ?;

-- name: GetProfileByName :one
SELECT id, name, text, default_tv, default_anime FROM profiles WHERE name = ?;

-- name: GetDefaultTVProfile :one
SELECT id, name, text, default_tv, default_anime FROM profiles WHERE default_tv = 1;

-- name: GetDefaultAnimeProfile :one
SELECT id, name, text, default_tv, default_anime FROM profiles WHERE default_anime = 1;

-- name: InsertProfile :one
INSERT INTO profiles (name, text) VALUES (?, ?) RETURNING id;

-- name: UpdateProfile :execrows
UPDATE profiles SET name = ?, text = ? WHERE id = ?;

-- name: DeleteProfile :execrows
DELETE FROM profiles WHERE id = ?;

-- name: ClearDefaultTV :exec
UPDATE profiles SET default_tv = 0 WHERE default_tv = 1;

-- name: SetDefaultTV :execrows
UPDATE profiles SET default_tv = 1 WHERE id = ?;

-- name: ClearDefaultAnime :exec
UPDATE profiles SET default_anime = 0 WHERE default_anime = 1;

-- name: SetDefaultAnime :execrows
UPDATE profiles SET default_anime = 1 WHERE id = ?;

-- name: ProfileShows :many
SELECT id, coalesce(name_eng, name) AS name FROM series WHERE profile_id = ? ORDER BY coalesce(name_eng, name);
