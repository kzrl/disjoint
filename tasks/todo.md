# Task List: Optional Anonymous Posting

Plan: [tasks/plan.md](plan.md)

- [x] Task 1: Adopt goose for DB migrations  (DONE)
  - Added `github.com/pressly/goose/v3` v3.28.0 dependency
  - Created `db/migrations/00001_init.sql` — single-file format (`-- +goose Up` = old DDL, `-- +goose Down` = drops). NOTE: v3.28 rejects the classic `.up.sql`/`.down.sql` pair (duplicate-version panic)
  - Kept `db/schema.sql` as the cumulative schema snapshot for sqlc; `db/db.go` now embeds `migrations/*.sql` → `db.MigrationsFS`; `sqlc.yaml` schema = `db/schema.sql`
  - `OpenDB`: replaced SchemaSQL exec with `goose.SetBaseFS` + `goose.SetDialect("sqlite")` + `goose.Up(conn, "migrations")`
  - Verified: fresh DB passes all tests; pre-existing `disjoint.db` upgrades to goose version 1; 3× `OpenDB` idempotent; boot smoke test OK; `sqlc generate` output unchanged

- [x] Task 2: Anonymous-posting schema migration + sqlc queries  (DONE)
  - `db/migrations/00002_posts_is_anonymous.sql` (single-file): Up = `ALTER TABLE posts ADD COLUMN is_anonymous INTEGER NOT NULL DEFAULT 0;`, Down = `DROP COLUMN` (verified working)
  - Added column to `db/schema.sql` snapshot; `sqlc.yaml` unchanged
  - Updated `db/query.sql`: `CreatePost` (new param); `ListPostsByThread`, `GetPostByID` (select `p.is_anonymous`); `GetThreadByID`, `ListThreadsByBoard`, `ListBoardsWithDetails`, `GetLatestThreadForBoard` (first-post subquery wrapped in `CAST(COALESCE(...,0) AS INTEGER)` so sqlc emits `int64`)
  - `sqlc generate`; wired `IsAnonymous: 0` in threads.go + posts.go
  - Verified: fresh DB → v2, upgraded copy → v2, `goose.Down` → v1 (col dropped); full suite green

- [x] Task 3: Anonymous thread creation end-to-end  (DONE)
  - "Post anonymously" checkbox added to `web/templates/new_thread.html` (after expiration, with hint)
  - `CreateThreadSubmit` reads `anonymous`, passes `IsAnonymous` to `CreatePost` for the opening post
  - "Started by" rendering in `thread.html` (starter line + post author) + `board.html` (thread list) honors the flag
  - `TestAnonymousThreadCreation` added: DB state (`is_anonymous=1`, `user_id` intact), thread + board render "Anonymous", attributed-thread regression — all pass

- [ ] Checkpoint: Foundation + thread creation
  - [ ] `go build ./...` and `go test ./...` pass on fresh and pre-existing DBs
  - [ ] goose versions correct (fresh=2, upgraded=2)
  - [ ] Anonymous thread flow verified end-to-end
  - [ ] Review with human before proceeding

- [x] Task 4: Anonymous replies (standard + HTMX)  (DONE)
  - Checkbox added to reply form in `thread.html` (shared by HTMX + standard paths; auto-resets on HTMX success via existing `hx-on::after-request`)
  - `CreatePostSubmit` reads `anonymous`, passes `IsAnonymous` to `CreatePost`
  - Post-author line in `post_partial.html` honors the flag (thread.html already handled in Task 3)
  - `TestAnonymousReplies` added: HTMX anon/attributed partials, standard anon reply re-render, per-post semantics (attributed reply in anon thread), DB sanity (3 anon posts, real user_id retained) — all pass

- [x] Task 5: Admin attribution annotation  (DONE)
  - `anonName` template func (app.go) centralizes: non-anon → username; anon+non-admin → `Anonymous`; anon+admin → `Anonymous (name)` with `.anon-attribution` dim styling
  - `SinglePostViewItem.CurrentUser` added so the HTMX partial can apply admin attribution; templates capture `{{$viewer := .CurrentUser}}` before `{{range}}` loops
  - `TestAdminAnonymousAttribution` added: admin thread page + board listing show real name; bystander thread page + board listing do NOT; admin HTMX partial shows own name, bystander's does not — all pass

- [ ] Checkpoint: Complete
  - [x] `go test ./...` passes (all 5 tasks: goose, migration, anon thread, anon replies, admin attribution)
  - [ ] Manual: create anonymous thread + reply; verify board, thread page, HTMX reply; verify admin vs non-admin rendering
  - [ ] Ready for review

---

# Task List: Unlink Anonymous Posts (admins can't see the author either)

Supersedes Task 5's admin-attribution behavior. Anonymous content is now stored with `user_id = NULL`, so no one — including admins and anyone with DB access — can identify the author.

