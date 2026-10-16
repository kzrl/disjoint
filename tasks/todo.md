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

---

# Task List: Quoting in Replies + @user Mentions

Plan: [tasks/plan-quotes-mentions.md](plan-quotes-mentions.md)

Approach: mentions = stateless goldmark inline parser (link-all valid tokens, code-aware); quoting = client-side Markdown blockquote insertion (no migration). See Open Questions Q1–Q3 in the plan.

### Phase 1 — @user mentions

- [x] Task 1: goldmark mention inline parser + renderer + unit tests  (DONE)
  - New `internal/markdown/mention.go`: goldmark `InlineParser` (trigger `@`) + `Mention` node (unique `ast.NewNodeKind`) + `renderer.NodeRenderer`; registered via a `mentionExt` extension added to the shared `md` instance in `markdown.go`
  - Left boundary implemented as `block.PrecendingCharacter()` not a word rune (letter/digit/`_`) — excludes `user@gmail.com`, `my_bot@host`, `a@kzrl`; allows line-head, after space/punct, and inside emphasis (`**@kzrl**` — first attempt with an allowlist of {start, space, `(`} wrongly rejected the `*` and was fixed)
  - Extra guard: `pc.IsInLinkLabel()` → `[@kzrl](/x)` stays a plain link (no nested `<a>`)
  - Render `<a class="mention" href="/users/{name}" data-mention="@{name}">@{name}</a>`; name is `[A-Za-z0-9]{3,32}` by construction so href is scheme/path/quote-free
  - 31 unit tests green: basics (line head, mid-line, EOL, two-per-line, emphasis, paren, punctuation stop), rejected (email, underscore email, glued, code span, fence, escaped `\@`, len 2/33, `@---`), length boundaries 3/32, XSS (script/slash/quote/colon), block contexts (blockquote, heading, table cell), link-label guard, code-awareness (exactly 1 of 2)
  - Side fix: `go mod tidy` corrected pre-existing `// indirect` markers on directly-imported deps (goldmark, goose, crypto, sqlite, uuid) — no version changes
  - **Scope:** Medium (2 new files + 1-line edit) · **Deps:** none
  - **Verify:** `go test ./internal/markdown/ -race` green; full suite green

- [x] Task 2: `.mention` styling + integration verification  (DONE)
  - `.mention` rule in `site.css`: tinted pill — `color: var(--bulma-primary)` on `background-color: color-mix(in srgb, var(--bulma-primary) 15%, transparent)` (hover: 25%), `border-radius: var(--bulma-radius-small)`, `padding: 0.05em 0.3em`. **BUG FIX (reported by user: white-on-white):** first attempt used `--bulma-primary-bold`/`--bulma-primary-light` — those ladder lightness steps live ONLY in Bulma's light base `:root` (verified: the dark theme block overrides zero ladder vars), so in the app's default dark theme the pill background resolved to lightness 91% (near-white). Now only `--bulma-primary` is used — its lightness IS overridden per theme (56% dark / 41% light) — plus a transparent color-mix tint; older browsers fall back to a tintless link. Do-not-repeat note is written into the CSS comment; selector is bare `.mention` (class only ever emitted by the markdown renderer) so it also works in bios/headings/table cells
  - `make css` regenerated `web/static/disjoint.css` (verified `bulma.min.css` unchanged — BULMA_DIR checkout matches vendored copy); assets are `go:embed`-ed, so the rule goes live on the next app restart (the instance running on :8080 from 21:39 still carries the old bundle — not restarted)
  - New `TestMentionRendering` in handlers_test.go, through real routes: thread body `@alice` (real) + `@nobody` (unknown) + `user@gmail.com` + `` `@bob` `` → thread page has exactly 2 mention anchors (alice, nobody; documents link-all), no mention for the e-mail, code span preserved; HTMX reply partial renders `@bob` mention; `/users/alice` target 200; `/users/nobody` 404
  - Side fix: `gofmt -w internal/handlers/app.go` (pre-existing field-alignment drift from the authLimiter addition)
  - **Scope:** Small-Medium (3 files) · **Deps:** Task 1
  - **Verify:** full suite green with `-race`; route-level checks cover the manual items (link to card, 404, code/email not linked). Visual pill check in both themes is the one item still needing a restarted instance

