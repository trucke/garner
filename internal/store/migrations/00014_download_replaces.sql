-- +goose Up
-- The tier of the episode's file when the download started, such as
-- "German 720p": the file the download replaces. Empty when the episode had
-- no file or its tier is unknown.
ALTER TABLE downloads ADD COLUMN replaces TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE downloads DROP COLUMN replaces;
