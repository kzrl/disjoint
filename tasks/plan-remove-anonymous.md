# Plan: Remove Anonymous Posts Entirely

Strip the anonymous-posting feature from Disjoint: every new post and thread is
attributed to the signed-in user, **all existing anonymous content is deleted**
(threads, posts, attachment rows, and the files on disk), and `user_id` on
`posts`/`threads` is re-tightened to `NOT NULL`.

## Context

The anonymous feature was implemented in two stages:

1. **00002** added `posts.is_anonymous`.
2. **00003** made `posts.user_id` / `threads.user_id` nullable and **severed**
   the author link for anonymous content (`user_id = NULL`, explicitly
   unrestorable).

Since 00003 the `is_anonymous` flag is redundant (anonymous ⇔ `user_id IS
NULL`), and removing the feature is a clean deletion of the flag, two form
fields, several query subqueries, and four tests — plus a destructive data
cleanup the user has now requested.

**User decisions (2026-07-06):**
- Delete all existing anonymous threads/posts (not just stop creating them).
- `user_id` must be `NOT NULL` again on `posts` and `threads`.

## Decisions

1. **One migration (`00006`) does the whole schema story** — destructive row
   deletion + table rebuilds in a single `NO TRANSACTION` migration (same
   pattern as 00003):
   - delete anonymous content rows (attachments → posts → threads),
   - rebuild `threads` with `user_id INTEGER NOT NULL`,
   - rebuild `posts` with `user_id INTEGER NOT NULL` **and** no
     `is_anonymous` column (rebuilding makes a separate `DROP COLUMN`
     unnecessary).
   Keeping 00002/00003 as history; goose only runs unapplied versions.
2. **Files on disk are deleted by a one-off Go tool, run BEFORE the
   migration** — SQL can't touch the filesystem, and once the attachment rows
   are deleted the `storage_path` → file mapping is gone. New
   `tools/cleanup_anonymous/main.go` (kept in the repo so other instances can
   run it too): opens the DB, lists the files belonging to anonymous content,
   shows the list, and with `--yes` deletes them from `UPLOAD_DIR`.
   Documented order: **run tool → then start the app** (app startup runs the
   migration). If the order is reversed, the tool finds no rows and the files
   become orphans (noted as a risk; the tool also warns if the migration is
   already applied).
3. **`authorLink` becomes link-only.** With `NOT NULL user_id` the username
   can never be empty, so the `"Anonymous"` branch disappears; the helper stays
   (5 call sites) and just renders the user-card link.
4. **Down migration is best-effort** (data is destroyed, mirroring 00003):
   re-add nullable `user_id` + `is_anonymous INTEGER NOT NULL DEFAULT 0`.

## Current touch-point inventory

| Layer | Where |
|---|---|
| Migrations | `00002_posts_is_anonymous.sql`, `00003_anonymous_unlinked.sql` — **keep as history**; add `00006_drop_and_purge_anonymous.sql` |
| Schema snapshot | `db/schema.sql:49,61,63` — `user_id` comments + `is_anonymous` line |
| Queries | `db/query.sql`: `CreatePost` param+column (`:200`), `p.is_anonymous` in `ListPostsByThread`/`GetPostByID` (`:172,187,322`), 4× `first_post_anonymous` subquery (`:69,84,102,124`), stale comment `:306` |
| Generated | `internal/database/query.sql.go`, `models.go` (regenerate via `make generate`) |
| Handlers | `threads.go:73-75,108-110,128` (form read, `NullInt64` author, param), `posts.go:59-61,72-74,79` (same), `profiles.go:35` (comment only) |
| Template funcs | `app.go:110-118` `authorLink` (2-arg, has Anonymous branch) |
| Templates | `new_thread.html:47-51` + `thread.html:110-114` (checkboxes); 5 `authorLink` call sites (`board.html:51`, `index.html:48,74`, `post_partial.html:3`, `thread.html:18,40`) |
| Tests | `handlers_test.go`: `TestAnonymousThreadCreation` (`:681`), `TestAnonymousReplies` (`:804`), `TestIndexPageAnonymousLatestThread` (`:987`), `TestAnonymousInvisibleToAdmins` (`:1041`); ~128 lines mention anon |
| Env/config | `UPLOAD_DIR` env (default `uploads`, `cmd/disjoint/main.go:30`) — needed by the cleanup tool; goose runs from embedded FS in `internal/database/db_conn.go:42` |
| Docs | README / specs: no mention of the feature — nothing to update. `tasks/plan.md` / `tasks/todo.md` are the historical plan for the feature; leave as-is |

