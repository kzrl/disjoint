-- +goose Up
-- Admin-controlled account suspension. Disabled users cannot authenticate;
-- their existing sessions are deleted by the disable handler and any surviving
-- one is rejected and purged by the auth middleware.
ALTER TABLE users ADD COLUMN is_disabled INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE users DROP COLUMN is_disabled;
