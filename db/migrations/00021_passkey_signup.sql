-- +goose Up
-- Passkey-only signup. users.password_hash keeps its NOT NULL constraint (no
-- table rebuild while eight tables reference users); password_set is the
-- authoritative flag, and when it is 0 the stored hash is a bcrypt digest of
-- random bytes that nothing knows.
ALTER TABLE users ADD COLUMN password_set INTEGER NOT NULL DEFAULT 1;

-- Signup creates the account row before the authenticator answers (WebAuthn
-- needs a user handle up front). Such a row is invisible, cannot authenticate,
-- and is deleted by the worker if the ceremony is abandoned.
ALTER TABLE users ADD COLUMN pending_until DATETIME; -- NULL = active account

CREATE INDEX IF NOT EXISTS idx_users_pending ON users(pending_until);

-- A signup ceremony remembers which invite it will redeem and for which
-- username, without touching either until the attestation succeeds.
ALTER TABLE webauthn_ceremonies ADD COLUMN context TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE webauthn_ceremonies DROP COLUMN context;
DROP INDEX IF EXISTS idx_users_pending;
ALTER TABLE users DROP COLUMN pending_until;
ALTER TABLE users DROP COLUMN password_set;