FK cascades (from `schema.sql`): `attachments → posts`, `posts → threads`,
`threads.user_id → users`, all `ON DELETE CASCADE`.

## Phases

### Slice 1 — Schema, queries, code (one atomic, always-buildable commit)

- [ ] Task 1: `db/migrations/00006_drop_and_purge_anonymous.sql`
  (`+goose NO TRANSACTION`, `PRAGMA foreign_keys = OFF` during rebuild):
  ```sql
  -- 1. Purge anonymous content, top-down.
  DELETE FROM attachments WHERE post_id IN (
      SELECT id FROM posts
      WHERE user_id IS NULL
         OR thread_id IN (SELECT id FROM threads WHERE user_id IS NULL));
  DELETE FROM posts WHERE user_id IS NULL
      OR thread_id IN (SELECT id FROM threads WHERE user_id IS NULL);
  DELETE FROM threads WHERE user_id IS NULL;

  -- 2. Rebuild threads: user_id NOT NULL.
  CREATE TABLE threads_new (... user_id INTEGER NOT NULL ...);
  INSERT INTO threads_new SELECT ... FROM threads;
  DROP TABLE threads; ALTER TABLE threads_new RENAME TO threads;
  -- (recreate idx_threads_board_id / _updated_at / _expires_at)

  -- 3. Rebuild posts: user_id NOT NULL, no is_anonymous.
  CREATE TABLE posts_new (... user_id INTEGER NOT NULL, no is_anonymous ...);
  INSERT INTO posts_new SELECT id, thread_id, user_id, body, created_at FROM posts;
  DROP TABLE posts; ALTER TABLE posts_new RENAME TO posts;
  -- (recreate idx_posts_thread_id)

  PRAGMA foreign_keys = ON;
  ```
  Down: re-add `user_id` nullable (rebuild) + `is_anonymous` column; comment
  that purged data is unrecoverable.
- [ ] Task 2: `db/schema.sql` — drop the `is_anonymous` line; `user_id`
  `NOT NULL` with comment "author (every post/thread is attributed)".
- [ ] Task 3: `db/query.sql` — remove `CreatePost`'s `is_anonymous`
  param+column; remove `p.is_anonymous` from `ListPostsByThread` /
  `GetPostByID`; remove the 4 `first_post_anonymous` subqueries; reword the
  `:306` comment (no more anonymous posts).
- [ ] Task 4: `threads.go` `CreateThreadSubmit` — delete the `anonymous` form
  read, `isAnonymous`, `NullInt64` author; pass `UserID: user.ID`.
- [ ] Task 5: `posts.go` `CreatePostSubmit` — same.
- [ ] Task 6: `app.go` `authorLink` — one-arg, link-only; update comment.
- [ ] Task 7: Templates — remove both anonymous checkboxes
  (`new_thread.html`, `thread.html`); update the 5 `authorLink` call sites.
- [ ] Task 8: `profiles.go` — refresh the stale comment.
- [ ] Task 9: `make generate` (sqlc); review diff in `internal/database/`.

**Checkpoint A**
- [ ] `go build ./...` + `go vet ./...` pass.
- [ ] Migration verified on a **copy** of the dev `disjoint.db` before
      touching the real one: rows purged, tables rebuilt, `PRAGMA
      table_info(posts)` shows `user_id` NOT NULL and no `is_anonymous`.

