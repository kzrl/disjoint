-- +goose Up
-- Admin-issued one-time login links. The token is delivered out-of-band
-- (Signal/chat), stored only as a SHA-256 hash, and consumed atomically.
CREATE TABLE login_tokens (
    token_hash TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL,
    created_by INTEGER,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at INTEGER NOT NULL,
    used_at    DATETIME,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX idx_login_tokens_user ON login_tokens(user_id);
CREATE INDEX idx_login_tokens_expires ON login_tokens(expires_at);

-- +goose Down
DROP TABLE login_tokens;
