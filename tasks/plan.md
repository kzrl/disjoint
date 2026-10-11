# Implementation Plan: Optional Anonymous Posting

## Overview
Let any logged-in user optionally post without their username being displayed. Anonymity is a **per-post** choice: the "Post anonymously" checkbox on the new-thread form applies to the thread's opening post (which also controls the "Started by" line on board listings and the thread page), and the checkbox on the reply form applies to that single reply. The real `user_id` is always stored in the database (no behavior/audit change); only the *display* is suppressed.

## Architecture Decisions
- **Per-post flag, not thread-level or pen names.** A single `posts.is_anonymous` boolean (default 0) is the smallest change that covers both creation and replies, and it lets a user sign some posts and not others in the same thread. Thread-level "started by" anonymity is derived from the first post's flag (smallest `id` post per thread) via a scalar subquery — no redundant thread column to keep in sync.
- **Display logic lives in templates, real username stays in queries.** Queries keep returning the true `author_username` and add the `is_anonymous` flag. Templates render `Anonymous` to regular users, and `Anonymous (realname)` to admins so moderators can attribute content. This keeps queries simple and makes the admin view a template-only concern.
- **Database migrations move to pressly/goose (v3.28).** The project previously had no migration system — `OpenDB` just executed the embedded `db/schema.sql` (all `IF NOT EXISTS`). We adopt `github.com/pressly/goose/v3` (v3.28.0). Two API realities shaped the design:
  - **goose v3.28 uses the single-file migration format, not the classic `.up.sql`/`.down.sql` pair.** Its FS collector globs `*.sql` and requires exactly one file per version; a `NNNNN_x.up.sql` + `NNNNN_x.down.sql` pair panics with `duplicate version N detected`. Each migration is one file `NNNNN_name.sql` containing `-- +goose Up` and `-- +goose Down` sections.
  - **sqlc cannot point at the migration files**, because it would execute the `-- +goose Down` `DROP` statements and end up with an empty schema. So we keep a **separate `db/schema.sql`** that is the cumulative "current schema" snapshot (the net effect of all `Up` sections). `sqlc.yaml` points at `db/schema.sql`. This is the standard sqlc+migrations pattern; the snapshot must be kept in sync when a migration is added (noted in the `Makefile`).
  - Layout: `db/migrations/00001_init.sql` (single file: Up = the old `schema.sql` DDL verbatim, idempotent via `IF NOT EXISTS`; Down = drop indexes then tables). `db/db.go` re-introduced as a tiny package that only `//go:embed migrations/*.sql` → `db.MigrationsFS` (embed cannot reach outside the package dir, so the migrations live under `db/`).
  - `OpenDB` (`internal/database/db_conn.go`) replaces the old `db.SchemaSQL` exec with `goose.SetBaseFS(db.MigrationsFS)` + `goose.SetDialect("sqlite")` + `goose.Up(conn, "migrations")`. (v3.28 exposes `Up`, not `ApplyMigrations`; `SetBaseFS` returns no error, `SetDialect` does.) goose manages its own `goose_db_version` table (columns `version_id`, `is_applied`).
  - **Existing-database upgrade path:** a pre-existing DB (created by the old code) has no `goose_db_version`, so goose applies `00001` (a no-op on the idempotent DDL), records version 1, then applies `00002` (the new column). Fresh databases get both. Verified idempotent — repeated `OpenDB` on the same file re-runs nothing.
  - Category/board seeding stays in Go (`db_conn.go`), unchanged — it's data, not schema.
  - New migrations are added by hand or via the `goose` CLI (see `Makefile` hint). **Caveat:** `SetBaseFS`/`SetDialect` are process-global; safe today because tests call `OpenDB` sequentially, but avoid concurrent `OpenDB` in parallel tests.
- **HTMX reply path shares the standard form.** The reply form in `thread.html` is both an HTMX form and a standard fallback (`hx-post` + `enctype="multipart/form-data"`), so one checkbox in that form serves both paths; `CreatePostSubmit` reads it the same way in both.
- **No retroactivity, no new routes.** Existing threads/posts are unaffected. No API surface changes beyond form fields. Existing posts default to `is_anonymous = 0` (attributed), which is correct.

## Task List

