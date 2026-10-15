-- +goose Up
-- How many of a candidate's reasons decided its verdict: the reasons it
-- was rejected for, or why it is only a last resort. The rest are facts.
ALTER TABLE candidates ADD COLUMN decisive INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE candidates DROP COLUMN decisive;