### Checkpoint: Mentions
- [x] `go build ./...` + `go test ./...` green
- [x] Manual: `@realuser` → card; `@nobody` → 404 link; `@x` in code span/fence and `user@gmail.com` NOT linked — fully subsumed by route-level assertions in `TestMentionRendering` (real user 200, unknown 404, e-mail/code span unlinked) plus `TestMentionRejected`/`TestMentionCodeAwareness` in the markdown package
- [ ] Review with human before proceeding

### Phase 2 — Quoting

- [x] Task 3: Quote button on post cards  (DONE)
  - `<button type="button" class="button is-small is-light quote-btn">Quote</button>` in a `has-text-right mt-4` row at the bottom of `.post-content-area` — pure Bulma classes, no new CSS
  - `data-thread-id="{{.Thread.ID}}"` on `#posts-container`
  - **Refactor beyond stated scope (justified):** the post-card markup was duplicated verbatim in `thread.html` + `post_partial.html`; extracted it to a shared `{{define "post_card"}}` in new `web/templates/post_card.html`, parsed into both template sets (app.go ParseFS ×2). Single source of truth so the card (and any future per-post UI) can't drift between the full page and HTMX partials. `post_partial.html` is now a one-line `{{template "post_card" .}}`
  - Verification in `TestMentionRendering`: exactly 1 quote button on the thread page (1 post), exactly 1 in the HTMX partial, `data-thread-id` present on the container
  - **Files:** `web/templates/post_card.html` (new), `web/templates/thread.html`, `web/templates/post_partial.html`, `internal/handlers/app.go` (2 ParseFS lines)
  - **Verify:** full suite green with `-race`; templates parse; button present per card incl. partial

- [x] Task 4: Quote-insertion JS + format regression test  (DONE)
  - Inline `<script>` at the bottom of `thread.html` content: one delegated `click` listener on `#posts-container` (works on HTMX-appended cards); on `.quote-btn`: author from `.post-author` textContent, post ID from the card `id` (`post-{id}`), thread ID from the container's `data-thread-id`, body from `.content` **innerText** (layout-aware: block boundaries + `<br>` → newlines; attachments live outside `.content` so they're excluded by construction)
  - Line handling: trim each line, drop leading/trailing blanks, empty body → single empty line (guards against a raw blank line splitting the blockquote); every line `> `-prefixed, blank lines as bare `>`; permalink line `> — [view original](/threads/{tid}#post-{pid})`
  - Insertion: append after existing textarea content (never clobber), sep normalized to a blank line; focus + caret to end + smooth scroll-into-view. Comment notes: move to a static file if a CSP `script-src` is ever added
  - Contract test `TestQuoteBlockquoteFormat` (new `internal/markdown/quote_test.go`), 4 cases pinned through `renderMarkdown`: two-line quote (plan example) → 1 blockquote with `<strong>…wrote:</strong>` + permalink anchor; multi-paragraph (bare `>` lines) → still 1 blockquote (first quoted line glues to attribution via hard-wrap `<br>` — assertion written to the actual shape); empty quote → 1 blockquote; quoted line starting `>` → outer+nested blockquotes, valid, permalink intact
  - Extra verification: extracted the JS construction and ran it under `node` — output byte-identical to the Go test inputs for both shapes; `node --check` passes on the full script
  - **Files:** `web/templates/thread.html` (script), `internal/markdown/quote_test.go` (new)
  - **Verify:** full suite green with `-race`; JS↔Go format contract byte-verified. Browser-level manual items (click → submit HTMX/standard → anchor jump) are the quoting checkpoint

- [x] Task 5: Quoting edge cases  (DONE)
  - Soft cap in JS (decision Q3): `MAX_QUOTE_CHARS = 500` on the joined quoted text; over → `slice(0, 500)` + trailing-whitespace strip + `\u2026`, then re-split for `> ` prefixing. Attribution + permalink lines are fixed overhead, always kept. Cutting mid-line → partial last line with `…`; cutting on a line boundary → `…` on the previous line, no dangling empty line
  - Attachment exclusion: by construction (JS reads only `.content`; `.post-attachments` is a sibling) — now pinned by a structural assertion in `TestThreadCreationAndHTMXReply`: the `.content` div closes before `.post-attachments` starts in the post card
  - Multi-line/empty-line blockquote integrity: already covered by Task 4's `TestQuoteBlockquoteFormat` (bare `>` lines, one blockquote) and the empty-body guard
  - Node-verified the cap logic: exactly 500 untouched; 501 → 500 chars + `…`; 300/300 two-line (601) → [300, 200] ending in `…`; every line of the capped quote still `>`-prefixed (stays one blockquote); `node --check` passes
  - **Files:** `web/templates/thread.html` (JS cap), `internal/handlers/handlers_test.go` (structural assertion)
  - **Verify:** full suite green with `-race`; cap logic node-verified. Browser-level manual item (quote a multi-paragraph + attachment post) is the quoting checkpoint

