-- +goose Up
-- Admin-issued password reset links. The token is delivered out-of-band
-- (Signal/chat, copy-paste), stored only as a SHA-256 hash, single-use and
-- short-lived. A reset sets a new password and revokes every session; it never
-- signs the holder in by itself.
CREATE TABLE password_reset_tokens (
    token_hash TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL,
    created_by INTEGER,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME NOT NULL,
    used_at    DATETIME,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX idx_password_reset_tokens_user ON password_reset_tokens(user_id);
CREATE INDEX idx_password_reset_tokens_expires ON password_reset_tokens(expires_at);

-- +goose Down
DROP TABLE password_reset_tokens;
