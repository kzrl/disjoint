# Implementation Plan - Passkey-only signup

Let an invited person create an account **with a passkey and no password**, while
keeping the existing password registration as the default. This builds on
`tasks/plan-passkeys.md` (passkey login/registration already ships) and adds a
password-less account state to the whole application.

Scope guardrails:

- **Opt-in per signup**, never the default: the invite page keeps the password
  form and adds a "Create account with a passkey instead" path.
- **First-run setup and admin accounts keep passwords.** The bootstrap admin is
  the instance's recovery of last resort; it must never be password-less.
- A passkey-only account can **set a password later** (escape hatch), gated by a
  fresh passkey assertion, so it can never be taken over from a stolen session.
- Existing password accounts are untouched. Passkeys stay additive for them.
- No email, no password reset: a passkey-only user who loses every device is
  still recoverable only by an admin (disable/delete) — documented, not solved
  here. The UI nags for a second passkey instead.

## 1. Data model (migration `00021`)

SQLite cannot drop a `NOT NULL` constraint in place, and rebuilding `users`
while eight tables reference it is the riskiest operation this app would ever
run. Avoid both: keep the column, add explicit state.

```sql
-- +goose Up
-- Passkey-only accounts still need a non-NULL password_hash (the column keeps
-- its NOT NULL constraint, so no table rebuild). password_set is the
-- authoritative flag; when 0 the stored hash is a bcrypt digest of random
-- bytes that nothing knows, and the password login path refuses before
-- comparing.
ALTER TABLE users ADD COLUMN password_set INTEGER NOT NULL DEFAULT 1;

-- Signup ceremonies create the account row before the authenticator answers
-- (WebAuthn needs a user handle up front). The row is invisible, cannot log in,
-- and is deleted by the worker if the ceremony is abandoned.
ALTER TABLE users ADD COLUMN pending_until DATETIME; -- NULL = active account

CREATE INDEX IF NOT EXISTS idx_users_pending ON users(pending_until);

-- The signup ceremony must remember which invite it is redeeming, and what
-- username was requested, without touching the invite until the attestation
-- succeeds.
ALTER TABLE webauthn_ceremonies ADD COLUMN context TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE webauthn_ceremonies DROP COLUMN context;
DROP INDEX IF EXISTS idx_users_pending;
ALTER TABLE users DROP COLUMN pending_until;
ALTER TABLE users DROP COLUMN password_set;
```

Mirror in `db/schema.sql`; `make generate`.

### User-handle note

Existing credentials use `userHandle = 8-byte big-endian user id`
(`internal/passkeys/user.go`). Signup creates the pending user row **before**
`begin`, so the handle is `userHandle(pendingUser.ID)` as usual — no handle
scheme change, no migration of existing credentials, and discoverable login
keeps its "handle must match the credential's owner" check.

### Queries (`db/query.sql`)

```sql
-- name: CreatePendingUser :one        -- username + unusable hash + pending_until
-- name: ActivatePendingUser :execrows -- SET pending_until = NULL, password_set = 0
-- name: DeletePendingUser :execrows   -- WHERE id = ? AND pending_until IS NOT NULL
-- name: DeleteExpiredPendingUsers :exec   -- pending_until < now
-- name: SetUserPassword :exec         -- hash + password_set = 1
-- name: GetUserByUsername :one        -- unchanged, but callers must check pending_until
```

Every query that enumerates people gains `AND pending_until IS NULL`:
`CountUsers` (first-run check must not be satisfied by a half-finished signup),
`ListUsers` (admin list), `ListMentionCandidates`, `ListOnlineUsers` (already
join-based, but presence must never be written for pending users anyway).
`GetUserByUsername`/`GetUserByID` stay permissive so the ceremony can load the
pending row; callers enforce visibility.

## 2. Service layer (`internal/passkeys`)

Extend the existing service rather than adding a parallel one:

```go
// BeginSignup validates the invite + username, creates the pending user and the
// registration ceremony, and returns the creation options.
func (s *Service) BeginSignup(ctx context.Context, code, username, passwordHash string) (Options, error)

// FinishSignup verifies the attestation, activates the pending user, consumes
// the invite atomically and returns the new account.
func (s *Service) FinishSignup(ctx context.Context, token string, r *http.Request) (database.User, error)
```

Details:

- `BeginSignup` requires a valid, unused invite (`GetInviteByCode`) and a free
  username (`GetUserByUsernameFold`, which must now also see pending rows so two
  ceremonies cannot claim the same name). It returns distinct errors:
  `ErrInviteInvalid`, `ErrUsernameTaken`, `ErrRateLimited` (see §6).