### Checkpoint: Quoting
- [x] `go build ./...` + `go test ./...` green
- [ ] Manual: full quote flow on a live thread, HTMX + standard submit, dark + light themes — rendering side verified (format contract tests + JS↔Go byte-identical check + `node --check`); the literal button click in a browser is the one unperformed step (no browser in this environment; app instance on :8080 still runs the pre-feature build)
- [ ] Review with human before proceeding

### Phase 3 — Polish

- [x] Task 6: Docs + final end-to-end verification  (DONE)
  - `specs/disjoint.md` §D: expanded Markdown Support bullet (GFM, hard wraps, raw-HTML neutralized, bios share the pipeline); new **@User Mentions** bullet (token shape, left-boundary rule, stateless render/404-link-all, code/link-label/email exclusions, alnum-by-construction href safety); new **Quoting Posts** bullet (delegated script, exact blockquote shape, one-blockquote invariant, 500-char soft cap, attachment exclusion, no schema change, immutability ⇒ quotes can't go stale)
  - `README.md` Features: Markdown bullet unchanged; added **@User Mentions** and **Quoting Posts** bullets
  - Final pass, all green: `go build` + full suite `-race`; **fresh-DB boot** (temp DB: goose 1→8, `/` → 303 `/setup`, served CSS contains `.mention` + `color-mix` rule); **pre-existing-DB boot** (copy of live disjoint.db: "no migrations to run, current version: 8", `/` → 303 `/login`, `/login` 200, new CSS served); both themes' computed values verified (dark: primary 56% + 15% tint; light: 41% + 15% tint)
  - **Files:** `specs/disjoint.md`, `README.md`
  - **Verify:** suite + build green; fresh + pre-existing DB boots verified; remaining open boxes are the human review + literal in-browser click

### Checkpoint: Complete
- [x] All acceptance criteria met (plan Overview + Decisions: both features implemented per confirmed decisions A/A/soft-cap; scope exclusions honored — no notifications, no autocomplete, no edit/delete)
- [x] Dark + light themes checked (computed per-theme values: mention pill = `--bulma-primary` 56%/41% text on 15% transparent tint in each theme; quote button is pure Bulma `is-small is-light` which the theme handles) — visual confirmation in a browser is folded into the manual checkbox above
- [x] Ready for review — all 6 tasks done, suite green with `-race`, fresh + pre-existing DB boots verified; the open boxes above are the human review steps and the one literal in-browser click

## Navbar → Bulma navbar component

- [x] Replace the hand-rolled `site-header`/`level` top bar with the Bulma navbar component, markup per `~/src/bulma/docs/documentation/components/navbar.html`  (DONE)
  - `layout.html` header → `<nav class="navbar has-shadow" role="navigation" aria-label="main navigation">`: `navbar-brand` (logo as `navbar-item logo` + `navbar-burger` with the four spans and `data-target="navMenu"`), `navbar-menu#navMenu` → `navbar-start` (New Board / Invites / Users as plain `navbar-item` links when authed) + `navbar-end` (user `tag`, Sign Out form in a `navbar-item`, or Sign In as a `navbar-item` when logged out)
  - Burger toggle: the docs' vanilla JS example verbatim at the bottom of the layout (Bulma ships no JS), plus one line syncing `aria-expanded` which the docs example omits but its own markup declares
  - Separation edge: `has-shadow` modifier (1px in `--bulma-background`) replaces the old `border-bottom`; navbar background comes from `--bulma-scheme-main` so it themes automatically
  - `site.css`: removed the `.site-header` background/border rule, the `.site-header .level { flex-wrap }` rule, and `.site-header .container` from the gutter selector (navbar is full-width and sizes itself); regenerated `disjoint.css` via `make css`
  - Note: `level is-mobile` remains in 4 other templates (user_card, thread, invites, board) — only the header-scoped CSS was removed
  - **Verify:** full suite green with `-race`; `node --check` on the burger script; live boot (fresh DB) — `/setup` serves all navbar classes (`navbar has-shadow`, brand, burger with `data-target`, menu/start/end, `aria-label="main navigation"`) and the toggle script

## Board creation → admin-only

- [x] Restrict board creation to admins  (DONE)
  - `requireAdmin` (already in invites.go) now guards `NewBoardPage` (GET /boards/new) and `CreateBoardSubmit` (POST /boards/new) in boards.go — non-admins get the "Unauthorized" flash + 303 to `/`, and no board row is created
  - Navbar: `+ New Board` moved into the `IsAdmin` block alongside Invites/Users in `navbar-start` (layout.html)
  - Empty-index CTA `+ Create First Board` (index.html) also gated by `IsAdmin` (first-run user is auto-admin, so the normal flow is unaffected)
  - Tests in `TestBoardCreation`: regular user denied GET + POST (redirect `/`, no board created), promoted admin succeeds; navbar link present for admin, absent for regular user on `/`
  - `specs/disjoint.md` §B: documented that board creation is admin-only
  - **Verify:** full suite green with `-race`, `go vet` clean

---

# Task List: Global Front Page (replaces boards)

Plan: [tasks/plan-front-page.md](plan-front-page.md)

Resolved decisions: boards **removed entirely** (tables + routes + unread state); front page = global thread list sorted by created date with **per-thread last-comment preview**; expiry = stored TTL with `expires_at = now + TTL` on every new comment (never = NULL). No voting (non-goal).

### Phase 1 — Foundation

- [x] Task 1: Migration `00010_front_page.sql` + schema snapshot  (DONE)
  - `threads.ttl_seconds INTEGER` (NULL = never) + backfill from `expires_at − created_at` (computed in the copy)
  - **Deviation:** `threads` is **rebuilt** (00003 pattern) instead of `ALTER TABLE DROP COLUMN` — SQLite's DROP COLUMN refuses columns declared in a table-level `FOREIGN KEY` clause (00006's style; verified: inline FKs drop fine, table-level FKs fail). Rebuild drops `board_id` + FK, adds `ttl_seconds`
  - Dropped `boards`, `user_board_reads`; `idx_threads_updated_at`/`idx_threads_board_id` gone, added `idx_threads_created_at` (kept `idx_threads_expires_at` for the worker)
  - NO TRANSACTION + `PRAGMA foreign_keys OFF/ON`; down is best-effort (placeholder "General" board, `board_id=1`)
  - `db/schema.sql` snapshot updated (sqlc regen deferred to Task 2 — query.sql still references boards)
  - Verified: fresh → v10; seeded v9 fixture (2 threads / 4 posts / attachment / reads) → v10 data intact, ttl = 604800/NULL, `PRAGMA foreign_key_check` clean; `goose.DownTo` v9 → re-up idempotent; live `disjoint.db` copy v9 → v10; `go build ./...` green

