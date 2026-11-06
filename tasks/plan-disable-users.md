# Implementation Plan - Disable Users (account suspension)

Add an admin ability to **disable** a non-admin account from `/admin/users`:
the account can no longer sign in, any live sessions are killed, and the user
is clearly marked as disabled in the admin user list. Disabling is reversible
via an **Enable** button.

Scope guardrails:

- Only **non-admin** users can be disabled. To disable an admin, demote first
  (mirrors the existing "cannot demote yourself" rule).
- An admin cannot disable themselves.
- Disabling is **not** deletion: posts, threads, uploads, invites and profile
  pages stay exactly as they are. The account simply cannot authenticate.
- Disabled users keep their username row, so the name cannot be re-registered.

## 1. Database

### `db/migrations/00019_user_disabled.sql` (new)

```sql
-- +goose Up
-- Admin-controlled account suspension. Disabled users cannot authenticate;
-- their existing sessions are deleted by the disable handler.
ALTER TABLE users ADD COLUMN is_disabled INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE users DROP COLUMN is_disabled;
```

Follow the existing style (`00016_invite_expiry.sql`, `00017_session_idle.sql`):
comment explaining the intent, `-- +goose Up` / `-- +goose Down`, additive and
backwards compatible so an older binary still runs against the migrated schema
(`DEFAULT 0`).

### `db/schema.sql`

Add `is_disabled INTEGER NOT NULL DEFAULT 0` to `CREATE TABLE users`
(after `is_admin`) to keep the sqlc snapshot in sync (see Makefile comment).

### `db/query.sql`

```sql
-- name: ListUsers :many
SELECT id, username, is_admin, is_disabled, created_at
FROM users ORDER BY id ASC;

-- name: ListMentionCandidates :many
-- Disabled accounts are not offered as @mention completions: they cannot
-- read what is posted, so completing them would only produce dead mentions.
SELECT
    u.username,
    u.avatar_path,
    u.avatar_nonce
FROM users u
LEFT JOIN user_presence p ON p.user_id = u.id
WHERE u.username LIKE ? ESCAPE '\'
  AND u.is_disabled = 0
ORDER BY (p.last_seen_at IS NULL) ASC, p.last_seen_at DESC, u.username ASC
LIMIT 10;

-- name: DisableUser :execrows
-- Guarded in SQL: only non-admin accounts may be disabled. 0 rows means the
-- target is an admin or does not exist.
UPDATE users SET is_disabled = 1 WHERE id = ? AND is_admin = 0;

-- name: EnableUser :exec
UPDATE users SET is_disabled = 0 WHERE id = ?;
```

`UpdatePasswordReset`-style atomicity: the admin check is in SQL *and* in the
handler so a promote/disable race cannot leave an admin disabled.

### Regenerate

```
export PATH="$HOME/go/bin:$PATH"
make generate   # sqlc generate (binary is in ~/go/bin)
```

`database.User` gains `IsDisabled int64`; `ListUsersRow` gains `IsDisabled`.
`GetUserByUsername` / `GetUserByFold` / `GetUserByID` return it automatically
(`SELECT *` / explicit column lists in `db/query.sql`).

## 2. Authentication enforcement (the important part)

Two independent gates, because sessions are long-lived:

### 2a. Sessions: `internal/middleware/middleware.go`

In `FirstRunAndAuth`, after the session user is loaded and before
`ContextWithUser` is called:

```go
if user.IsDisabled == 1 {
    // Kill the surviving session row so the cookie can never come back,
    // clear the cookie client-side, and treat the request as anonymous.
    _ = m.queries.DeleteSession(ctx, tokenHash)
    auth.ClearSessionCookie(w, r.TLS != nil) // match existing helper/secureCookie semantics
    currentUser = nil
}
```

Then fall through to the existing anonymous path, which already redirects to
`/login` (or sets `HX-Redirect` for HTMX requests). Because `currentUser` is
nil, presence (`UpsertUserPresence`) is skipped too.

Cookie `Secure` flag: the middleware does not currently read `COOKIE_SECURE`
(the helper lives in `handlers`). Either
- move `secureCookie` into `internal/auth` and use it from both packages
  (preferred, removes duplication), or
- simply omit the cookie clear and rely on the session row deletion + anonymous
  redirect; the stale cookie then just fails lookup.
Either way the row deletion is what makes the logout permanent.

### 2b. Login: `internal/handlers/auth.go` → `LoginSubmit`