### Phase 1: Foundation
- [x] Task 1: Adopt goose for DB migrations
- [x] Task 2: Anonymous-posting schema migration + sqlc queries

### Checkpoint: Foundation
- [x] `go build ./...` and `go test ./...` pass on fresh and pre-existing DBs (behavior unchanged; flag defaults to 0)

### Phase 2: Anonymous thread creation (vertical slice)
- [x] Task 3: Anonymous thread creation end-to-end

### Checkpoint: Core feature
- [x] Anonymous thread shows "Anonymous" on thread page + board listing; attributed threads unchanged

### Phase 3: Anonymous replies
- [x] Task 4: Anonymous replies (standard + HTMX)

### Phase 4: Admin attribution
- [x] Task 5: Admin attribution annotation

### Checkpoint: Complete
- [x] All tests pass (automated: `TestAdminAnonymousAttribution` covers admin vs non-admin rendering on thread page, board listing, and HTMX partial)
- [ ] Manual smoke: `make run`, create an anonymous thread + reply as a regular user, view as admin (see `Anonymous (name)`) vs non-admin (see `Anonymous`)

## Task Details

### Task 1: Adopt goose for DB migrations  ✅ DONE
**Description:** Introduce goose v3 (v3.28.0) as the migration system. Add `github.com/pressly/goose/v3` to `go.mod`. Create `db/migrations/00001_init.sql` (single-file format: `-- +goose Up` = the old `schema.sql` DDL verbatim, `-- +goose Down` = drop indexes then tables). Keep `db/schema.sql` as the cumulative schema snapshot for sqlc; re-introduce `db/db.go` as a tiny package embedding `migrations/*.sql` → `db.MigrationsFS`. Point `sqlc.yaml` schema at `db/schema.sql`. In `internal/database/db_conn.go`, replace the `db.SchemaSQL` exec with `goose.SetBaseFS` + `goose.SetDialect("sqlite")` + `goose.Up(conn, "migrations")`. Verify a pre-existing DB upgrades cleanly and idempotency.

**Acceptance criteria:**
- [x] Fresh DB: `OpenDB` produces the same schema as before (all tests pass)
- [x] Pre-existing DB from old code: `OpenDB` succeeds, `goose_db_version` at version 1, app fully functional (verified on a copy and on the real `disjoint.db`)
- [x] `goose_db_version` is created and tracked; repeated `OpenDB` on the same file is a no-op (3 opens → identical state)
- [x] `sqlc generate` runs against `db/schema.sql` and generated code unchanged

**Verification:** `go build ./...`, `go vet ./...`, `gofmt -s -l` clean, `go test ./...` pass; manual boot smoke test against the real `disjoint.db` (server starts, `/`→303 `/login`, `/login`→200)
**Dependencies:** None
**Files:** `go.mod`/`go.sum`, `db/migrations/00001_init.sql` (new, single-file), `db/schema.sql` (kept as sqlc snapshot), `db/db.go` (now embeds migrations), `db/query.sql` (unchanged), `sqlc.yaml`, `internal/database/db_conn.go`, `Makefile` (hint comment)
**Scope:** M

**Implementation notes:** goose v3.28 rejects the classic `.up.sql`/`.down.sql` pair (duplicate-version panic) — single-file format required. `SetBaseFS`/`SetDialect` are global; keep `OpenDB` sequential in tests.

### Task 2: Anonymous-posting schema migration + sqlc queries  ✅ DONE
**Description:** Add `db/migrations/00002_posts_is_anonymous.sql` (single-file: Up = `ALTER TABLE posts ADD COLUMN is_anonymous INTEGER NOT NULL DEFAULT 0;`, Down = `ALTER TABLE posts DROP COLUMN is_anonymous;`). Add the column to the `db/schema.sql` snapshot. Update the sqlc queries: `CreatePost` (new param), `ListPostsByThread` / `GetPostByID` (select `p.is_anonymous`), and the thread-listing queries (`GetThreadByID`, `ListThreadsByBoard`, `ListBoardsWithDetails`, `GetLatestThreadForBoard`) with a first-post subquery. Run `sqlc generate`.