### Slice 2 — File cleanup tool

- [ ] Task 10: `tools/cleanup_anonymous/main.go` (~100 lines, stdlib only +
      same sqlite driver):
  - Flags: `-db` (default from `DISJOINT_DB`/same default as the app),
    `-dir` (default `UPLOAD_DIR` or `uploads`), `--yes` (without it: dry-run
    that prints the file list and exits 0).
  - Query: `SELECT a.storage_path FROM attachments a JOIN posts p ON
    a.post_id = p.id WHERE p.user_id IS NULL OR
    p.thread_id IN (SELECT id FROM threads WHERE user_id IS NULL)`.
  - Warn (and refuse to delete) if the goose version table shows 00006 already
    applied — rows are gone, paths unrecoverable.
  - Delete only files whose full path stays inside `-dir` (no path traversal);
    print each deletion and a total.
- [ ] Task 11: Run it on the dev DB: dry-run first (review list), then
      `--yes`. Verify files gone from `uploads/`.

**Checkpoint B**
- [ ] `go build ./...` still green; tool dry-run on the dev DB shows the
      expected legacy-anonymous files; after `--yes`, `uploads/` no longer
      contains them.

### Slice 3 — Tests, migration on real DB, final verification

- [ ] Task 12: Delete the 4 anon tests; grep `handlers_test.go` for any other
      `anon`/`Anonymous` assertions and fix stragglers.
- [ ] Task 13: Back up the real `disjoint.db` (`cp disjoint.db
      disjoint.db.bak-pre00006`), then boot the app once — migration 00006
      applies; legacy anonymous threads/posts are gone from the UI.
- [ ] Task 14: Full verification:
  - [ ] `go build ./...`, `go vet ./...`, `go test ./...` all green.
  - [ ] Fresh-DB boot (`DISJOINT_DB=/tmp/fresh.db`) — migrations 1→6 apply
        with nothing to purge.
  - [ ] Live smoke: create thread + reply (no checkboxes), authors attributed,
        index/board/thread pages render.
  - [ ] `grep -ri anon web/templates db/ internal/` returns nothing except
        the two historical migrations and this plan's docs.

**Final**
- [ ] Commits: (1) slice 1 — feature removal + migration, (2) slice 2 —
      cleanup tool (+ the dev-DB run is data, not code), (3) slice 3 — test
      cleanup. (DB backup file stays untracked/git-ignored.)
- [ ] Leave `tasks/plan.md` / `tasks/todo.md` untouched (historical record).

## Risks / unknowns

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| **Data loss** — purged posts/threads/files are unrecoverable | Certain (intended) | User content gone | DB backup in Task 13 before boot; tool dry-run in Task 11; migration tested on a copy first (Checkpoint A) |
| Wrong tool/app ordering (migration runs before file cleanup) | Medium | Orphaned files in `uploads/` | Tool warns when 00006 already applied; document "tool first, app second" in the migration header + tool README comment |
| Rebuild migration fails mid-way (NO TRANSACTION, no rollback) | Low | Broken DB | Identical pattern to 00003 which ran in production; test on a copy first; backup exists |
| A legacy anonymous post is the *only* post in an attributed thread, or vice versa | Certain | Correctness of purge set | The purge predicate (post NULL-author OR thread NULL-author) covers both; verify counts on the dev copy before/after |
| `sqlc` regen diff touches more than expected | Low | Review noise | Diff review in Task 9 |
| Test posts `anonymous=1` outside the 4 named tests | Low | Test failure | Task 12 grep catches stragglers |

## Explicitly out of scope

- Restoring or archiving purged content (deletion is intentional and
  irreversible by design).
- Changing the expiration worker, avatars, invites, or any other behavior.
- Docs: nothing in README/specs describes the feature; the migration header and
  tool comments carry the operational notes.
