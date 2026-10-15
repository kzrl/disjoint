-- +goose Up
-- Per-user nonce mixed into the default-avatar seed so users can re-roll
-- their pixel avatar ("Shuffle"). 0 = the original roll for the username.
-- Plain ADD COLUMN is safe (no table rebuild, same as avatar_path in 00005).
ALTER TABLE users ADD COLUMN avatar_nonce INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE users DROP COLUMN avatar_nonce;