**Acceptance criteria:**
- [x] Fresh DB and a pre-existing (Task 1-upgraded) DB both end up with `posts.is_anonymous` (DEFAULT 0) at goose version 2
- [x] Generated Go models expose `IsAnonymous int64` on post rows and `FirstPostAnonymous int64` on the four thread rows
- [x] All existing handlers still compile (new params wired with `0` until Tasks 3–4); full test suite green

**Verification:** `go build ./...`, `go vet ./...`, `gofmt -s` clean, `go test ./...` pass; migration verified on a fresh DB (00001+00002 → v2) and a Task 1-upgraded copy (00002 only → v2); `goose.Down` verified to drop the column (v2 → v1)
**Dependencies:** Task 1
**Files:** `db/migrations/00002_posts_is_anonymous.sql` (new), `db/schema.sql` (snapshot updated), `db/query.sql`, `internal/database/query.sql.go` + `models.go` (generated), `internal/handlers/threads.go` + `posts.go` (compile-only wiring)
**Scope:** M

**Implementation notes:** sqlc cannot infer the type of `COALESCE((subquery), 0)` (emits `interface{}`) — wrapped as `CAST(COALESCE(... , 0) AS INTEGER)` so the generated fields are `int64`. In `ListBoardsWithDetails` (a GROUP BY query) the subquery is wrapped in `MAX(...)`. `ALTER TABLE ... DROP COLUMN` works on the bundled modernc SQLite (verified).

### Task 3: Anonymous thread creation end-to-end  ✅ DONE
**Description:** Add a "Post anonymously" checkbox to `web/templates/new_thread.html`. `CreateThreadSubmit` reads `anonymous` from the form and passes it to `CreatePost` for the opening post. Update `thread.html` ("Started by" line + post authors) and `board.html` ("Started by" in the thread list) to render `Anonymous` when the relevant `is_anonymous` flag is set.

**Acceptance criteria:**
- [x] Creating a thread with the checkbox shows "Anonymous" as starter on both the thread page and the board listing; without it, the username shows (regression)
- [x] The real username remains queryable in the DB (`posts.user_id` unchanged)

**Verified by:** `TestAnonymousThreadCreation` (handlers_test.go) — DB assertions (`is_anonymous=1`, `user_id` intact), thread page `Started by <strong>Anonymous</strong>` + `post-author">Anonymous`, board listing `Started by <strong>Anonymous</strong>`, and attributed-thread regression (`Started by <strong>anon_tester</strong>`). Full suite green.

**Verification:** `go test ./...` + new/extended handler test: create thread with anonymous flag → `ListThreadsByBoard`/`GetThreadByID` row has the flag set and rendered body contains "Anonymous"; without flag → username present
**Dependencies:** Task 2
**Files:** `web/templates/new_thread.html`, `web/templates/thread.html`, `web/templates/board.html`, `internal/handlers/threads.go`, `internal/handlers/handlers_test.go`
**Scope:** M

### Task 4: Anonymous replies (standard + HTMX)  ✅ DONE
**Description:** Add the checkbox to the reply form in `thread.html` (serves both HTMX and standard-submit paths). `CreatePostSubmit` reads `anonymous` and passes it to `CreatePost`. Update the post-author line in `thread.html` and `post_partial.html` to honor the flag.

**Acceptance criteria:**
- [x] Reply with checkbox → rendered post shows "Anonymous" (both HTMX partial response and standard redirect re-render)
- [x] Reply without checkbox → username shown (regression)
- [x] A non-anonymous reply in an anonymous thread still shows the replier's username (per-post semantics)

**Verified by:** `TestAnonymousReplies` (handlers_test.go) — (1) HTMX anonymous reply partial shows `post-author">Anonymous` and contains no username; (2) HTMX attributed reply partial shows username; (3) standard anonymous reply → redirect + thread page shows it while the attributed starter stays visible; (4) attributed reply inside an anonymous thread shows the replier while the anon starter stays "Anonymous"; (5) DB: exactly 3 anonymous posts, each retaining its real `user_id`. Full suite green.
**Dependencies:** Task 3
**Files:** `web/templates/thread.html`, `web/templates/post_partial.html`, `internal/handlers/posts.go`, `internal/handlers/handlers_test.go`
**Scope:** M

