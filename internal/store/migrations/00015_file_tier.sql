-- +goose Up
-- The file's tier, such as "German 720p": the tier of the release it came
-- from, or, for a file Garner took over from the library, what the file
-- check found. Empty when unknown.
ALTER TABLE episode_files ADD COLUMN tier TEXT NOT NULL DEFAULT '';
UPDATE episode_files SET tier = coalesce((SELECT d.tier FROM downloads d WHERE d.id = episode_files.download_id), '');

-- +goose Down
ALTER TABLE episode_files DROP COLUMN tier;
