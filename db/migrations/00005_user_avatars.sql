-- +goose Up
-- User avatars: nullable path into the shared uploads directory. Plain ADD
-- COLUMN is safe (no table rebuild, same as the bio column in 00004).
ALTER TABLE users ADD COLUMN avatar_path TEXT;

-- +goose Down
ALTER TABLE users DROP COLUMN avatar_path;