- [x] Task 6: Migration `00003_anonymous_unlinked.sql`  (DONE)
  - Rebuilds `threads` + `posts` with nullable `user_id` (SQLite can't loosen NOT NULL in place); re-creates indexes
  - Severs links for existing anonymous content: `UPDATE posts SET user_id = NULL WHERE is_anonymous = 1` + threads whose first post is anonymous
  - **`-- +goose NO TRANSACTION` is required:** with `foreign_keys=ON` on the connection, `DROP TABLE threads` cascade-deletes every row in `posts` (and `DROP TABLE posts` would wipe `attachments`) before the copy runs. `PRAGMA foreign_keys` is a no-op inside a transaction, so the migration toggles `PRAGMA foreign_keys OFF/ON` and runs autocommit
  - Verified: fresh DB, seeded v2 DB with anonymous content (data preserved, links nulled, `PRAGMA foreign_key_check` clean), `goose.DownTo` v2 (data preserved, best-effort relink to user 1), and the real `disjoint.db` (2 anon posts + anon thread nulled, attributed rows untouched)
  - `db/schema.sql` snapshot updated; `sqlc generate`

- [x] Task 7: Queries + handlers store NULL for anonymous content  (DONE)
  - `db/query.sql`: `JOIN users` → `LEFT JOIN users` + `COALESCE(u.username, '')` in `ListPostsByThread`, `GetPostByID`, `GetThreadByID`, `ListThreadsByBoard`, `GetLatestThreadForBoard`
  - `CreateThreadSubmit` / `CreatePostSubmit` pass `sql.NullInt64{Int64: user.ID, Valid: isAnonymous == 0}`

- [x] Task 8: Remove admin attribution from all render sites  (DONE)
  - `anonName` (app.go) simplified to `(isAnon, name)`: anonymous → `Anonymous` for everyone; dropped `isAdmin` param + 4 template call sites; removed `.anon-attribution` CSS; removed `SinglePostViewItem.CurrentUser` (partial no longer needs the viewer)
  - **Fixed pre-existing leak:** `index.html` rendered `LatestThread.AuthorUsername` with no anonymity check — now routes through `anonName` (real name was visible on the homepage for anonymous threads)

- [x] Task 9: Tests  (DONE)
  - `TestAnonymousThreadCreation` / `TestAnonymousReplies`: DB assertions flipped — anonymous posts/threads now must have `user_id IS NULL` and empty `AuthorUsername`
  - `TestAdminAnonymousAttribution` → `TestAnonymousInvisibleToAdmins`: admin thread page, board listing, and HTMX partial must NOT contain the poster's username; DB-level assertions (thread + all posts `user_id IS NULL`)
  - New `TestIndexPageAnonymousLatestThread`: homepage shows `by Anonymous`, never the real username
  - Full suite green; app boots against pre-migration `disjoint.db`

- Trade-off (accepted, documented): with the link severed, abusive anonymous content cannot be attributed even via DB access. No out-of-band audit log is written (it would be visible to admins and defeat the purpose).
- Side effect: deleting a user no longer cascades to their anonymous posts/threads (NULL `user_id`) — desired.

---

# Task List: BBS Community Trio (user cards, on-the-air, activity ticker)

Plan: [tasks/plan.md](plan.md) (section "Implementation Plan: BBS Community Trio")

- [x] Task 10: Community schema migration + sqlc queries  (DONE)
  - `00004_community.sql`: `users.bio TEXT` (nullable) + `user_presence(user_id PK, last_seen_at)` table
  - Queries: `UpsertUserPresence` (30s SQL-throttled conditional upsert), `ListOnlineUsers` (5-min window), `GetUserPresence`, `CountPostsByUserID`, `UpdateUserBio`, `ListRecentActivity` (10, anonymity-safe JOIN pattern)
  - `sqlc generate`; fresh + real DB to v4

- [x] Task 11: Presence tracking middleware + "On the air" widget  (DONE)
  - `FirstRunAndAuth`: upsert presence after user identified (errors never block the request)
  - `IndexPage` loads `ListOnlineUsers`; `index.html` "On the air" strip (green dot, name, Xm ago; dimmed empty state)
  - Tests: throttle respected (<30s no-op), 5-min window boundary, no writes on anon/static paths, failing upsert doesn't break the response

- [ ] Checkpoint: Presence
  - [ ] `go build ./...` + tests green
  - [ ] Two sessions: see each other on the air; stale user hidden

- [x] Task 12: User card page + profile editing + author links  (DONE)
  - `GET /users/{name}` (404 on miss): bio (markdown, escaped), joined, last seen, attributed post count
  - `POST /users/me/profile` (self-only, ≤500 chars)
  - `userLink` template func (path-escaped); attributed authors linked in thread/board/homepage; anonymous never linked
  - Username validation: registration + setup accept only `^[A-Za-z0-9]{3,32}$` (ASCII alphanumeric); legacy users unaffected
  - Tests: card render, 404, bio round-trip + length cap, XSS-escaped bio, post count excludes anonymous, anonymity assertions

- [x] Task 13: Recent-activity ticker  (DONE)
  - `ListRecentActivity(ctx, 10)` in `IndexPage`; "Recent activity" section on homepage with thread links + relative times
  - Tests: newest-first cap-10, anonymous shows "Anonymous" (never username), expired threads excluded, empty state

- [x] Checkpoint: Complete
  - [x] `go test ./...` passes
  - [x] Manual: app booted against live `disjoint.db` (v3→v4); homepage shows on-air strip (kzrl, linked, "just now"), Recent activity with linked attributed author + "Anonymous" for unlinked posts; user card shows Admin badge, joined, last seen, attributed post count (1 of 3 — anon excluded), edit-profile form
  - [x] Ready for review
