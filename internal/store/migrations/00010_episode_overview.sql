-- +goose Up
-- TVDB's description of each episode: English when TVDB has it, else
-- German, else the original language; overview_language says which.
ALTER TABLE episodes ADD COLUMN overview TEXT NOT NULL DEFAULT '';
ALTER TABLE episodes ADD COLUMN overview_language TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE episodes DROP COLUMN overview_language;
ALTER TABLE episodes DROP COLUMN overview;