- The ceremony's `context` column stores JSON `{"invite":"<code>"}`; the username
  is recoverable from the pending user row (`user_id`).
- `FinishSignup` takes the ceremony (single use, as today), verifies the
  attestation against the pending user, then in **one transaction**:
  1. `ConsumeInvite(code, used_by = user.ID)` — 0 rows means a concurrent signup
     redeemed it first: delete the pending user and fail;
  2. `ActivatePendingUser(user.ID)` (clears `pending_until`, sets `password_set=0`);
  3. `CreateWebAuthnCredential` with the default label "Passkey 1".
  Any failure rolls back and the pending row is deleted, so a failed ceremony
  never leaves a squatted username or a consumed invite.
- Ceremony TTL is the existing 5 minutes; `pending_until` is 10 minutes (a
  little longer than the ceremony so a slow authenticator is not raced by the
  sweeper).
- Abandoned ceremonies need no special handling: the worker deletes pending users
  with `pending_until < now` (their ceremony rows expire on their own).

## 3. Handlers & routes

New routes next to the existing register ones (public, rate-limited by the
same per-IP `authLimiter`):

| Route | Body | Notes |
|---|---|---|
| `POST /register/passkey/begin` | JSON `{code, username}` | Validates invite + username, creates the pending user and ceremony, sets the ceremony cookie, returns options. |
| `POST /register/passkey/finish` | JSON credential | Verifies, activates, consumes the invite, starts a session via `app.startSession`, audits `passkey_signup`, returns `{"redirect":"/account"}` so the new user sees "add a second passkey". |

- Middleware: extend the public-route check from the exact `/login/passkey/`
  prefix to also allow `/register/passkey/` (both are pre-session ceremonies).
- Rate limits: the per-IP auth limiter on both; additionally cap pending users
  per IP is **not** tracked (no request IP stored). Instead cap *global* pending
  rows implicitly by the 10-minute TTL and by requiring a valid invite per
  begin, and add a per-username race guard (unique username constraint) — a
  spammer needs invites, which only admins mint.
- Password login path (`LoginSubmit`): after loading the user, if
  `password_set == 0` (or `pending_until` is non-NULL) answer
  "This account signs in with a passkey. Use “Sign in with a passkey”." — this
  is a deliberate, helpful message on a private instance; no password comparison
  is performed (the stored hash is random).
- Passkey login path (`PasskeyLoginFinish`) and middleware: reject
  `pending_until != NULL` accounts exactly like disabled ones (generic failure,
  no session). A pending user must never authenticate through a stray ceremony.
- Account page: for `password_set == 0`, render **"Set a password"** instead of
  "Change password" (no current-password field). Setting one requires a fresh
  passkey assertion (§4).
- Audit: `passkey_signup`, `set_password` (existing `change_password` stays for
  the password-change flow).

## 4. Set-password escape hatch (passkey re-auth)

A session alone must not mint a permanent password, mirroring "adding a passkey
requires the password". Reuse the login ceremony with a third kind
`CeremonyReauth`:

- `POST /account/password/begin` → assertion options restricted to the session
  user's credentials (`BeginLogin` with the user's allow-list, not discoverable).
- `POST /account/password/finish` → verifies, then stores a session-scoped
  "reauth until" marker (short-lived, e.g. 5 minutes, in a cookie or a column)
  and returns `{"ok":true}`; the password form then posts normally.
- Simplest robust variant: one endpoint that verifies the assertion **and**
  accepts the new password in the same finish body (`{credential, new_password}`),
  so no reauth marker is needed. Prefer this: stateless, one round trip, and the
  password is only accepted in the same request as the proof.

UI: "Set a password" opens the same minimal form (new + confirm), but the submit
runs `navigator.credentials.get()` first (synchronously in the tap, reusing the
Safari-safe pattern) and posts `{credential, new_password}`.

## 5. Registration UI & JavaScript

`web/templates/register.html`:

- Keep the current password form.
- Add a second panel: "Create account with a passkey" — username field, a
  **Create with passkey** button, and the existing invite code as a hidden field
  (the code travels in the JSON body, not the URL, so it stays out of logs).
- Explain the trade-off in one line: "No password. You must use this device (or
  another passkey you add) to sign in; an admin can disable the account but
  cannot recover it."
- The account page gets a banner for `password_set == 0`: "You have no password.
  Add a second passkey, or set a password." with links.

`web/static/passkeys.js` (or a new `signup.js` loaded only on `/register`):

- Same two-phase Safari-safe pattern as account: tap 1 fetches creation options
  (validating invite/username), tap 2 calls `create()` synchronously.