- [x] Task 2: sqlc query rework  (DONE)
  - New `ListFrontPageThreads` (created_at DESC, LIMIT/OFFSET, post_count, last-reply via correlated subquery excluding the opening post, anonymity-safe LEFT JOINs)
  - New `BumpThreadExpiration` (replaces `TouchThread`); `CreateThread` gains `TtlSeconds` / loses `BoardID`; `GetThreadByID` loses board join, gains `ttl_seconds`
  - Removed all board/board-read/`ListRecentActivity` queries; `sqlc generate` clean — zero board references in `internal/database/`

### Checkpoint: Foundation
- [x] `go build ./...` green; goose v10 fresh + upgraded (+ down/re-up round trip)
- [x] Red window closed: all 4 previously-red handler tests reworked and green (Tasks 2–6 landed as one atomic slice — see note below)
- [x] No generated query references boards

> **Slice consolidation note:** the sqlc regeneration (Task 2) changed the generated
> `CreateThreadParams`/`GetThreadByIDRow` shapes and deleted the board queries, so the
> handlers, templates, and test package could not compile until de-boarding happened.
> Tasks 2–6 therefore landed together in one atomic slice rather than five
> separately-green commits. All acceptance criteria below were verified in that slice.

### Phase 2 — Core

- [x] Task 3: Front-page handler + template + board removal  (DONE)
  - `IndexPage` (`internal/handlers/front.go`) → global list (30/page, `?page=N`), on-air strip kept, boards/activity/NEW badges gone
  - `index.html` rewritten: rank, title, author, created, reply count, last-comment preview (author · time · escaped-plain-text snippet → `#post-{id}` anchor), "No comments yet", "expires in" tag
  - `boards.go` deleted (BoardPage/NewBoardPage/CreateBoardSubmit); `/boards/*` routes gone (404); `board.html` + `new_board.html` deleted; navbar "+ New Board" → "+ New Thread" (all signed-in users)
  - Obsolete tests removed: TestUnreadIndicators, TestBoardCreation, TestRecentActivityTicker, TestActivityTickerEmptyState (features gone); remaining tests reworked to `/threads/new`
  - Smoke-verified live: empty state, thread row with rank/author/expires tag/reply count, snippet + `#post-2` anchor

