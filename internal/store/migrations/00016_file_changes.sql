-- +goose Up
-- Changes to files that Garner is about to make: a file it moves within
-- the library, with the path it moves to, or a file it removes, with an
-- empty new_path. Each is written before the change and removed once the
-- file and its record agree again, so Garner finishes a change that a
-- crash interrupted when it starts again.
CREATE TABLE file_changes (
    path TEXT PRIMARY KEY,
    new_path TEXT NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE file_changes;
