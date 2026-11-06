-- +goose Up
-- One row per registered passkey. The credential id is stored base64url so it
-- can be compared and logged as text; the public key and counters are what the
-- WebAuthn library needs to verify a signature.
CREATE TABLE IF NOT EXISTS webauthn_credentials (
    id              TEXT PRIMARY KEY,          -- base64url(raw credential id)
    user_id         INTEGER NOT NULL,
    public_key      BLOB NOT NULL,
    sign_count      INTEGER NOT NULL DEFAULT 0,
    transports      TEXT NOT NULL DEFAULT '',  -- comma-separated hints
    aaguid          TEXT NOT NULL DEFAULT '',  -- hex
    backup_eligible INTEGER NOT NULL DEFAULT 0,
    backup_state    INTEGER NOT NULL DEFAULT 0,
    name            TEXT NOT NULL DEFAULT '',  -- user-facing label
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at    DATETIME,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_webauthn_credentials_user ON webauthn_credentials(user_id);

-- Short-lived ceremony state (challenge, allowed credentials, user handle).
-- Stored server-side so a tampered cookie cannot reshape the challenge; the
-- cookie only carries this row's opaque id, and each row is consumed once.
CREATE TABLE IF NOT EXISTS webauthn_ceremonies (
    id           TEXT PRIMARY KEY,
    user_id      INTEGER,
    kind         TEXT NOT NULL,               -- 'register' | 'login'
    session_data BLOB NOT NULL,               -- JSON of webauthn.SessionData
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at   DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_webauthn_ceremonies_expiry ON webauthn_ceremonies(expires_at);

-- +goose Down
DROP TABLE IF EXISTS webauthn_ceremonies;
DROP TABLE IF EXISTS webauthn_credentials;
