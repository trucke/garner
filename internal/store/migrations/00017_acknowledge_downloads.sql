-- +goose Up
-- Clearing a warning is separate from the failure and its timing: both
-- still drive failed-release exclusions and the monitor's next search.
ALTER TABLE downloads ADD COLUMN acknowledged_at INTEGER;

-- +goose Down
ALTER TABLE downloads DROP COLUMN acknowledged_at;
