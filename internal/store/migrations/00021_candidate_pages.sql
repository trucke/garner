-- +goose Up
-- Keep the address of each release's page at the indexer, where people
-- may have commented on it. Empty for candidates from before, and for an
-- address that could hold credentials.
ALTER TABLE candidates ADD COLUMN page TEXT NOT NULL DEFAULT '';
ALTER TABLE season_candidates ADD COLUMN page TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE season_candidates DROP COLUMN page;
ALTER TABLE candidates DROP COLUMN page;
