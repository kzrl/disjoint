# Implementation Plan - Passkeys (WebAuthn)

Add passkey sign-in: users register one or more passkeys from `/account` and
can then sign in with platform authenticators (Touch ID / Windows Hello /
Android / security keys) instead of typing a password.

Design stance for v1:

- **Additive, not substitutive.** Passwords stay; passkeys are an extra way in.
  That means no account can be locked out by losing a device, which is the
  usual failure mode of passwordless-first designs, and the botched recovery
  story (no email, admin login-link reverted) stays out of scope.
- **WebAuthn Level 2**, library `github.com/go-webauthn/webauthn` v0.18.x
  (verified on the module proxy: `go 1.26`, so it fits this repo's `go 1.27.3`).
- **Discoverable credentials** (resident keys) so users can sign in without
  typing a username, plus conditional UI (`autocomplete="webauthn"`) as a
  stretch goal.
- HTTPS is mandatory for WebAuthn. Production is behind Caddy with HSTS;
  local dev is allowed on `http://localhost` per the spec.

## 1. Configuration

RP ID and origins must match the browser's origin exactly, so they cannot be
guessed from `r.Host` (a reverse proxy or a test could differ). Add env vars,
falling back to `PUBLIC_BASE_URL` when present:

| Env | Meaning | Prod value |
|---|---|---|
| `WEBAUTHN_RP_ID` | registrable domain suffix | `disjoint.kzrl.org` |
| `WEBAUTHN_RP_ORIGINS` | comma-separated allowed origins | `https://disjoint.kzrl.org` |

Rules (`internal/passkeys/config.go`):

- If `WEBAUTHN_RP_ID` is unset, derive from `PUBLIC_BASE_URL` hostname; if that
  is also unset, use `localhost` (dev) and log a warning.
- If origins are unset, derive `https://<rp_id>` (or `http://localhost:PORT`
  for the dev fallback).
- `RPDisplayName = "Disjoint"`, `AttestationPreference = none`,
  `AuthenticatorSelection = { ResidentKey: preferred, UserVerification:
  preferred }`, `Timeout = 60s`.
- Wire the values in `cmd/disjoint/main.go` (env → config → `handlers.NewApp`
  or an exported setter), not inside handlers, so tests can inject a config
  for `localhost`.
- Add the two env vars to `deploy-disjoint.yml` Phase 5's systemd `Environment=`
  lines and to the README env table.

## 2. Database (migration `00020`)

Next free goose version is **20** (18 is burned by the reverted
admin-login-links migration — see commit `117b6b7`; 19 is `is_disabled`).

`db/migrations/00020_passkeys.sql`:

```sql
-- +goose Up
-- One row per registered passkey. The credential id is stored base64url so it
-- can appear in URLs and sqlc-generated structs without []byte juggling.
CREATE TABLE IF NOT EXISTS webauthn_credentials (
    id              TEXT PRIMARY KEY,         -- base64url(raw credential id)
    user_id         INTEGER NOT NULL,
    public_key      BLOB NOT NULL,
    sign_count      INTEGER NOT NULL DEFAULT 0,
    transports      TEXT NOT NULL DEFAULT '', -- comma-separated hints
    aaguid          TEXT NOT NULL DEFAULT '',
    backup_eligible INTEGER NOT NULL DEFAULT 0,
    backup_state    INTEGER NOT NULL DEFAULT 0,
    name            TEXT NOT NULL DEFAULT '', -- user-facing label
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at    DATETIME,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_webauthn_credentials_user ON webauthn_credentials(user_id);

-- Short-lived ceremony state (challenge, allowed credentials, user handle).
-- Stored server-side rather than in a client cookie so a tampered cookie can
-- never reshape the challenge; consumed exactly once.
CREATE TABLE IF NOT EXISTS webauthn_ceremonies (
    id           TEXT PRIMARY KEY,            -- random opaque token, also set as a cookie
    user_id      INTEGER,                     -- NULL for discoverable login
    kind         TEXT NOT NULL,               -- 'register' | 'login'
    session_data BLOB NOT NULL,               -- JSON of webauthn.SessionData
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at   DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_webauthn_ceremonies_expiry ON webauthn_ceremonies(expires_at);

-- +goose Down
DROP TABLE IF EXISTS webauthn_ceremonies;
DROP TABLE IF EXISTS webauthn_credentials;
```

Mirror both tables in `db/schema.sql` (sqlc snapshot), then `make generate`.

### Queries (`db/query.sql`)

```sql
-- name: CreateWebAuthnCredential :exec
-- name: ListWebAuthnCredentialsByUser :many
-- name: GetWebAuthnCredential :one          -- by credential id
-- name: UpdateWebAuthnCredentialUsage :exec -- sign_count, last_used_at
-- name: UpdateWebAuthnCredentialName :exec
-- name: DeleteWebAuthnCredential :execrows  -- WHERE id = ? AND user_id = ?
-- name: CountWebAuthnCredentialsByUser :one
-- name: CreateWebAuthnCeremony :exec
-- name: TakeWebAuthnCeremony :one           -- DELETE ... RETURNING, single use
-- name: DeleteExpiredWebAuthnCeremonies :exec
```

`TakeWebAuthnCeremony` must be a `DELETE … RETURNING` (modernc SQLite supports
it) so a ceremony can never be replayed, even by two concurrent requests.

Prune expired ceremonies opportunistically at the start of every begin call
(cheap `DELETE WHERE expires_at < now`), rather than growing the worker for a
table with a 5-minute horizon.

## 3. Package layout (`internal/passkeys`)

Keep protocol logic out of handlers, like `internal/linkmeta`/`internal/thumbs`:

- `config.go` — env parsing + validation (RP ID is a suffix of every origin;
  origins are absolute `http(s)` URLs).
- `store.go` — ceremony + credential persistence; converts between
  `database.*` rows and `webauthn.Credential`/`webauthn.User`.
- `service.go` — thin wrapper around `*webauthn.WebAuthn`:
  - `BeginRegistration(ctx, user, passwordConfirmed bool) (options, ceremonyToken)`.
  - `FinishRegistration(ctx, user, ceremonyToken, body)`.
  - `BeginLogin(ctx) (options, ceremonyToken)` and optional username-first
    `BeginLoginForUser(ctx, username)`.
  - `FinishLogin(ctx, ceremonyToken, body) (userID, credentialID)`.
  - `UserHandle(user)` = 8-byte big-endian user id (stable, reveals nothing).
  - `ExcludeCredentials(user)` for registration.
- Interfaces so handlers/tests can stub the service, with one real
  implementation: `type Service interface { ... }` and `passkeys.New(...)`.

Errors are typed (`ErrCeremonyExpired`, `ErrCredentialNotFound`,
`ErrSignCountRegression`, `ErrOrigin`, `ErrVerification`) and mapped to generic
user-facing messages.

## 4. Handlers & routes

New file `internal/handlers/passkeys.go`.

Registration (authenticated):

| Route | Notes |
|---|---|
| `POST /account/passkeys/begin` | JSON in: `{"password": "..."}`. Re-verifies the current password (session hijack protection) using `app.passwordLimiter`, then returns creation options + sets the ceremony cookie. |
| `POST /account/passkeys/finish` | JSON credential; verifies ceremony+origin, stores the credential with a default name (`Passkey 1`, `Passkey 2`, …), audits `add_passkey`, returns `{"ok":true}`. |
| `POST /account/passkeys/{id}/delete` | Regular form POST (existing CSRF + redirect flow), scoped by `user_id`, audits `delete_passkey`. |
| `POST /account/passkeys/{id}/name` | Optional: rename; form POST, `maxlength=40`. |

Login (anonymous):

| Route | Notes |
|---|---|
| `POST /login/passkey/begin` | No body. Returns `publicKey` options for discoverable credentials. Wrapped in `app.authLimiter` (same bucket as password login). |
| `POST /login/passkey/finish` | Verifies assertion, rejects `IsDisabled == 1`, updates sign count/last-used, calls the existing `app.startSession(ctx, w, r, user.ID)`, audits `passkey_login`, redirects (`{"redirect":"/"}`). |

Details that matter:

- **Ceremony cookie**: `HttpOnly`, `SameSite=Lax`, `Secure` per
  `auth.SecureCookie`, `MaxAge = 300`, name `disjoint_webauthn`. Server-side
  state stays in `webauthn_ceremonies`, so the cookie is only a pointer.
- **Single-use**: `TakeWebAuthnCeremony` deletes on read; a wrong/expired token
  is a generic 400.
- **Disabled accounts**: `BeginLoginForUser`/`FinishLogin` check
  `IsDisabled == 0` (same rule the password path enforces after a correct
  password). Return the generic failure message, not "account disabled", for
  the anonymous endpoint.
- **Clone detection**: if `credential.Authenticator.SignCount` regresses,
  log + audit `passkey_sign_count_regression`, reject the login, and leave the
  credential in place (admin/user can delete it) — do not silently trust it.
- **Limits**: max 10 credentials per user (`CountWebAuthnCredentialsByUser`),
  rejecting new registrations beyond that; JSON bodies capped at 64 KiB;
  begin/finish rate-limited.
- **Audit**: `add_passkey`, `delete_passkey`, `passkey_login` (existing
  `audit()` log format; actor id, credential/target id, no secrets).
- **No user enumeration**: login begin never echoes which credentials exist;
  finish errors are all "Passkey sign-in failed".
- **Sessions**: reuse `startSession` unchanged; no new cookie, no change to
  `SessionDuration`/`IdleTimeout`.
- **Sign-out everywhere / password change** already revoke sessions; passkeys
  are unaffected (they are credentials, not sessions) — document this in the
  account page copy.

Routes are registered next to the existing account/auth routes in
`handlers/app.go`; `/login/passkey/*` stays public in
`middleware.FirstRunAndAuth` (it is a POST under `/login/passkey`, so the
existing `isPublicRoute` exact-match needs a `strings.HasPrefix` for that
prefix).

## 5. Templates & JavaScript

- `layout.html`: add `{{block "scripts" .}}{{end}}` after the shared scripts so
  pages opt into extra JS instead of loading it everywhere.
- `web/templates/account.html`: new "Passkeys" box above the password box —
  list of credentials (name, created, last used, Delete button), an "Add
  passkey" button with a password confirmation field, and copy explaining that
  passwords still work.
- `web/templates/login.html`: "Sign in with a passkey" button under the form,
  hidden until `window.PublicKeyCredential` exists (progressive enhancement;
  no-JS users keep the password form). Stretch: conditional UI by adding
  `autocomplete="webauthn"` to the username input and calling `begin` on load.
- `web/static/passkeys.js` (new, no inline JS so the CSP stays tight).
  **Safari/iOS requires `navigator.credentials.*` in the same task as the user
  gesture**, so a `fetch()` for the options first loses activation and
  `create()`/`get()` fail. The flow is therefore two-phase: the first tap
  fetches and stores the options (on `/account` after the password check; on
  `/login` as a silent prefetch), the second tap calls WebAuthn synchronously
  and posts the result. Chrome/Firefox use the same two taps, so there is one
  code path, and every failure writes a message next to the button.
  - `b64urlToBuf` / `bufToB64url` helpers (challenges and ids must be decoded
    before `navigator.credentials.*`).
  - Registration: fetch begin → `navigator.credentials.create` →
    fetch finish; surface library error codes as short messages.
  - Login: fetch begin → `navigator.credentials.get` → finish → `location =
    redirect`.
  - Feature-detect `PublicKeyCredential` and `isUserVerifyingPlatformAuthenticatorAvailable`
    to decide whether to show the button.
- `web/css/site.css`: minimal additions (passkey list rows, icon) and
  `make css`.

## 6. Tests

1. **`internal/passkeys` unit tests**
   - config parsing/validation: rp id/origin suffix checks, dev fallback.
   - ceremony store: single use, expiry, unknown token, wrong kind.
   - user handle round-trip (id → bytes → id), and that handles are stable.
   - sign-count regression detection.

2. **Handler tests with a stubbed `Service`** (`internal/handlers/passkeys_test.go`)
   - `/account/passkeys/*` requires a session; wrong password refuses begin;
     rate limit trips; credential count cap; delete is scoped to the owner
     (another user's id is a 404 and deletes nothing); audit lines.
   - `/login/passkey/finish` success starts a session (real
     `authCookieFor`-style assertion via `Set-Cookie`), disabled user is
     refused, expired/absent ceremony cookie is a generic 400.
   - CSRF: cross-origin JSON POST is rejected (existing middleware test covers
     the generic path; assert once for the new route).

3. **One real-crypto integration test**
   - Add `github.com/descope/virtualwebauthn` (v1.0.5, verified on the proxy,
     Go 1.25) as a **test-only** dependency, or hand-roll a tiny ES256
     authenticator in `_test.go` if a new dependency is unwanted.
   - Register through `/account/passkeys/*`, then log in through
     `/login/passkey/*` against a `localhost` RP config; assert session cookie
     and stored sign count.
   - Negative: assertion from a different RP ID/origin is rejected.

4. **Manual/browser verification** (documented in the plan's task list)
   - Chrome DevTools → WebAuthn → "Enable virtual authenticator" on
     `https://disjoint.kzrl.org` (or localhost), register, sign out, sign in
     with the passkey; Safari/iOS sanity check if available.

## 7. Rollout

1. `make generate`; `gofmt`, `go vet ./...`, `go test -race ./...` (CI already
   runs these plus `govulncheck`; the new dependency will be scanned).
2. Deploy with the playbook after adding the two env vars:
   ```
   spot -p deploy-disjoint.yml -t node1.prd.springupsoftware.com \
     -e DOMAIN:disjoint.kzrl.org -e APP_PORT:8082
   ```
   Goose applies `00020_passkeys.sql` at startup before the new binary serves.
3. Smoke: `/account` shows the Passkeys box; register on a phone; sign out; sign
   in with the passkey; verify the session appears and the account page lists
   `last used`.
4. Rollback: revert the commit and redeploy. The tables are additive and the
   old binary ignores them; existing password logins are untouched.

## 8. Risks & open questions

- **Origin/RP misconfiguration** is the most likely breakage (works on desktop,
  fails on a proxy or with `www.`). Mitigate with explicit env vars, startup
  validation that logs loudly, and a config unit test.
- **Library surface**: `go-webauthn` pulls CBOR, JWT and TPM parsing into the
  dependency graph (binary size + supply chain). Acceptable for the feature;
  `govulncheck` is already in CI.
- **Recovery**: losing every passkey is fine (password still works). Losing the
  password *and* passkeys is still unrecoverable — out of scope, but the
  admin-issued login link from `117b6b7` is the natural follow-up.
- **Multi-device discoverability**: resident keys synced by iCloud/Google are
  the norm; `backup_eligible`/`backup_state` are stored so the UI can label
  "synced" credentials later.
- **Conditional UI** is explicitly a stretch goal; decide after v1 ships.
- **Attestation** stays `none` (no trust policy, no admin approval flow).

## 9. Task checklist (implementation order)

- [ ] 1. Config: `internal/passkeys/config.go` + env wiring in `main.go` +
      validation tests; playbook/README env docs.
- [ ] 2. Migration `00020`, schema.sql, queries, `make generate`.
- [ ] 3. `internal/passkeys/store.go` + ceremony single-use/TTL tests.
- [ ] 4. `internal/passkeys/service.go` (begin/finish for both ceremonies) +
      interface for handlers.
- [ ] 5. Routes + handlers `internal/handlers/passkeys.go`, middleware public
      prefix, cookie handling, audit, rate limits.
- [ ] 6. `web/static/passkeys.js`, login/account templates, `block "scripts"`,
      CSS + `make css`.
- [ ] 7. Handler tests (stub service) + real-crypto integration test.
- [ ] 8. Docs: README features/env, `specs/disjoint.md` auth section.
- [ ] 9. Deploy with playbook; browser smoke on desktop + phone.
