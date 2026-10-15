-- +goose Up
-- TVDB facts for a show's details page, stored so the page needs no TVDB
-- request. A NULL network means the show was loaded before these were
-- kept; the catalogue sync loads such shows again.
ALTER TABLE series ADD COLUMN network TEXT;
ALTER TABLE series ADD COLUMN last_aired TEXT;
ALTER TABLE series ADD COLUMN overview TEXT;
ALTER TABLE series ADD COLUMN overview_language TEXT;

-- +goose Down
ALTER TABLE series DROP COLUMN overview_language;
ALTER TABLE series DROP COLUMN overview;
ALTER TABLE series DROP COLUMN last_aired;
ALTER TABLE series DROP COLUMN network;