- Surfaces distinct errors: invalid/expired invite, username taken, rate limited,
  cancelled prompt.
- Reuses `b64urlToBuf`/`encodeRegistration`; extract those helpers into a shared
  file (`passkey-util.js`) if both pages need them, or keep one file and gate by
  element presence (current approach).

## 6. Tests

`internal/passkeys`:

1. `BeginSignup` rejects an invalid/used/expired invite and a taken username
   (including a username held by a *pending* row).
2. Full signup with the virtual authenticator: pending user created, attestation
   finishes, invite consumed exactly once, `password_set=0`,
   `pending_until IS NULL`, one credential stored.
3. Concurrent/replayed finish: second `FinishSignup` with the same token fails;
   two ceremonies racing the same invite — exactly one activates, the loser's
   pending row is deleted.
4. `FinishSignup` failure (bad attestation, wrong origin) deletes the pending row
   and leaves the invite unused.

`internal/handlers`:

5. Password login for a passkey-only account returns the passkey message and sets
   no session; pending accounts get the generic failure.
6. Passkey login works for an activated passkey-only account and sets a session;
   pending accounts are refused.
7. `POST /register/passkey/begin|finish` are public (no login redirect), CSRF-
   protected, and rate-limited.
8. Set-password: assertion + password in one request stores a bcrypt hash and
   flips `password_set=1`; without a valid assertion the password is unchanged;
   a stolen session alone cannot set one.
9. Admin user list / first-run `CountUsers` / mention autocomplete ignore pending
   rows.

`internal/worker`:

10. `CleanupOnce` deletes an expired pending user and leaves active accounts
    alone.

## 7. Rollout

1. `make generate`, `gofmt`, `go vet ./...`, `go test -race ./...`.
2. Deploy with the playbook (migration 21 applies at startup; additive columns,
   old binary keeps working if rolled back).
3. Smoke: mint an invite, open it in a private window, choose "passkey", create,
   land on `/account` with the passkey listed and the no-password banner; sign
   out; sign in with the passkey; set a password via the escape hatch; verify
   password login then works.
4. Rollback: revert the commit; the new columns are ignored by the old binary.
   Accounts created meanwhile keep working through password login only if they
   set one — document that caveat before enabling signup in production.

## 8. Risks & open questions

- **Recovery**: a passkey-only user who loses all devices is unrecoverable
  without an admin. Mitigations in scope: require the account to have one
  passkey (automatic), nag for a second, offer "set a password". Out of scope:
  admin-issued login links / password resets (worth doing next).
- **Pending-account abuse**: invites gate signup, TTL bounds the window, and
  pending rows are invisible and cannot authenticate. A determined inviter can
  still briefly squat usernames; acceptable.
- **Ceremony/worker ordering**: if the worker deletes a pending user while a slow
  authenticator is still finishing, `FinishSignup` fails cleanly (row gone) and
  the user retries. `pending_until` is deliberately longer than the ceremony TTL.
- **Privacy/UX**: telling a password login that the account is passkey-only
  reveals account existence, consistent with the existing "account disabled"
  message but worth a deliberate yes.
- **Two accounts, same person**: nothing prevents a user registering with a
  passkey and later being invited again for a password account; out of scope.
- Follow-ups: "remove password, become passkey-only" (the inverse migration),
  passkey management for admins, and email-based recovery if the app ever gains
  mail.

## 9. Task checklist (implementation order)

- [ ] 1. Migration `00021` (password_set, pending_until, ceremonies.context),
      schema.sql, queries, `make generate`.
- [ ] 2. Visibility rules: pending excluded from CountUsers, ListUsers,
      mention candidates; worker deletion of expired pending users + test.
- [ ] 3. `passkeys.Service.BeginSignup` / `FinishSignup` + typed errors,
      invite-in-ceremony context, transactional activation; unit/integration
      tests with the virtual authenticator.
- [ ] 4. Handlers + routes (public prefix, auth limiter, CSRF), session start,
      audit; password-login passkey-only message; pending rejected everywhere.
- [ ] 5. Set-password escape hatch (assertion + password in one request) and
      account-page variants for `password_set == 0`.
- [ ] 6. `/register` UI + JS (two-phase, shared helpers), no-password banner on
      `/account`, second-passkey nag.
- [ ] 7. Handler tests (password login, pending rejection, set-password,
      rate-limit/CSRF) and doc updates (README, `specs/disjoint.md`, plan).
- [ ] 8. Deploy + browser smoke (desktop and iOS), then decide whether to
      advertise passkey signup on the invite page by default.