### Task 5: Admin attribution annotation  ✅ DONE
**Description:** In the four author render sites (thread "Started by", thread posts, board listing, post partial), when the post is anonymous *and* the viewer is an admin, show `Anonymous (realname)` with the real name styled subtly (dim/small). Non-admins always see plain `Anonymous`.

**Acceptance criteria:**
- [x] Admin viewing an anonymous post/thread sees the real username in the annotation
- [x] Non-admin viewing the same content never sees the username in the HTML body (asserted on response body in tests)

**Implementation:** A single `anonName` template func (app.go) centralizes the rule — non-anonymous → username; anonymous+non-admin → `Anonymous`; anonymous+admin → `Anonymous <span class="anon-attribution">(name)</span>`. `SinglePostViewItem` gained a `CurrentUser` field so the HTMX partial (no layout) can decide. Templates capture `{{$viewer := .CurrentUser}}` before `{{range}}` loops so the viewer isn't shadowed by the loop item. `.anon-attribution` styled dim/small in `style.css`.

**Verified by:** `TestAdminAnonymousAttribution` (handlers_test.go) — admin thread page + board listing contain the poster's real name in `.anon-attribution`; bystander (non-admin) thread page + board listing do NOT contain it; admin HTMX partial shows own name, bystander HTMX partial does not. Full suite green.
**Dependencies:** Task 3
**Files:** `internal/handlers/app.go` (`anonName`), `internal/handlers/threads.go` (`SinglePostViewItem.CurrentUser`), `internal/handlers/posts.go` (set viewer), `web/templates/thread.html`, `web/templates/board.html`, `web/templates/post_partial.html`, `web/static/style.css`, `internal/handlers/handlers_test.go`
**Scope:** S

## Risks and Mitigations
| Risk | Impact | Mitigation |
|------|--------|------------|
| goose on a pre-existing DB without `goose_db_version` | Med | 00001 is idempotent (`IF NOT EXISTS`), so applying it to an initialized DB is a no-op; verify with the real `disjoint.db` in Task 1 |
| sqlc parsing migration files (comments, multi-statement files) | Low | Classic `.up.sql`/`.down.sql` format; `-- +goose` annotation lines are plain SQL comments; verify generated output is unchanged in Task 1 |
| `ALTER TABLE ADD COLUMN` on a populated table | Low | `NOT NULL DEFAULT 0` is safe on SQLite (no table rewrite); up-only down is acceptable |
| Subquery on first post costs on board listings | Low | SQLite, small forum scale; `posts.id` is PK; can add index later if needed |
| Anonymous flag leaks via admin annotation XSS | Low | Real name flows through `html/template` auto-escaping (existing pattern) |
| sqlc regen churn in `query.sql.go` | Low | Regenerate in Tasks 1–2 only; don't hand-edit generated files |
| HTMX partial missing flag in template data | Med | `GetPostByID` row gains the flag in Task 2; partial template updated in Task 4 with a regression test on the HTMX path |

## Open Questions
- **Should admins see real names at all?** Plan says yes (subtle annotation) for moderation value. Alternative: hide from everyone, admins included (stronger privacy, weaker moderation). — *defaulting to "yes" unless you object*
- **Anonymous identity correlation:** the DB still links anonymous posts to `user_id`, so admins (with DB access) can correlate. True anonymity (unlinked posts) would require a schema change (nullable `user_id` or pen-name records) and is a much larger project — out of scope here.
- **Should anonymity apply to attachments only visible to the poster?** N/A — attachments are public to all logged-in users today; unchanged.
- ~~**Down-migration for 00002:** `ALTER TABLE ... DROP COLUMN` requires SQLite ≥ 3.35~~ — **RESOLVED (Task 2):** `goose.Down` verified working on the bundled modernc SQLite; the down migration is real, not a no-op.

---

# Addendum: Unlink Anonymous Posts (Tasks 6–9, see tasks/todo.md)

**Decision (supersedes the "admin annotation" answer in Open Questions):** anonymity is now real — anonymous threads/posts are stored with `user_id = NULL`, so no one (admins included, even via direct DB access) can identify the author.

