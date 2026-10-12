-- +goose Up
-- +goose NO TRANSACTION

-- Server-side sessions replace the stateless HMAC cookie token. The cookie
-- holds a random token; the table stores its SHA-256 hash (so a leaked
-- database does not leak live sessions) mapped to a user with an expiry.
-- expires_at is unix seconds for simple comparison and pruning.
CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

-- +goose Down
-- +goose NO TRANSACTION

DROP TABLE IF EXISTS sessions;
