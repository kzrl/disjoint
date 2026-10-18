# Implementation Plan: Editing Posts and Thread Titles

## Overview

Let a post's author edit the post's **body (Markdown)** and **first-class
link**, and let a thread's author edit the **thread title**. Everything else
is out of scope.

**Scope (decided with user)**
- Editable: post `body`, post `url` (first-class link), thread `title`.
- NOT editable: attachments (no add/remove on edit), expiration/TTL, the
  `is_anonymous` flag, authorship.
- Permissions: **author only** — no admin override (deliberately narrower
  than the emoji-deletion "admin can do any" precedent).
- History: a `posts.updated_at` column (NULL until first edit) drives a dim
  "(edited)" marker on the post card. No edit-history table.

**Explicitly out of scope:** edit history/audit trail, admin edit
overrides, editing attachments or the anonymity flag, inline (in-place)
edit UI, editing expiration.

## Current-state facts that shape the design

- Migrations run through **goose** single-file format
  (`db/migrations/NNNNN_*.sql` with `+goose Up` / `+goose Down`);
  `db/schema.sql` is the cumulative snapshot that **sqlc** reads
  (`sqlc.yaml` → `internal/database`). Both must change together. Next
  version: **00013**.
- Post rendering is shared: `post_card.html` is a `{{define}}` parsed into
  every template set and into the HTMX partial set (`post_partial.html`
  just includes it). One change to `post_card.html` covers the thread
  page, HTMX-appended replies, and the edited-post response.
- The thread page already has a viewer-injection pattern: the anonymous-
  attribution work captured `{{$viewer := .CurrentUser}}` before the
  `{{range .Posts}}` loop because `range` shadows the data map. The
  author-only "Edit" button reuses exactly this.
- Every mutating handler in the app is a **POST** on a plain route
  (`POST /threads/{id}/posts`, `POST /posts/{id}/reactions`, …). Edit
  routes follow the same shape; no PUT/PATCH.
