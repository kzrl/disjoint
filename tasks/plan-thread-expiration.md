# Implementation Plan: Thread Author Can Change the Lifespan / Expiration

## Overview

Let a thread's author change the thread's lifespan (24h / 3d / 7d / 30d /
Never) after creation, via a toggle button + inline select form in the
thread header — the same UI pattern as "Edit title".

**Scope (decided with user)**
- **Deadline semantics: reset.** Changing the lifespan sets
  `expires_at = now + new ttl` (or NULL for Never), consistent with
  creation and with how every reply resets the deadline to
  last-comment + ttl.
- **UI:** a "Change expiration" toggle button that reveals an inline
  select form (Save/Cancel), next to the existing "Edit title" toggle.
- **Choices:** the same five as creation — `24h`, `3d`, `7d`, `30d`,
  `never` — so the string→duration mapping lives in one place.
- Permissions: **author only** (consistent with editing); 404, never 403.

**Explicitly out of scope:** admin override, changing expiration from the
front page, arbitrary/custom durations, an "extend by X" quick action,
changing who the thread is attributed to.

## Current-state facts that shape the design

- `threads.ttl_seconds` (NULL = never) stores the chosen lifespan;
  `threads.expires_at` (NULL = never) is the live deadline. All expiry
  predicates, the worker purge, and the header tag operate on
  `expires_at` only — so a change is complete the moment both columns are
  written.
- `CreateThreadSubmit` (threads.go) already contains the string→duration
  mapping as an inline anonymous map + `expiresAt = now + duration
  .Round(0)`. It must be extracted so create and update share it.
- Every reply runs `BumpThreadExpiration` (`expires_at = now + ttl`,
  NULL stays NULL) — unchanged by this feature; a changed ttl is picked
  up by the next bump automatically.
- The header's right side already shows the `⏳ expires in …` /
  "Never expires" tag (reads `expires_at`, so it updates itself after a
  change).
- Ownership pattern from the editing feature: `:execrows` UPDATE with
  `user_id = ?` in the WHERE clause; 0 rows ⇒ 404; a Go-side check after
  load puts 404 ahead of validation errors.