- [x] Task 4: Global thread creation at `/threads/new`  (DONE)
  - Routes + handler rewritten (no board); expiration select drives `ttl_seconds` + `expires_at` (never → both NULL)
  - `new_thread.html` de-boarded; help text now mentions comment-resets-deadline; navbar link wired
  - Existing TTL/attachment/markdown/unauth tests reworked and green (TestThreadCreationAndHTMXReply, TestMentionRendering, TestAttachmentTypeEnforcementAndServing, TestUnauthorizedHTMXRequest, …)

- [x] Task 5: Expiry reset on reply  (DONE)
  - `CreatePostSubmit`: `BumpThreadExpiration` (`expires_at = now + ttl`, NULL for never); board-read upsert dropped
  - New regression test `TestReplyBumpsExpiration`: initial deadline = created+ttl, reply moves deadline to reply+ttl, never-thread stays NULL
  - Worker purge regression unchanged (TestExpiringThreadsAndWorkerCleanup); live smoke: deadline advanced `00:45:43 → 00:45:58` (+24h) after a reply

- [x] Task 6: Thread-page cleanup  (DONE)
  - Breadcrumb: Home → Thread became Front Page → Thread; `BoardID`/`BoardName` gone from `thread.html`
  - `grep -ri board web/templates/ internal/` clean (only remaining hits: reused `board-*` CSS row classes + one code comment)

### Checkpoint: Core
- [x] Create thread → comment → front page shows it with last-comment preview (live smoke); `/boards/*` → 404
- [x] `go build ./...` + `go test ./...` green (race run pending at Phase 3)

### Phase 3 — Polish

- [x] Task 7: Docs + final verification  (DONE)
  - `README.md`: intro rewritten (global front page, no boards), new front-page feature bullet, ephemerality bullet now states the comment-resets-deadline rule, Unread Indicators bullet removed
  - `specs/disjoint.md`: schema section de-boarded (no `boards`/`user_board_reads`; `threads` shows `ttl_seconds` + the live-deadline comment; indexes match v10), §4B rewritten (front page row contents + global `/threads/new`), §4C documents the TTL-reset-on-activity rule, §4D notes the deadline bump on reply, §4F now "Unread Indicators — Removed" with rationale, directory-structure comment updated
  - `go test ./... -race` green
  - Fresh-DB boot: goose 1→10 in one boot, first-run `/setup` flow works, front page renders
  - Pre-existing-DB boot: faithful copy of live `disjoint.db` (v9, user + 4 boards, WAL included) → migrated v9→v10, user preserved, `boards`/`user_board_reads` gone, front page 200
  - Themes: dark-first CSS served with the `prefers-color-scheme: light` override; front page uses only theme-aware Bulma/site classes (no JS theme toggle exists — verified by inspection)
  - Note: `/boards/*` and any other unknown GET render the front page with 200 — the pre-existing `GET /` tree-root catch-all at HEAD (soft-404 design), not a board regression

### Checkpoint: Complete
- [x] All acceptance criteria met
- [x] Manual: front page, thread creation, reply resets expiry, worker purge
- [ ] Review with human
