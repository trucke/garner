-- +goose Up
-- The monitor's own latest search of each episode. Its schedule counts
-- from these, so a search someone runs to look at releases changes
-- nothing about when the monitor searches or downloads.
CREATE TABLE monitor_searches (
    episode_id INTEGER PRIMARY KEY REFERENCES episodes (id) ON DELETE CASCADE,
    searched_at INTEGER NOT NULL,
    outcome TEXT NOT NULL -- found, fallback, nothing or failed
) STRICT;

-- +goose Down
DROP TABLE monitor_searches;
