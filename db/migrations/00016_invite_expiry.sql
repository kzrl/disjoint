-- +goose Up
-- New invites carry a seven-day expiry (set by CreateInvite); existing rows
-- stay NULL = no expiry so already-shared links are not invalidated.
ALTER TABLE invites ADD COLUMN expires_at DATETIME;

-- +goose Down
ALTER TABLE invites DROP COLUMN expires_at;