Key design points:
- **Table rebuild, not `ALTER`:** SQLite cannot loosen a `NOT NULL` constraint in place; `00003` rebuilds `threads` and `posts`.
- **`-- +goose NO TRANSACTION` + `PRAGMA foreign_keys OFF/ON`:** with `foreign_keys=ON` (set on the app connection), `DROP TABLE threads` cascade-deletes all child rows in `posts` before the copy runs — and `PRAGMA foreign_keys` is a no-op *inside* a transaction, so goose's default transaction wrapping would make an in-migration pragma ineffective. The migration therefore runs autocommit; a mid-migration failure is not rolled back (accepted for a one-shot destructive migration).
- **Display simplification:** `anonName(isAnon, name)` — anonymous renders as `Anonymous` for everyone; the admin branch, `.anon-attribution` CSS, and `SinglePostViewItem.CurrentUser` are removed.
- **Latent bug fixed:** `index.html` showed `LatestThread.AuthorUsername` with no anonymity check (real name leaked on the homepage for anonymous threads before this change).
- **Accepted trade-off:** abusive anonymous content can no longer be attributed, even out-of-band. No audit log is written at creation time (it would be admin-visible and defeat the goal).
- **Side effect:** user deletion no longer cascades to anonymous content (NULL `user_id`) — desired.
- **Downgrade is best-effort:** the severed link is unrecoverable; NULL authors are re-linked to user 1 as a placeholder so the restored `NOT NULL` holds.

---

# Implementation Plan: BBS Community Trio (user cards, on-the-air presence, activity ticker)

## Overview
Three small, complementary features that answer a visitor's first three questions — *who are these people? who's here? is anything happening?* — to recreate the old-school BBS community feel. All are read-mostly, HTMX-free, and fit the existing Go/SQLite/sqlc/`html/template` stack. Anonymous-content rules carry through: nothing in this batch may expose the author of an anonymous post (NULL `user_id`), and user-card post counts therefore count attributed posts only.

