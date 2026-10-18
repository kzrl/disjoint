-- +goose Up
-- Edit tracking: NULL until a post is edited for the first time. The edit
-- handler sets CURRENT_TIMESTAMP on every successful edit; the "(edited)"
-- marker in the post card derives from validity. created_at is never
-- touched.
ALTER TABLE posts ADD COLUMN updated_at DATETIME;

-- +goose Down
ALTER TABLE posts DROP COLUMN updated_at;
