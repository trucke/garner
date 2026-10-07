-- +goose Up
-- Import of a completed download into the library: pending, moving,
-- scanning, imported, unverified, rejected or failed.
ALTER TABLE downloads ADD COLUMN import_state TEXT NOT NULL DEFAULT '';
ALTER TABLE downloads ADD COLUMN import_detail TEXT NOT NULL DEFAULT '';
ALTER TABLE downloads ADD COLUMN import_recovery TEXT NOT NULL DEFAULT '';
ALTER TABLE downloads ADD COLUMN import_files TEXT NOT NULL DEFAULT '[]'; -- JSON: plan and file checks
ALTER TABLE downloads ADD COLUMN import_updated_at INTEGER NOT NULL DEFAULT 0;

-- The file Garner imported for each episode; one version per episode.
CREATE TABLE episode_files (
    episode_id INTEGER PRIMARY KEY REFERENCES episodes (id) ON DELETE CASCADE,
    path TEXT NOT NULL, -- as Garner reaches it
    size INTEGER NOT NULL,
    download_id INTEGER REFERENCES downloads (id) ON DELETE SET NULL,
    imported_at INTEGER NOT NULL
) STRICT;

-- +goose Down
DROP TABLE episode_files;
ALTER TABLE downloads DROP COLUMN import_updated_at;
ALTER TABLE downloads DROP COLUMN import_files;
ALTER TABLE downloads DROP COLUMN import_recovery;
ALTER TABLE downloads DROP COLUMN import_detail;
ALTER TABLE downloads DROP COLUMN import_state;