After the password check succeeds, before `startSession`:

```go
if user.IsDisabled == 1 {
    setFlash(w, r, "flash_error", "This account has been disabled. Contact an administrator.")
    http.Redirect(w, r, "/login", http.StatusSeeOther)
    return
}
```

Keep the password check first and the wording generic enough not to leak much;
"account disabled" after a *correct* password is intentional (it tells a
legitimate user why they cannot get in). Do **not** create a session.

`RegisterSubmit` needs no change: disabled accounts still own their username,
and `usernamePattern`/`GetUserByUsernameFold` already block reuse.

### 2c. Optional notice for the kicked-out browser

`web/templates/login.html` shows a notice when `?disabled=1` is present, and the
middleware redirect becomes `/login?disabled=1`:

```go
{{if .Disabled}}<p class="notification is-warning">Your account has been disabled.</p>{{end}}
```

`LoginPage` passes `{"Disabled": r.URL.Query().Get("disabled") == "1"}`.
This is a nicety; the flash message on the next login attempt is the real
feedback.

## 3. Handlers — `internal/handlers/users.go`

Both follow the exact shape of `DemoteUserSubmit`:

```go
func (app *App) DisableUserSubmit(w http.ResponseWriter, r *http.Request) {
    user := requireAdmin(w, r)
    if user == nil { return }
    id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
    if err != nil { /* flash "Invalid user ID"; redirect /admin/users */ }

    if id == user.ID {
        // flash_error "You cannot disable your own account"
    }
    rows, err := app.queries.DisableUser(r.Context(), id)
    if err != nil { /* flash_error "Failed to disable user" */ }
    else if rows == 0 { /* flash_error "Only non-admin users can be disabled" */ }
    else {
        _ = app.queries.DeleteSessionsForUser(r.Context(), id) // already exists
        audit(r, user.ID, "disable_user", id)
        setFlash(w, r, "flash_success", "User disabled")
    }
    http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func (app *App) EnableUserSubmit(...) // EnableUser + audit "enable_user", flash "User enabled"
```

Notes:
- `DeleteSessionsForUser` already exists in `db/query.sql` — no new query.
- Self-disable check is belt-and-braces with the SQL `is_admin = 0` guard
  (self is always an admin here, since `requireAdmin` passed).
- `EnableUser` is ungated: enabling a demoted-then-re-disabled account is safe,
  and enabling an already-enabled account is a no-op.

### Routes — `internal/handlers/app.go`

```go
mux.HandleFunc("POST /admin/users/{id}/disable", app.DisableUserSubmit)
mux.HandleFunc("POST /admin/users/{id}/enable", app.EnableUserSubmit)
```

CSRF and body-size middleware already wrap the whole mux, so the forms need no
special handling. Update the "Admin User Administration" comment block if the
demo/route list in `README.md` documents these paths.

## 4. Template — `web/templates/users.html`

- Add a `Status` column between `Role` and `Joined`:
  - `{{if eq .IsDisabled 1}}<span class="tag is-danger">Disabled</span>{{else}}<span class="muted is-size-7">Active</span>{{end}}`
- Actions cell (non-admins only, keep the existing Promote form):

```html
{{if eq .IsAdmin 1}}
  ... existing Revoke Admin ...
{{else}}
  <div class="buttons is-right">
    <form method="POST" action="/admin/users/{{.ID}}/promote">...</form>
    {{if eq .IsDisabled 1}}
      <form method="POST" action="/admin/users/{{.ID}}/enable">
        <button class="button is-primary is-small" type="submit">Enable</button>
      </form>
    {{else}}
      <form method="POST" action="/admin/users/{{.ID}}/disable"
            onsubmit="return confirm('Disable {{.Username}}? They will be signed out and unable to log in.');">
        <button class="button is-danger is-small" type="submit">Disable</button>
      </form>
    {{end}}
  </div>
{{end}}
```

Use the same `confirm(...)` inline-JS pattern as `post_card.html` /
`thread.html` / `account.html` (no new JS files).

Also: promoting a **disabled** user should be refused rather than silently
creating a disabled admin. In `PromoteUserSubmit`, check
`GetUserByID` → `IsDisabled == 1` → flash `"Enable the account before promoting
it"`. (Alternative: auto-enable on promote; pick the refusal so the status
column and role column never disagree.)

## 4b. Mention autocomplete — `db/query.sql` + `internal/handlers/mentions.go`

