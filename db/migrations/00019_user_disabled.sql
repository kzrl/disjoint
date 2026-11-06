-- +goose Up
-- Admin-controlled account suspension. Disabled users cannot authenticate;
-- their existing sessions are deleted by the disable handler and any surviving
-- one is rejected and purged by the auth middleware.
--
-- Numbered 00019 because goose version 18 is already recorded in deployed
-- databases by a since-reverted admin-login-links migration (see commit
-- 6c2cb42); reusing 18 would make goose skip this migration silently.
ALTER TABLE users ADD COLUMN is_disabled INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE users DROP COLUMN is_disabled;