- **`is-hidden`, not the `hidden` attribute** — Bulma's `.has-addons
  { display:flex }` overrides the UA `[hidden]` rule (bug fixed in
  88d8d73; the title-edit form already uses `is-hidden`).
- `render()` injects `CurrentUser`; authorship gating in the header is
  `{{if and .CurrentUser (eq .CurrentUser.ID .Thread.UserID)}}` (already
  wraps the title-edit block).
- `handlers_test.go` patterns: `TestThreadTitleEditing` is the closest
  template (rename helper, flash assertions, UI assertions incl. the
  initial-hidden state).

## Architecture decisions

### D1 — One `expirationToTTL` helper shared by create and update
Extract from `CreateThreadSubmit` into threads.go:

```go
// expirationToTTL maps the form's lifespan choice to its duration.
// "never" (and any unknown value) reports ok=false.
func expirationToTTL(choice string) (time.Duration, bool) {
    switch choice {
    case "24h": return 24 * time.Hour, true
    case "3d":  return 3 * 24 * time.Hour, true
    case "7d":  return 7 * 24 * time.Hour, true
    case "30d": return 30 * 24 * time.Hour, true
    }
    return 0, false
}
```

`CreateThreadSubmit` keeps its existing behavior exactly ("never"/unknown
→ NULLs) — this is a pure refactor pinned by the existing thread-creation
and TTL tests.

### D2 — `SetThreadExpiration` query; deadline reset; no `updated_at` bump
```sql
-- name: SetThreadExpiration :execrows
-- Author-only lifespan change. expires_at is reset to the value computed
-- by the caller (now + ttl, or NULL for never-threads) — the same
-- "ttl of silence from now" rule every reply applies. updated_at is
-- deliberately untouched: a settings change is not a comment.
UPDATE threads
SET ttl_seconds = ?, expires_at = ?
WHERE id = ? AND user_id = ?;
```
Caller computes (mirroring creation): `never` → both NULL; otherwise
`ttl_seconds = int64(d.Seconds())`, `expires_at = now + d` (`.Round(0)`,
same as creation). Both params are `sql.Null*`.

### D3 — Handler `UpdateThreadExpirationSubmit`, route `POST /threads/{id}/expiration`
Order mirrors the title handler: auth (HTMX-aware login guard) → parse id
→ parse form → load thread (404) → Go-side author check (404) → validate
choice (`expirationToTTL`; unknown → `flash_error` + redirect back) →
no-op detection (submitted choice matches the stored state:
`ttl_seconds` NULL ↔ "never", equal seconds ↔ that choice → flash
"No changes to save.", redirect, no write — keeps `expires_at`
byte-identical) → `SetThreadExpiration` (0 rows ⇒ 404) → flash
"Expiration updated." → redirect to `/threads/{id}`.

### D4 — UI: second toggle in the author block, generic toggle script
- The existing author-only `{{if}}` block in the thread header gains a
  second pair: a "Change expiration" button
  (`id="change-expiration-btn"`) + an inline form
  (`id="expiration-edit-form"`, `class="field has-addons mb-1 is-hidden"`)
  holding a `<select name="expiration">` with the five options and
  Save/Cancel. The select's current choice is preselected.
- **Preselect** via a new funcMap entry in `app.go` (keeps the template
  free of six numeric comparisons):
  `expirationIs(ttl sql.NullInt64, seconds int64) bool` — true when the
  stored ttl equals that choice's seconds. "never" preselects when
  `!ttl.Valid` (template: `{{if not (or (expirationIs …) …) }}` — or a
  second helper `expirationIsNever(ttl) bool`; pick whichever renders
  more cleanly, smallest template wins).
- **Script:** replace the title-edit's bespoke script with one small
  delegated listener handling both toggles: each button carries
  `data-toggle-form="#<form-id>"`; on click, toggle `is-hidden` on the
  button and the target form, and focus+select the form's input when it
  becomes visible (the select gets `.focus()`, no select-all). One
  script, two controls, same inline-script style as before.
- The `⏳ expires in …` / "Never expires" tag is untouched and stays
  visible to everyone (including the author, next to the button).

## Task List

### Phase 1: Foundation
- [x] **Task 1: `expirationToTTL` refactor + `SetThreadExpiration` query** (DONE)
  - Extract the mapping in threads.go; `CreateThreadSubmit` calls it
    (behavior unchanged)
  - Add `SetThreadExpiration` (D2) to `db/query.sql`; `sqlc generate`
    (no schema change — no migration needed)
  - **Verify:** `go build ./...` + full suite green (creation, TTL-bump,
    and expiry tests are the regression net for the refactor)
  - Files: `internal/handlers/threads.go`, `db/query.sql`,
    `internal/database/*` (generated)
  - **Pitfall hit:** sqlc v1.31.1's lexer mis-parses the rest of the query
    file after a non-ASCII byte in a comment (em dash in the new comment
    corrupted every later query: SELECT→SELEC…). ASCII-only in db/query.sql.

### Checkpoint: Foundation
- [x] Build + tests green; behavior unchanged (full suite green)

### Phase 2: Expiration change (vertical slice)
- [x] **Task 2: `UpdateThreadExpirationSubmit` handler + route** (DONE)
  - Handler per D3 in threads.go; `POST /threads/{id}/expiration` in
    `app.go` `Routes()`
  - **Verify:** `TestThreadExpirationEditing` in handlers_test.go:
    never→7d (ttl + deadline ≈ now+7d, flash, redirect), 7d→24h
    (deadline moves earlier — reset, not keep), →never (both NULL),
    invalid choice (flash_error, untouched), no-op (deadline
    byte-identical, "No changes to save."), non-author 404 (untouched),
    expired thread 404, unauthenticated → /login
  - Files: `internal/handlers/threads.go`, `internal/handlers/app.go`,
    `internal/handlers/handlers_test.go`

- [x] **Task 3: Header UI — toggle, select with preselect, shared script** (DONE)
  - thread.html: second toggle pair in the author block (D4);
    `is-hidden` (NOT the `hidden` attribute)
  - app.go funcMap: `expirationIs` (+ `expirationIsNever` if needed)
  - Replace the title script with the generic `data-toggle-form`
    delegated listener (title toggle keeps working — existing test is
    the regression)
  - **Verify:** extend `TestThreadExpirationEditing` — author page has
    button + form starting `is-hidden`, select preselects the stored
    choice (never and a TTL case); non-author page has neither; title
    toggle still renders (`id="edit-title-btn"`, form still `is-hidden`)
  - Files: `web/templates/thread.html`, `internal/handlers/app.go`,
    `internal/handlers/handlers_test.go`

### Phase 3: Docs
- [x] **Task 4: Update the spec** (DONE)
  - `specs/disjoint.md` §C (The Ephemerality Twist): the author may
    change the lifespan any time before expiry via the thread header
    (`POST /threads/{id}/expiration`); the deadline resets to now + new
    ttl (Never → NULL); the next reply then bumps from the new ttl
  - Files: `specs/disjoint.md`

### Checkpoint: Complete
- [x] All acceptance criteria met; full suite green (make build, go vet,
      gofmt, go test -count=1 ./...)
- [ ] Manual pass in a browser: change a never-thread to 24h, watch the
      tag update; change back to never; verify a reply resets from the new
      ttl (covered by the test suite, but worth one human click-through)
- [x] Ready for review

## Risks and Mitigations
| Risk | Impact | Mitigation |
|------|--------|------------|
| Refactor of the inline TTL map changes creation behavior | Med | Pure extraction; existing creation/TTL/expiry tests must pass untouched |
| `hidden` attribute regression (Bulma display override) | Low | Use `is-hidden`; test asserts the exact class list, as the title form does (88d8d73) |
| Deadline comparison in tests is clock-sensitive | Low | Assert windows (e.g. new deadline < old deadline after reset; within a few seconds of now+ttl), never exact equality, except the no-op case (byte-identical, no clock involved) |
| Preselect logic wrong for NULL ttl | Low | Dedicated template helper + test asserts preselection for both never and a TTL value |
| Worker purges a thread the author just "saved" by switching to Never | Low | The UPDATE is a single atomic statement; a thread that is already past its deadline is 404 to the editor anyway (GetThreadByID filter) |

## Open questions
- None. If we later want a quick "extend by 24h" action or custom
  durations, the helper + query make that a small addition.
