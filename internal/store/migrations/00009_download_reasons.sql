-- +goose Up
-- Why the release was chosen, copied from its search result, which a new
-- search replaces. decisive counts the leading reasons that decided the
-- verdict, as for candidates.
ALTER TABLE downloads ADD COLUMN reasons TEXT NOT NULL DEFAULT '[]';
ALTER TABLE downloads ADD COLUMN decisive INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE downloads DROP COLUMN decisive;
ALTER TABLE downloads DROP COLUMN reasons;
