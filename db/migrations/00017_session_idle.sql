-- +goose Up
-- Idle timeout support: last_used_at is touched (throttled) on authenticated
-- requests. Existing sessions get a fresh idle window at migration time rather
-- than being logged out wholesale because their creation date is old.
ALTER TABLE sessions ADD COLUMN last_used_at INTEGER;
UPDATE sessions SET last_used_at = CAST(strftime('%s', 'now') AS INTEGER);

-- +goose Down
ALTER TABLE sessions DROP COLUMN last_used_at;
