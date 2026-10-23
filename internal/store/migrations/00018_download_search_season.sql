-- +goose Up
-- Keep the search scope with an attempt: episode and season result sets
-- may coexist, and a replacement must not silently switch between them.
-- Older downloads retain their existing episode-search behavior.
ALTER TABLE downloads ADD COLUMN search_season INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE downloads DROP COLUMN search_season;