- `CreatePostSubmit` already contains all the validation to reuse:
  `cleanPostURL` (absolute http(s), ≤2048 chars, host required) and the
  "post needs a body OR a link OR an attachment" rule. On edit the
  attachment half of that rule is fixed by construction (attachments
  can't change), so the rule adapts (see D3).
- Ownership checks in this app are done in the handler against the loaded
  row (e.g. emoji deletion compares `created_by` to the current user).
  For edits we enforce ownership **in the UPDATE's WHERE clause**
  (`user_id = ?`) with `:execrows` — 0 rows ⇒ 404, which avoids leaking
  whether a post exists to a non-author.
- `handlers_test.go` is the single test file; it asserts on both DB state
  and rendered HTML (precedent: `TestAnonymousReplies`,
  `TestAdminAnonymousAttribution`). New tests follow that pattern.

## Architecture decisions

### D1 — `posts.updated_at`, NULL = never edited
```sql
ALTER TABLE posts ADD COLUMN updated_at DATETIME;  -- NULL until first edit
```
Mirrors `threads.updated_at` but nullable so the "(edited)" marker can be
derived with one truthy test. No `edit_count` (would need a trigger or
Go-side increment for little value). `created_at` remains the original
timestamp and is never touched — the marker shows `(edited)` next to the
creation time, we do not replace it with the edit time.

### D2 — Ownership enforced in SQL, not in Go
```sql
-- name: UpdatePost :execrows
UPDATE posts SET body = ?, url = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ? AND user_id = ?;

-- name: UpdateThreadTitle :execrows
UPDATE threads SET title = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ? AND user_id = ?;
```
`:execrows` → 0 rows means "not found *or* not yours"; both return 404.
This makes the permission check race-free and un-bypassable. The handlers
still load the post/thread first anyway, for the expiry check and the
no-op detection (D3).

### D3 — Edit validation adapts the creation rule
- `url` goes through the same `cleanPostURL` as creation (empty allowed).
- Content rule: **if the post has no attachments**, `body` or `url` is
  required (same as creation). **If it has attachments**, an empty body+url
  is legal — an attachment-only post can be edited down to attachments
  only. (We can't just re-run the creation rule verbatim; an
  attachment-only post would become uneditable.)
- **No-op saves are skipped**: if trimmed `body` and cleaned `url` are both
  unchanged, the handler returns success without an UPDATE, so
  `updated_at` stays NULL and "(edited)" doesn't appear on a resubmitted
  form.
- Title: `TrimSpace`, non-empty, capped at 200 chars (the front-page rows
  are single-line; creation today has no cap, we don't retrofit it here).
- Expired threads: 410 (posts) / 404 (title) — same behavior as creation
  (`CreatePostSubmit` uses `StatusGone`).

### D4 — Dedicated edit page + standard form + redirect (HTMX optional)
- `GET /posts/{id}/edit` renders a small page: prefilled `body` textarea +
  `url` input, a note that attachments can't be changed, Save/Cancel.
  Author only; non-author/expired/unknown → 404.
- `POST /posts/{id}/edit` validates, updates, flashes "Post updated.",
  redirects to `/threads/{tid}#post-{id}`.
- No HTMX swap for now: a full-page redirect to the anchored post is
  simple, testable, and matches how `CreateThreadSubmit` behaves. (The
  reply path's HTMX partial pattern remains the precedent to copy later
  if we ever want in-place editing; the re-rendered `post_card` needs no
  new template work either way.)
- **Edit button**: on `post_card.html`, a small "Edit" button (styled like
  the existing `quote-btn`) rendered only when `CurrentUser` is the post's
  author (`$viewer.ID == .Post.UserID`). Captured with
  `{{$viewer := .CurrentUser}}` in `thread.html` before the `range`,
  passed down in the `PostViewItem`-render path — same mechanism as the
  admin anonymity attribution.

### D5 — Thread title edit: inline form in the thread header
- `thread.html` header: an "Edit title" button for the author only,
  toggling a small inline form (title input + Save/Cancel) via a tiny
  inline `<script>` — consistent with the existing quote-button inline
  script (no CSP, delegated patterns already precedented).
- `POST /threads/{id}/title` → validate, `UpdateThreadTitle`, flash,
  redirect back to `/threads/{id}`.
- Side effects come for free: the front page shows the new title (title is
  read live), the `<h1>` updates, and the browser tab title is passed per
  render. `threads.updated_at` is bumped — harmless, nothing keys off it
  for expiry (that's `expires_at` only).
- Editing the *opening post's body* is just the normal post-edit flow
  (D4) — no special case. The front page's last-comment preview picks up
  body edits automatically because it's a live subquery.

### D6 — Known, accepted semantic changes
- **Quotes can go stale.** The spec currently says "since posts are
  immutable a stored quote can never go stale" — that's no longer true.
  Accepted: a quote is a point-in-time copy; the "view original"
  permalink line still resolves. No mechanism to mark or refresh old
  quotes (out of scope).
- **Reactions survive edits.** A reaction to a post whose body was
  completely rewritten stays valid — reactions are per-post, not per-text.
- **`(edited)` on HTMX-appended cards** is automatic: new posts have
  `updated_at = NULL`, the marker is just a `{{if}}`.

## Task List

### Phase 1: Foundation
- [x] **Task 1: `posts.updated_at` migration + sqlc queries** (DONE)
  - `db/migrations/00013_posts_updated_at.sql` (single-file): Up = `ALTER TABLE posts ADD COLUMN updated_at DATETIME;`, Down = `DROP COLUMN` (verified working)
  - `db/schema.sql` snapshot updated; `db/query.sql`: added `UpdatePost` / `UpdateThreadTitle` (`:execrows`, `user_id = ?` in WHERE per D2), `p.updated_at` added to `ListPostsByThread` + `GetPostByID` (plain column → sqlc emits `sql.NullTime`, no COALESCE needed)
  - `sqlc generate` (v1.29 at ~/go/bin/sqlc); `go build ./...` + `go test -count=1 ./...` green
  - Verified: fresh DB → v13; copy of pre-existing `disjoint.db` upgraded v12→v13 (live DB untouched); `goose.DownTo(12)` → v12, column gone. NOTE: goose CLI (`go run github.com/pressly/goose/v3/cmd/goose`) does NOT work in this module — its cmd package needs a mysql driver dep not in go.mod; use a tiny program calling `goose.DownTo` instead
  - `db/migrations/00013_posts_updated_at.sql` (single-file: Up =
    `ALTER TABLE posts ADD COLUMN updated_at DATETIME;`, Down =
    `ALTER TABLE posts DROP COLUMN updated_at;`)
  - Update `db/schema.sql` snapshot; add `UpdatePost` (D2) and
    `UpdateThreadTitle` (D2) to `db/query.sql`; select `p.updated_at` in
    `ListPostsByThread` and `GetPostByID` (plain column → sqlc emits
    `sql.NullTime`, no COALESCE needed)
  - `make sqlc` / `sqlc generate`; `go build ./...`
  - **Verify:** build + full test suite green; fresh DB reaches goose
    v13; copy of pre-existing DB upgrades to v13; `goose.Down` to v12
    drops the column
  - Files: `db/migrations/00013_posts_updated_at.sql`, `db/schema.sql`,
    `db/query.sql`, `internal/database/*` (generated)

### Checkpoint: Foundation
- [x] `go build ./...` + `go test ./...` green on fresh and pre-existing DBs

### Phase 2: Post editing (vertical slice)
- [x] **Task 2: `UpdatePostSubmit` handler + route** (DONE)
  - `POST /posts/{id}/edit` in `posts.go` + registered in `app.go` `Routes()`
  - Order: auth → parse id → parse form → load post (404) → Go-side author check (404, keeps 404 ahead of validation errors) → thread load + expiry (404 via `GetThreadByID`'s expiry filter, same as creation) → `cleanPostURL` (400) → content rule with attachment-count exception (new `CountAttachmentsByPostID` query) → no-op skip → `UpdatePost :execrows` (0 rows ⇒ 404)
  - `TestPostEditing`: 8 cases — author edit (DB + timestamps + flash + anchor redirect), no-op save leaves `updated_at` untouched, non-author 404 with post untouched, `javascript:` url 400, empty body+url 400 without attachments, empty allowed with attachments, expired thread 404, unauthenticated → /login. Full suite green
  - `POST /posts/{id}/edit` in `posts.go` + registered in `app.go`
    `Routes()`
  - Load post (404 if missing), 410 if thread expired, author check via
    `UpdatePost :execrows` (404 on 0 rows), D3 validation + no-op skip,
    flash + redirect to `/threads/{tid}#post-{id}`
  - **Verify:** `TestPostEditing` in `handlers_test.go` — author edit
    updates body+url in DB and sets `updated_at`; no-op save leaves
    `updated_at` NULL; non-author gets 404 (not 403); bad `url` (relative,
    `javascript:`) rejected 400; empty body+url rejected when no
    attachments but allowed when the post is attachment-only; expired
    thread → 410
  - Files: `internal/handlers/posts.go`, `internal/handlers/app.go`,
    `internal/handlers/handlers_test.go`

- [x] **Task 3: Edit UI — edit page, button, "(edited)" marker** (DONE)
  - New template `web/templates/edit_post.html` (registered in `app.go` `NewApp` pages list): prefilled body/url, note that attachments can't change, Save/Cancel
  - `GET /posts/{id}/edit` handler (`EditPostPage` in `posts.go`, author only, 404 otherwise) + route
  - `post_card.html`: Edit button (`is-small is-secondary` like the quote button) via `.CanEdit`; "(edited)" marker after the timestamp when `UpdatedAt.Valid` (shows to ALL viewers — it is content metadata; only the button is author-gated)
  - Authorship plumbed via `PostViewItem.CanEdit` (set in `ThreadPage` from the viewer's user id — cleaner than a `$viewer` template capture) and `SinglePostViewItem.CanEdit` (always true in `CreatePostSubmit` — the HTMX partial's only viewer is the post's own author; without the field the shared `post_card` define would fail to execute on the partial's struct)
  - Verified: `TestPostEditing` step 9 — author page shows button + marker + new content; non-author sees marker but no button; edit page prefills body+url; non-author edit page 404. Full suite green (HTMX reply partial regression included)

### Checkpoint: Post editing
- [x] Author can edit body/link end-to-end (edit page → save → anchored
      redirect → "(edited)" marker); non-author sees no button and gets
      404 on direct URLs
- [x] Full suite green

- [x] **Task 4: `UpdateThreadTitleSubmit` handler + route** (DONE)
  - `POST /threads/{id}/title` in `threads.go` + route; `maxThreadTitleLength = 200`
  - Order mirrors post editing: auth → id → form → load (404) → Go-side author check (404) → validation (empty / >200 chars → `flash_error` + redirect back, matching the `CreateThreadSubmit` pattern) → no-op skip → `UpdateThreadTitle :execrows` (0 rows ⇒ 404) → flash "Title updated." + redirect
  - `TestThreadTitleEditing`: 8 cases — rename (trim + DB + flash + thread/front-page render), no-op "No changes to save.", empty/whitespace rejected, >200 chars rejected (title untouched in both), non-author 404, expired thread 404, unauthenticated → /login. Full suite green
  - `POST /threads/{id}/title` in `threads.go` + route
  - D3 title validation (non-empty, ≤200), `UpdateThreadTitle :execrows`
    (404 on 0 rows), 404 if expired, flash + redirect to `/threads/{id}`
  - **Verify:** `TestThreadTitleEditing` — author rename updates DB;
    non-author 404; empty/whitespace title rejected; >200 chars rejected;
    expired thread 404
  - Files: `internal/handlers/threads.go`, `internal/handlers/app.go`,
    `internal/handlers/handlers_test.go`

- [x] **Task 5: Title-edit UI in the thread header** (DONE)
  - `thread.html`: "Edit title" button in the header box gated on `and .CurrentUser (eq .CurrentUser.ID .Thread.UserID)` (CurrentUser is injected by `render()`), toggling an inline form (title input prefilled, `maxlength=200`, Save/Cancel) via a small inline `<script>` in the quote-button style (toggle `hidden`, focus+select the input)
  - Verified: `TestThreadTitleEditing` step 9 — author page has button + prefilled form (`action=/threads/{id}/title`, `value="renamed title"`); non-author page has neither. Full suite green

### Phase 4: Docs
- [x] **Task 6: Update the spec** (DONE)
  - `specs/disjoint.md`: `updated_at` added to the posts table in §3; new "Editing Posts & Thread Titles" bullet under D:
    author-only post edit (body + link) with `updated_at` / "(edited)"
    marker; author-only title edit; attachments/expiration/anonymity
    immutable; note the quote-staleness consequence (replaces the
    "posts are immutable" sentence in the quoting section)
  - Files: `specs/disjoint.md`

### Checkpoint: Complete
- [x] All acceptance criteria met; full suite green on fresh + pre-existing DB (`make build`, `go vet`, gofmt clean, `go test -count=1 ./...`)
- [ ] Manual pass in a browser: edit a post, edit a title, verify front
      page + thread page + HTMX-appended replies all render correctly
      (covered by the test suite, but worth one human click-through)
- [x] Ready for review

## Risks and Mitigations
| Risk | Impact | Mitigation |
|------|--------|------------|
| `user_id` in WHERE silently 404s a legitimate author race (e.g. concurrent account deletion) | Low | Acceptable — deletion cascades the post anyway; 404 is the honest answer |
| `post_card.html` changes affect the HTMX reply partial and front page | Med | `post_partial.html` only includes `post_card`; new fields are `{{if}}`-gated; existing tests (`TestAnonymousReplies`) are the regression net |
| sqlc type churn (`NullTime` on hot paths) | Low | `updated_at` is additive; only two selects touched |
| Stale quotes confusing readers | Low | Accepted (D6); permalink preserved; note in spec |
| 200-char title cap rejects an existing long title on edit | Low | Cap applies to new values only; pre-existing long titles can still be *viewed*, and a rename just needs to be ≤200 |

## Open questions
- None blocking. If in-place (HTMX swap of the post card) editing is ever
  wanted, the pieces are already in place: the `postPartialTmpl` and the
  `UpdatePost` query — only the form's `hx-post`/`hx-swap` wiring and a
  `SinglePostViewItem` build are needed.
