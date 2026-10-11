-- +goose Up
-- Community trio (user cards, on-the-air, activity ticker) foundation:
--   1) users.bio — free-form profile text for user cards. Nullable, so a
--      plain ADD COLUMN is safe (no table rebuild, unlike 00003).
--   2) user_presence — when each user was last seen, powering the
--      "On the air" list and the "last seen" line on user cards. Sessions
--      are stateless HMAC tokens, so this table is the only presence
--      record; it is maintained by a throttled upsert in the auth middleware.
ALTER TABLE users ADD COLUMN bio TEXT;

CREATE TABLE user_presence (
    user_id INTEGER PRIMARY KEY,
    last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE user_presence;
ALTER TABLE users DROP COLUMN bio;