## Architecture Decisions
- **Presence = `user_presence(user_id PK, last_seen_at)` table**, not session-based (sessions are stateless HMAC tokens). Updated by the auth middleware via a *single conditional upsert* throttled in SQL (`ON CONFLICT DO UPDATE ... WHERE last_seen_at < datetime('now','-30 seconds')`) — no per-request reads, no Go-side rate state, no worker needed. "On the air" = `last_seen_at` within 5 minutes.
- **User card = one page, not a subsystem.** `bio` is a single nullable column on `users` (simple `ALTER TABLE ADD COLUMN`, no rebuild — unlike 00003). Card shows: username, joined, last seen, attributed post count, markdown bio. Profile editing is a form on the same page, self-only (`POST /users/me/profile`).
- **Post counts exclude anonymous posts by construction** — `COUNT(*) WHERE user_id = ?` never matches NULL. This is the correct semantics given the unlink design (anonymous authorship is unknowable, so it can't be credited).
- **Activity ticker = one query, one homepage section.** `ListRecentActivity` joins posts→threads→boards with the established `LEFT JOIN users` + `COALESCE(u.username,'')` + `is_anonymous` pattern (identical to the anonymity-safe pattern from 00003), rendered through `anonName`. No per-board variant in this batch.
- **Author links via a `userLink` template func** that builds `/users/<path-escaped username>` anchors. Anonymous authors are never linked.
- **Username charset is ASCII alphanumeric** (`^[A-Za-z0-9]{3,32}$`, decided 2026-10-11): registration and first-run setup reject anything else. Existing users with legacy usernames keep working (links still path-escape); only new registrations are constrained. This makes `/users/{name}` URLs unambiguous.
- **No new routes beyond `GET /users/{name}` and `POST /users/me/profile`.** No realtime; presence freshness (≤30s write, 5min window) is appropriate for an async forum.

## Task List

### Phase 1: Foundation
- [x] Task 10: Community schema migration + sqlc queries

### Checkpoint: Foundation
- [x] Fresh + pre-existing DBs migrate to v4; `go build ./...` clean

### Phase 2: On the air
- [x] Task 11: Presence tracking middleware + "On the air" homepage widget

### Checkpoint: Presence
- [x] Authenticated requests populate presence; homepage shows live users only; stale users hidden

### Phase 3: User cards
- [x] Task 12: User card page + profile editing + author links

### Phase 4: Motion
- [x] Task 13: Recent-activity ticker on the homepage

### Checkpoint: Complete
- [x] Full suite green; live-DB smoke (v3→v4, on-air + ticker + card verified on the real `disjoint.db`); ready for review

## Tasks

### Task 10: Community schema migration + sqlc queries
**Description:** Add `db/migrations/00004_community.sql`: `ALTER TABLE users ADD COLUMN bio TEXT;` plus `CREATE TABLE user_presence (user_id INTEGER PRIMARY KEY, last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE);` (Down: drop table, `DROP COLUMN bio` — verified-safe pattern from 00002). Update `db/schema.sql` snapshot. Add to `db/query.sql`: `UpsertUserPresence :exec` (conditional 30s-throttled upsert, see decision above), `ListOnlineUsers :many` (5-minute window, joined username, ordered most-recent-first), `GetUserPresence :one` (for the card's "last seen"), `CountPostsByUserID :one`, `UpdateUserBio :exec`, `ListRecentActivity :many` (params: `limit int64`; selects `p.id, p.created_at, p.is_anonymous, COALESCE(u.username,'') AS author_username, t.id AS thread_id, t.title, t.board_id`, filters expired threads, `ORDER BY p.created_at DESC, p.id DESC LIMIT ?`). Run `sqlc generate`.
**Acceptance criteria:**
- [ ] Fresh DB and the real `disjoint.db` both reach goose version 4 with data intact
- [ ] Generated Go compiles (`go build ./...`) with `UserPresence`, `ListOnlineUsersRow`, `ListRecentActivityRow`, etc.
- [ ] `UpsertUserPresence` is a single statement whose second call within 30s is a no-op (verifiable in a unit test later via SQL only — no app wiring yet)
**Verification:** `go build ./...`; manual: boot app against copy of `disjoint.db`, confirm version 4 + `PRAGMA foreign_key_check` clean
**Dependencies:** None
**Files:** `db/migrations/00004_community.sql` (new), `db/schema.sql`, `db/query.sql`, `internal/database/*` (generated)
**Scope:** M

### Task 11: Presence tracking middleware + "On the air" widget
**Description:** In `middleware.FirstRunAndAuth`, after the user is successfully identified (step 2, `currentUser != nil`), call `UpsertUserPresence` and ignore errors (presence must never block or fail a request). In `IndexPage` (boards.go), load `ListOnlineUsers` and pass as `OnlineUsers`. In `index.html`, render a compact "On the air" strip above the board index: green dot + username (plain text for now; links land in Task 12) + "· Xm ago", or a dimmed "No one else is on the air" line when empty (exclude the viewer themselves? **No** — BBS on-screen lists include yourself; show everyone).
**Acceptance criteria:**
- [ ] Any authenticated request within 30s of a previous one does not update `last_seen_at` (DB assertion in test)
- [ ] Homepage lists a user who has a presence row < 5 min old; does not list one at 6 min (test inserts rows directly)
- [ ] Unauthenticated / static / setup paths never write presence
- [ ] A failing upsert does not alter the response (e.g., inject error → request still 200)
**Verification:** `go test ./internal/middleware/ ./internal/handlers/` (new middleware test file if none exists)
**Dependencies:** Task 10
**Files:** `internal/middleware/middleware.go`, `internal/handlers/boards.go`, `web/templates/index.html`, `internal/middleware/middleware_test.go` (new) + handlers test additions
**Scope:** M

### Task 12: User card page + profile editing + author links
**Description:** Also tighten username validation (decided): `RegisterSubmit` and the first-run setup handler accept only `^[A-Za-z0-9]{3,32}$` (ASCII alphanumeric; existing length bounds folded into the regex) with a clear flash message. New `internal/handlers/profiles.go`: `UserCardPage` (`GET /users/{name}` → `GetUserByUsername`, 404 on miss; loads bio, `GetUserPresence` (absence → "never"), `CountPostsByUserID`) and `UpdateProfileSubmit` (`POST /users/me/profile`, self-only by construction; trims bio, rejects > 500 chars with flash, redirects to `/users/{name}`). New template `user_card.html` (add to the `pages` list in app.go): username heading, dimmed meta line (joined `formatTime` · last seen `formatRelative` · N attributed posts), markdown-rendered bio, and an "Edit profile" textarea form visible only when `CurrentUser.ID == card user`. Add a `userLink(name string)` template func (path-escaped anchor; empty name → plain text) and use it for attributed authors in `thread.html` (starter line + post authors), `board.html` (starter), and `index.html` (latest-thread author + on-air strip). Anonymous authors stay unlinked plain text.
**Acceptance criteria:**
- [ ] `GET /users/alice` renders bio, join date, post count; unknown user → 404
- [ ] Bio edit round-trips (form → POST → redirect → updated page); > 500 chars rejected with flash; only self can edit (route is `/users/me/profile`)
- [ ] Post count excludes anonymous posts (DB: 2 attributed + 1 anonymous → shows 2)
- [ ] `<script>` in a bio renders escaped (no raw script tag in response)
- [ ] Attributed authors on thread/board/homepage pages are links to their card; anonymous author is never linked and shows "Anonymous"
- [ ] Registration/setup reject usernames with non-ASCII-alphanumeric characters (e.g., `a/b`, `a_b`, `a b` → flash error); existing users unaffected
**Verification:** `go test ./internal/handlers/`
**Dependencies:** Task 10, Task 11 (links the on-air strip)
**Files:** `internal/handlers/profiles.go` (new), `internal/handlers/app.go` (routes + template list + `userLink` func), `web/templates/user_card.html` (new), `web/templates/thread.html`, `web/templates/board.html`, `web/templates/index.html`, `internal/handlers/handlers_test.go`
**Scope:** M–L (7 files but mostly 1–3 line link edits; the substance is one handler + one template)

### Task 13: Recent-activity ticker
**Description:** In `IndexPage`, load `ListRecentActivity(ctx, 10)` and pass as `Activity`. In `index.html`, render a "Recent activity" section (below the board index): one line per entry — `authorLink-or-anonName author posted in "thread title" (board link) · Xm ago`, thread title linking to the thread. Anonymous entries render `Anonymous` with no link and no username (existing `anonName` + `userLink` semantics).
**Acceptance criteria:**
- [ ] Ticker shows the 10 most recent posts across all boards, newest first, with working thread links
- [ ] Anonymous post entries contain the word "Anonymous" and NOT the real username (assert on full page body)
- [ ] Posts in expired threads are absent
- [ ] Empty forum (no posts) renders a dimmed "No activity yet" line, not an empty section
**Verification:** `go test ./internal/handlers/`
**Dependencies:** Task 10, Task 12 (reuses `userLink`)
**Files:** `internal/handlers/boards.go`, `web/templates/index.html`, `internal/handlers/handlers_test.go`
**Scope:** M

## Risks and Mitigations
| Risk | Impact | Mitigation |
|------|--------|------------|
| Per-request presence writes amplify SQLite writes | Med | 30s SQL-side throttle (single conditional upsert, no reads); WAL mode already on; forum-scale user counts |
| Legacy usernames (pre-existing) may still contain non-alphanumeric chars | Low | `userLink` path-escapes so legacy names remain linkable; new registrations constrained to `[A-Za-z0-9]{3,32}` |
| Bio XSS | High | Existing safe markdown renderer + `html/template` auto-escaping; explicit `<script>`-in-bio test |
| `ListRecentActivity` full-scan `ORDER BY created_at` (no index) | Low | Acceptable at self-hosted forum scale; add `idx_posts_created_at` later if it shows up in profiling |
| Presence middleware error path masks a broken query for every request | Low | Errors logged (or dropped deliberately, matching the app's existing `_ =` style); query is trivial and covered by tests |
| Ticker/card leak anonymous authors | High | Reuses the proven 00003 pattern (`LEFT JOIN` + `COALESCE` + `is_anonymous`); negative assertions in tests, same as `TestAnonymousInvisibleToAdmins` |

## Open Questions
- ~~**Admin badge on user cards?**~~ — **RESOLVED (2026-10-11): yes**, subtle dim badge.
- ~~**On-air window & placement**~~ — **RESOLVED: 5-minute window, homepage-only**, list includes the viewer themselves. Header widget deferred.
- ~~**Username charset**~~ — **RESOLVED: ASCII alphanumeric only** (`[A-Za-z0-9]{3,32}`) for new registrations (Task 12).
- ~~**Ticker verb**~~ — **RESOLVED: "posted in"** for all entries.
