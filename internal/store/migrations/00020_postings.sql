-- +goose Up
-- Tell NZB postings apart by the indexer's ID for each, as a fingerprint:
-- two postings of the same release share the name, and a failed one must
-- not rule out the other (GRR-43). Rows from before have none; they are
-- told apart by name and size.
ALTER TABLE candidates ADD COLUMN posting TEXT NOT NULL DEFAULT '';
ALTER TABLE season_candidates ADD COLUMN posting TEXT NOT NULL DEFAULT '';
ALTER TABLE downloads ADD COLUMN posting TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE downloads DROP COLUMN posting;
ALTER TABLE season_candidates DROP COLUMN posting;
ALTER TABLE candidates DROP COLUMN posting;
