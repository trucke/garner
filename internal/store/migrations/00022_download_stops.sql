-- +goose Up
-- Say why Garner tried no other release after a download failed, such as
-- no other upload of a release the user chose. Empty when it tried one,
-- never had to, or for downloads from before.
ALTER TABLE downloads ADD COLUMN stopped TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE downloads DROP COLUMN stopped;
