-- +goose Up
-- Which of a show's episodes the monitor gets: new (those that air from
-- monitor_since on), all (every aired one that is missing) or off. Shows
-- added before get new ones from the day they were added.
ALTER TABLE series ADD COLUMN monitor TEXT NOT NULL DEFAULT 'new';
ALTER TABLE series ADD COLUMN monitor_since INTEGER NOT NULL DEFAULT 0;
UPDATE series SET monitor_since = added_at;

-- +goose Down
ALTER TABLE series DROP COLUMN monitor_since;
ALTER TABLE series DROP COLUMN monitor;
