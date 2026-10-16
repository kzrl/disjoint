# Implementation Plan: Global Front Page (replaces boards)

**Status: Implemented (v10).** All 7 tasks done — see [todo.md](todo.md) "Task List: Global Front Page" for the completion record, including the Tasks 2–6 slice-consolidation note.

## Overview

Replace the board-based forum with a HackerNews/Slashdot-style global front page. The home page (`/`) lists **all threads across the whole site, sorted by created date (newest first)**. Each thread row shows author, created time, reply count, and a **preview of the most recent comment** (author + truncated snippet linking into the thread). No voting/points. Threads keep the existing TTL expiry (24h/3d/7d/30d/never chosen at creation), but **every new comment resets the deadline to last-comment + TTL**. Boards are **removed entirely**: `boards` and `user_board_reads` tables dropped, `threads.board_id` dropped, board pages/creation/read-state deleted, thread creation moves to a global `/threads/new`.

## Architecture Decisions

- **A. Keep `expires_at` as the live deadline; add `threads.ttl_seconds`.** Store the chosen TTL on the thread (NULL = never). On creation: `expires_at = now + ttl`. On every new post: `expires_at = now + ttl`. All existing expiry predicates (`expires_at IS NULL OR expires_at > CURRENT_TIMESTAMP`), the worker purger, and the thread-page 404 check work unchanged. The alternative (derive expiry from `updated_at + ttl`) would rewrite every expiry predicate and defeat the `idx_threads_expires_at` index.
- **B. One migration (`00010_front_page.sql`)**: add `ttl_seconds`, backfill it from existing `expires_at − created_at`, drop `threads.board_id`, `boards`, `user_board_reads`, and the board-related indexes. `-- +goose NO TRANSACTION` + `PRAGMA foreign_keys OFF/ON` (same pattern as 00003 — dropping `boards` while `threads.board_id` still references it would otherwise cascade-wipe threads). Down is best-effort (placeholder board row, no board data restored — accepted, mirrors 00003's best-effort down).
- **C. Front page shape:** single `ListFrontPageThreads` query with a correlated subquery for the last reply (`MAX(id)` excluding the thread's `MIN(id)` opening post, which by construction is the first post created in the same tx). Reply count = `post_count − 1` (existing convention). Snippet is raw body truncated in Go (~120 chars) and rendered as **escaped plain text** (not markdown) to avoid half-open markup. Pagination: `?page=N`, 30 per page.
- **D. Routes:** delete `/boards/new` (GET/POST), `/boards/{id}`, `/boards/{id}/threads/new` (GET/POST) — they 404. Add `GET/POST /threads/new`. Navbar: drop admin "+ New Board", add "+ New Thread" for any signed-in user.
- **E. Accepted regressions (consequences of removing boards):**
  - **Unread `NEW` badges disappear** — they were built on `user_board_reads`. Not replaced in this plan (a thread-level read table is a possible follow-up).
  - The **"Recent activity" ticker is removed** from the home page — it was board-based ("posted in {board}") and its information is now on each thread row.
  - The **"On the air" strip stays** — it's user-presence-based, board-independent.
- **F. No voting/points** — explicit non-goal.
- **G. Worker unchanged.** It purges `expires_at`-based rows and attachment files; the TTL-reset only makes rows expire later.

## Task List

### Phase 1 — Foundation
- [ ] Task 1: Migration `00010_front_page.sql` + schema snapshot
- [ ] Task 2: sqlc query rework

### Checkpoint: Foundation
- [ ] `go build ./...` green; fresh DB → v10, seeded v9 fixture → v10, `goose.Down` back to v9, re-up idempotent
- NOTE (2026-10-16, Task 1 done): the handler suite is **expected red** after this migration until Tasks 3–6 rework the board-dependent tests (4 tests fail: TestFirstRunSetupAndAuthFlow, TestThreadCreationAndHTMXReply, TestMentionRendering, TestExpiringThreadsAndWorkerCleanup). Build stays green.

### Phase 2 — Core
- [ ] Task 3: Front-page handler + template + board removal
- [ ] Task 4: Global thread creation at `/threads/new`
- [ ] Task 5: Expiry reset on reply
- [ ] Task 6: Thread-page cleanup (board references gone)

### Checkpoint: Core
- [ ] End-to-end: create thread → comment → see it on front page with last-comment preview; old `/boards/*` URLs 404

### Phase 3 — Polish
- [ ] Task 7: Docs + final verification

### Checkpoint: Complete
- [ ] All acceptance criteria met; ready for review

## Tasks

### Task 1: Migration `00010_front_page.sql` + schema snapshot

**Description (DONE 2026-10-16):** New goose migration (NO TRANSACTION, `PRAGMA foreign_keys OFF/ON`). **Deviation from original design:** `ALTER TABLE threads DROP COLUMN board_id` is rejected by SQLite when the column appears in a table-level `FOREIGN KEY` clause (how 00006 declared it — verified empirically: inline FKs drop fine, table-level FKs fail the post-drop sanity check), so `threads` is **rebuilt** (00003 pattern): `threads_new` without `board_id`, with `ttl_seconds`, data copied with the backfill computed in the copy (`CASE WHEN expires_at IS NOT NULL THEN CAST((julianday(expires_at) − julianday(created_at)) * 86400 AS INTEGER) END`), old table dropped, renamed.
1. Rebuild `threads` (drops `board_id` + its FK, adds `ttl_seconds`, backfilled in the copy)
2. `DROP TABLE user_board_reads; DROP TABLE boards;`
3. Index swap: `idx_threads_updated_at` + `idx_threads_board_id` gone (with the old table), `CREATE INDEX idx_threads_created_at ON threads(created_at DESC);`; `idx_threads_expires_at` re-created (worker purge still uses it)

Verified: fresh DB → v10; seeded v9 fixture (2 threads [one 7-day-TTL, one never], 4 posts, attachment, board reads, 2 users) → v10 with all data intact, `ttl_seconds` = 604800/NULL, boards+reads gone, `PRAGMA foreign_key_check` clean; `goose.DownTo` v9 restores placeholder board + `board_id=1` + indexes, fk clean; re-up idempotent. Live `disjoint.db` copy (v9, 0 threads, 4 boards) → v10.

Update the cumulative `db/schema.sql` snapshot to match (sqlc schema source). Down: best-effort restore of `boards` (one placeholder "General" row), `threads.board_id INTEGER NOT NULL DEFAULT 1`, `user_board_reads`, drop `ttl_seconds`.

**Acceptance criteria:**
- [ ] Fresh DB boots to goose v10; all suites green
- [ ] Copy of live `disjoint.db` (v9) upgrades to v10: thread/post data intact, `ttl_seconds` backfilled sensibly for expired-at-set threads, boards gone, `PRAGMA foreign_key_check` clean
- [ ] `goose.DownTo` v9 on a copy: app still boots (placeholder board)

**Verification:** `go build ./...`; `go test ./...`; manual goose up/down on DB copies (never the live DB in-repo).

**Dependencies:** None (high-risk — first).

**Files likely touched:** `db/migrations/00010_front_page.sql` (new), `db/schema.sql`
**Estimated scope:** Small (2 files)

### Task 2: sqlc query rework

**Description:** In `db/query.sql`:
- **New** `ListFrontPageThreads :many` — global non-expired threads, `ORDER BY created_at DESC, id DESC`, `LIMIT ? OFFSET ?`, selecting: thread id/title/created_at/expires_at/ttl_seconds, `COALESCE(u.username,'') AS author_username` (anonymity-safe LEFT JOIN pattern), `COUNT(p.id) AS post_count`, and last-reply info via correlated subquery: `lc.id AS last_comment_id`, `lc.body AS last_comment_body`, `lc.created_at AS last_comment_at`, `COALESCE(lcu.username,'') AS last_comment_author` where `lc.id = (SELECT MAX(p2.id) FROM posts p2 WHERE p2.thread_id = t.id AND p2.id > (SELECT MIN(p3.id) FROM posts p3 WHERE p3.thread_id = t.id))` (opening post excluded → NULL for comment-less threads).
- **New** `BumpThreadExpiration :exec` — `UPDATE threads SET updated_at = CURRENT_TIMESTAMP, expires_at = ? WHERE id = ?` (supersedes `TouchThread`, which is only called from posts.go; remove `TouchThread`).
- **Change** `CreateThread` params: drop `BoardID`, add `TTLSeconds sql.NullInt64` (and keep `ExpiresAt`).
- **Change** `GetThreadByID`: drop the `boards` JOIN + `board_name`; add `t.ttl_seconds`.
- **Remove:** `ListBoards`, `GetBoardByID`, `CreateBoard`, `ListBoardsWithDetails`, `ListLatestThreadsByBoard`, `ListThreadsByBoard`, `UpsertUserBoardRead`, `GetUserBoardRead`, `ListUserBoardReads`, `ListRecentActivity` (ticker removed — its board JOIN no longer compiles anyway).

Run `sqlc generate`.

**Acceptance criteria:**
- [ ] `sqlc generate` clean; generated code compiles
- [ ] No generated query references `boards` or `user_board_reads`

**Verification:** `go build ./...`; `grep -ri "board" internal/database/` shows nothing.

**Dependencies:** Task 1.

**Files likely touched:** `db/query.sql`, `internal/database/*` (generated)
**Estimated scope:** Medium

### Task 3: Front-page handler + template + board removal

**Description:**
- Rewrite `IndexPage` (boards.go → rename file to `front.go` or keep): load `ListFrontPageThreads` with pagination (`?page=N`, 30/page), keep `ListOnlineUsers` ("On the air"), drop board details / read map / activity ticker. Build `FrontPageThreadItem` rows: `ReplyCount = PostCount − 1`, snippet = `strings.TrimSpace` of markdown → plain-text truncate to ~120 chars + `…`.
- Rewrite `web/templates/index.html` as the front page: rank number, title link, `by {author}` (anonName/userLink semantics), created relative time, `{n} comments`, last-comment line (`{author} · {time}: "{snippet}" →` linking `/threads/{id}#post-{lastCommentID}`), "No comments yet" when NULL; subtle dim `expires in …` tag when `expires_at` is set; prev/next pagination. Keep the on-air strip.
- Delete `BoardPage`, `NewBoardPage`, `CreateBoardSubmit` and their routes; delete `web/templates/board.html`, `new_board.html`; remove from template ParseFS lists in app.go.
- Navbar (layout.html): drop admin `+ New Board`; add `+ New Thread` link for signed-in users (placeholder route, implemented in Task 4).
- Remove unread-badge code paths (readMap in IndexPage, `IsUnread` fields).

**Acceptance criteria:**
- [ ] `/` renders global thread list sorted newest-first with last-comment previews; anonymous threads/posts show "Anonymous", never a real username (assert on full body)
- [ ] Expired threads absent; comment-less threads show "No comments yet"; pagination works
- [ ] `GET /boards/1`, `GET /boards/new` → 404; no `/boards` routes registered
- [ ] Board/new_board templates and handlers gone; `grep -ri board internal/ web/templates/` clean

**Verification:** `go test ./internal/handlers/` (new `TestFrontPage` in handlers_test.go: ordering, reply counts, snippet + anchor link, anonymity negative assertions, expired excluded, pagination boundary).

**Dependencies:** Task 2.

**Files likely touched:** `internal/handlers/boards.go` (rewrite/rename), `internal/handlers/app.go` (routes + templates), `web/templates/index.html`, `web/templates/layout.html`, `web/templates/board.html` (delete), `web/templates/new_board.html` (delete), `internal/handlers/handlers_test.go`
**Estimated scope:** M–L (largest task; keep snippet logic tiny)

### Task 4: Global thread creation at `/threads/new`

**Description:** Move thread creation off boards: routes `GET /threads/new` + `POST /threads/new`; `NewThreadPage`/`CreateThreadSubmit` lose board parsing/lookups; expiration select now drives **both** `TTLSeconds` and `ExpiresAt` (e.g. `7d` → 604800, `never` → NULL/NULL). Redirect targets become `/threads/new`. `new_thread.html`: drop board breadcrumb + "in {board}" heading, form action `/threads/new`, expiration help text updated to mention the comment-resets-deadline behavior. Wire the navbar link from Task 3.

**Acceptance criteria:**
- [ ] Signed-in user creates a thread from `/` → new thread appears first on the front page with correct ttl/expires_at (24h/3d/7d/30d/never matrix)
- [ ] Anonymous checkbox, attachments, and markdown all still work on the new form
- [ ] Unauthenticated POST redirects to `/login`; empty title/body re-renders with flash

**Verification:** `go test ./internal/handlers/` (extend `TestThreadCreation…`: route is `/threads/new`, DB has `ttl_seconds` + `expires_at` per option, `never` → both NULL).

**Dependencies:** Task 2. (Parallelizable with Task 3 after its routes stub, but sequence after to keep the app green each step.)

**Files likely touched:** `internal/handlers/threads.go`, `internal/handlers/app.go`, `web/templates/new_thread.html`, `internal/handlers/handlers_test.go`
**Estimated scope:** Medium

### Task 5: Expiry reset on reply

**Description:** In `CreatePostSubmit` (posts.go), replace `TouchThread` with `BumpThreadExpiration`: if the thread has a TTL, set `expires_at = now + ttl` (computed in Go, UTC); `never` threads keep `expires_at = NULL`. Runs inside the existing post-creation transaction. Drop the user-board-read upsert from this handler (table gone).

**Acceptance criteria:**
- [ ] New comment on a 24h thread created 20h ago pushes `expires_at` to ≈ now + 24h (thread survives; worker won't purge it)
- [ ] Comment on a `never` thread leaves `expires_at IS NULL`
- [ ] `updated_at` still advances (existing `idx`/ordering unaffected)
- [ ] Worker purges a thread with no comments after its TTL (regression)

**Verification:** `go test ./internal/handlers/` (new `TestExpiryResetOnComment`: HTMX + standard paths; assert DB `expires_at` before/after; worker `CleanupOnce` regression on a faked expired thread).

**Dependencies:** Task 5 needs Task 2 (query) + Task 1 (column).

**Files likely touched:** `internal/handlers/posts.go`, `internal/handlers/handlers_test.go`
**Estimated scope:** Small–Medium

### Task 6: Thread-page cleanup

**Description:** `ThreadPage` and `thread.html` lose board references: `GetThreadByID` no longer returns `board_name` (Task 2); breadcrumb becomes Home → Thread title; remove the board read-state upsert; drop `BoardID` usage anywhere in the reply form/JS (quote permalink uses only thread/post ids — unaffected). Check `users.html`/`user_card.html`/`invites.html` for stray board links and remove.

**Acceptance criteria:**
- [ ] Thread page renders with no board crumb; quote/mention/avatar features unchanged
- [ ] No template references `/boards/` or `BoardName`/`BoardID`

**Verification:** `go test ./...`; `grep -ri "board" web/templates/ internal/handlers/` clean.

**Dependencies:** Tasks 2, 3.

**Files likely touched:** `internal/handlers/threads.go`, `web/templates/thread.html`, possibly `web/templates/{users,user_card,invites}.html`
**Estimated scope:** Small

### Task 7: Docs + final verification

**Description:** Update `specs/disjoint.md` and `README.md`: replace the Boards/Topic-Based Organization bullets with the global front-page description (created-date ordering, last-comment previews, no voting); document the TTL-reset expiry semantics ("threads expire TTL after creation, and each new comment pushes the deadline to last-comment + TTL"); remove the Unread Indicators bullet (feature gone) or note its removal; update the route map + directory notes. Final end-to-end pass.

**Acceptance criteria:**
- [ ] README + spec accurately describe the front page, `/threads/new`, expiry reset, and list no board routes
- [ ] Full suite green with `-race`; `go vet` clean

**Verification:**
- [ ] `go build ./...` + `go test ./... -race`
- [ ] Fresh-DB boot (temp DB: goose 1→10) — `/` front page, create thread, reply, see reset expiry
- [ ] Pre-existing-DB boot (copy of live `disjoint.db`) — threads survive migration, front page renders real data
- [ ] Manual: front page in dark + light themes; expired thread disappears after worker tick

**Dependencies:** Tasks 3–6.

**Files likely touched:** `specs/disjoint.md`, `README.md`
**Estimated scope:** Small

## Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Migration drops `boards` while `threads.board_id` FK is live → cascade wipe of threads | High | NO TRANSACTION + `PRAGMA foreign_keys OFF` (00003 precedent); drop `board_id` **before** `boards`; verify on a seeded v9 copy incl. `PRAGMA foreign_key_check` |
| `ttl_seconds` backfill is approximate for threads that already had activity (stored `expires_at` was set at creation only) | Low | Accepted: backfill = original creation TTL; live behavior corrects itself on the next comment |
| Last-comment subquery cost (correlated, per row) | Low | Self-hosted scale; `idx_posts_thread_id` covers the inner lookups; front page caps at 30 rows |
| Unread badges silently disappear (user-visible regression) | Med | Documented in README/spec + this plan (Decision E); thread-level read tracking is a follow-up if wanted |
| Snippet of raw markdown leaks markup noise | Low | Plain-text truncation, HTML-escaped by template, not re-rendered as markdown |
| `MIN(id)` = opening post assumption breaks | Low | True by construction (opening post created in the same tx, ids autoincrement); pinned by a test asserting the preview is NULL for a single-post thread |
| Anonymous author leak via last-comment preview | High | Same `LEFT JOIN` + `COALESCE(u.username,'')` pattern as 00003; negative body assertions in `TestFrontPage`, same bar as `TestAnonymousInvisibleToAdmins` |

## Open Questions
- ~~Boards fate~~ — **RESOLVED: removed entirely** (tables, routes, pages, unread state).
- ~~"Recent comments"~~ — **RESOLVED: per-thread last-comment preview** (no separate ticker section).
- ~~Expiry reset~~ — **RESOLVED: `expires_at = now + TTL` on every new comment; `never` stays NULL.**
- Pagination size 30 — default choice; trivial to change if the user prefers another number.
- "Expires in" countdown tag on each front-page row — included as a subtle dim tag; drop it if unwanted.