Disabled accounts must not appear in the `@` autocomplete on the compose box.

- **Query** (`ListMentionCandidates`, shown in section 1): add
  `AND u.is_disabled = 0` to the `WHERE` and extend the comment. This is the
  only change needed — `MentionSuggestions` calls the query and passes rows
  through unchanged, so `internal/handlers/mentions.go` needs no code edit.
- This is the sole consumer of `ListMentionCandidates` (verified), so no other
  user-picker surface changes.
- **Deliberately unchanged**:
  - Mentions already written in existing posts still render as profile links.
    The renderer resolves usernames against the `users` table, and the profile
    page stays reachable — nobody's history is rewritten and no reply breaker
    appears.
  - Users can still *type* a disabled user's handle by hand; the mention is
    stored and rendered like any other. Filtering completions is a UX nudge,
    not a content rule (consistent with "unknown users are legal mentions",
    per the handler comment).
- Presence sorting is unaffected: the `LEFT JOIN user_presence` row may still
  exist for a disabled user, it is just never selected.

## 5. Tests

`internal/handlers/handlers_test.go` — extend the existing admin-user test
(`setupTestApp` creates `admin`, `member_one`, `second_admin`, plus
`authCookieFor`):

1. Disable a member as admin → 303 to `/admin/users`, `GetUserByID(...).IsDisabled == 1`.
2. Disabling deletes that user's sessions: `GetSessionUserID` with
   `authCookieFor`'s token now fails (or assert via middleware: request with the
   old cookie → 303 to `/login`).
3. Disabled user's `POST /login` with the **correct** password → 303 to
   `/login`, no `Set-Cookie` session, flash set.
4. Admin disabling themselves → flash_error, still `IsDisabled == 0`.
5. Admin disabling another **admin** → flash_error, unchanged (0 rows).
6. Non-admin `POST /admin/users/{id}/disable` → 303 to `/` (Unauthorized),
   target unchanged.
7. Enable flow: disable then enable → `IsDisabled == 0`, login succeeds again.
8. Audit rows `disable_user` / `enable_user` exist.
9. Promote of a disabled user is refused.

`internal/handlers/mentions_test.go` — extend `TestMentionSuggestionsEndpoint`
(which already creates `alice`, `albert`, `alex` and asserts the exact
`q=al` name list):

10. Disable a matching user (e.g. `alex`) via `queries.DisableUser`, then
    `q=al` returns `albert,alice` — the disabled user is absent.
11. Re-enable that user (`queries.EnableUser`) and `q=al` returns all three
    again, proving the filter is on `is_disabled` and not a side effect.
12. Disabling a user who is currently top of the list (recently active) does
    not panic the presence-ordering join: disable a user with a
    `UpsertUserPresence` row and assert the request still returns 200 with the
    remaining users.

`internal/middleware/middleware_test.go` — one test for 2a: valid session
cookie for a disabled user yields 303 `/login` and the session row is gone.

Run: `go test ./...` and `go test -race ./internal/...` before deploy.

## 6. Rollout

1. `make generate` (sqlc) → `gofmt`/`go vet ./...`.
2. `make test`.
3. Deploy with the existing playbook:
   ```
   spot -p deploy-disjoint.yml -t node1.prd.springupsoftware.com \
     -e DOMAIN:disjoint.kzrl.org -e APP_PORT:8082
   ```
   Goose runs `00019_user_disabled.sql` at startup **before** the new binary
   serves requests, so no window where the app queries a missing column.
4. Smoke test: `/admin/users` shows the Status column and Disable buttons;
   after disabling a test account, `GET /api/mentions?q=<prefix>` (while signed
   in) no longer lists it. The service restarts briefly (Phase 5), so expect a
   few seconds of downtime.
5. Rollback: revert the commit and redeploy; `is_disabled` is additive and
   ignored by the old binary (migration Down only needed for a clean schema).

## 7. Follow-ups / explicitly out of scope

- No "reason" field or disable timestamps (could become `disabled_at` +
  `disabled_by` later, mirroring `audit_log`).
- No email/notification to the disabled user (there is no mail subsystem).
- Disabled users' posts/threads are untouched; if moderation ever needs
  "hide content from disabled accounts", that is a separate feature.
- No "hide disabled users from other people's directories" feature: profile
  pages, post authorship and avatar routes all still work for disabled
  accounts. Only the admin list marks them and only autocomplete omits them.
