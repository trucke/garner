-- +goose Up
-- Notes on an import worth showing, such as subtitle files moved or files
-- left in SABnzbd's folder. JSON array of strings.
ALTER TABLE downloads ADD COLUMN import_notes TEXT NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE downloads DROP COLUMN import_notes;
