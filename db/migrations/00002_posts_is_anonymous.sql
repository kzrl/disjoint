-- +goose Up
ALTER TABLE posts ADD COLUMN is_anonymous INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE posts DROP COLUMN is_anonymous;
