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

# Task List: Slack-Style Reactions

Plan: [tasks/plan-reactions.md](plan-reactions.md)

- [x] Task 1: Migrations + queries + sqlc  (DONE)
  - `db/migrations/00011_emojis_and_reactions.sql` (goose single-file, plain additive DDL): `emojis` table (name UNIQUE, source unicode/custom, glyph, storage_path, mime_type, file_size, created_by FK users ON DELETE SET NULL) + `reactions` table (UNIQUE (post_id, user_id, emoji_id), all FKs ON DELETE CASCADE) + 12 seeded built-in unicode rows (joy, excited, tada, clap, heart, eyes, thinking, thumbsup, thumbsdown, fire, rocket, party)
  - `db/schema.sql` snapshot updated to match (sqlc input)
  - `db/query.sql`: `AddReaction :execrows` (INSERT OR IGNORE — rowsAffected 0 = toggle-off), `RemoveReaction :execrows`, `ListReactionsByThreadID :many` (JOINs emojis + posts, COALESCE'd emoji columns), `ListAllEmojis :many`, `GetEmojiByID :one`, `GetEmojiByName :one`, `CreateEmoji :one`, `DeleteEmoji :exec`
  - `sqlc generate`; `go build ./...` clean
  - Verified: fresh DB → goose v11 with 12 seeded emoji rows; `goose down` (via temp helper, goose CLI not in go.sum) → v10 with both tables dropped; pre-existing dev `disjoint.db` backed up to `disjoint.db.bak-pre00011` and upgraded v10→v11 (posts intact); `go test ./...` green
- [x] Task 2: Custom emoji upload + management page  (DONE)
  - `internal/handlers/emojis.go`: `EmojiPage` (auth-gated grid), `CreateEmojiSubmit` (multipart `file` + `name`; Slack-style name regex `^[a-z0-9_]{2,64}$`; duplicate-name check via `GetEmojiByName`; sniff + stdlib `image.Decode` validation), `DeleteEmojiSubmit` (owner or admin; built-ins undeletable; removes file, reactions cascade via FK)
  - `saveEmojiFile`: 256KB cap, 256×256px max, PNG/GIF only — WEBP dropped vs plan (not sniffable by `http.DetectContentType`, no stdlib decoder; zero-dep policy)
  - `db/query.sql`: `ListAllEmojis` now joins uploader username (`created_by_username`) instead of raw ID; `sqlc generate`
  - Routes `GET /emojis`, `POST /emojis`, `POST /emojis/{id}/delete`; "Emoji" navbar link for signed-in users; `web/templates/emojis.html` (Bulma, upload form + tile grid, delete button for owner/admin)
  - `web/css/site.css`: `.emojis-grid`, `.emoji-tile`, `.emoji-glyph`, `.emoji-thumb` (`.emoji-thumb` is the pinned class Task 4 chips reuse); vendored CSS regenerated via `make css`
  - `handlers_test.go` `TestCustomEmojiUploads` (11 sections): guest redirect, 12 built-in tiles, PNG + GIF happy paths (row/file/render/attribution), duplicate name, invalid name, non-image, oversized dims, oversized file, delete permissions (stranger/built-in/owner), admin delete, reaction cascade on emoji delete — all pass; full suite green
- [x] Task 3: Reaction toggle handler + route  (DONE)
  - `db/query.sql`: `ListReactionsByPostID :many` (per-post rows for the toggle response); `sqlc generate`
  - `internal/handlers/reactions.go`: `ToggleReaction` — auth gate (HTMX 401 + `HX-Redirect` / non-HTMX 303 to login), post + emoji 404s, toggle via `AddReaction :execrows` (0 rows inserted → `RemoveReaction`), renders the re-grouped bar; `ReactionGroup` / `ReactionBarData` + `groupReactions` helper (shared by the thread page in Task 4)
  - `web/templates/reaction_bar.html` partial: chips (glyph span or `<img class="emoji-img">`, `is-active` when you reacted, count + title) + `+` button; `reactionBarTmpl` registered in `NewApp`
  - Route `POST /posts/{id}/reactions`
  - `handlers_test.go` `TestReactionToggles`: anonymous HTMX 401+redirect, anonymous 303, toggle-on (active chip, hx-vals, count, title), toggle-off, two-user count 2, custom emoji renders `<img>` chip, unknown post/emoji 404s — all pass; full suite green
- [x] Task 4: Reaction bar, picker, thread-page wiring, CSS  (DONE)
  - `reaction_bar.html`: chips + `+` trigger + server-rendered `.emoji-picker` grid (every emoji, glyph or `<img>`), whole bar re-rendered per toggle so counts/active state stay current
  - `ReactionBarData.Emojis` + `ListAllEmojis` in the toggle handler, `ThreadPage`, and the HTMX reply path; `PostViewItem`/`SinglePostViewItem` gain `Reactions` + `Emojis`; bar included once in `post_card.html` (covers thread page + HTMX partial)
  - `threads.go` `ThreadPage`: one `ListReactionsByThreadID` + one `ListAllEmojis` per page load, grouped via the shared `groupReactions`
  - `app.go`: `reactionBar` template func + `reaction_bar.html` parsed into every template set
  - `web/static/disjoint.js`: picker toggle via document-level click delegation (HTMX-appended bars work), outside-click + Escape close, aria-expanded sync
  - `web/css/site.css`: `.reaction-bar`, `.reaction-chip` (pill, `is-active` tint via color-mix like `.mention`), `.reaction-emoji`, `.emoji-img` (1.25em), `.reaction-picker-wrap`, `.emoji-picker` (8-col grid, scroll, z-30); vendored CSS regenerated via `make css`
  - `handlers_test.go` `TestThreadPageReactionBars`: per-user active chips (ally 2 / bob 1), grouped counts `joy (2)`, custom `<img>` chip, picker carries built-ins + customs, `hx-post` targets the post endpoint, HTMX reply appends a bar with a working picker — all pass; full suite green
  - Smoke: binary boots on the dev DB; login 200, guest /emojis + /threads 303; picker JS + chip CSS present in served static assets
- [x] Task 5: End-to-end verification + docs  (DONE)
  - `go test -race -count=1 ./...` green (fresh in-mem DBs per test)
  - Fresh-DB e2e smoke (temp instance, PORT=8091): /setup → create thread → upload a real 48×48 PNG custom emoji → toggle built-in joy on/off via `POST /posts/1/reactions` (HTMX) → react with the custom emoji (chip renders `<img>`) → HTMX reply appends a bar with a working picker → react on the reply → thread page shows chips + picker → delete the emoji: 2 reactions → 0, chip gone (FK cascade confirmed in SQLite)
  - Pre-existing-DB boot: dev DB at v11, goose "no migrations to run", login 200, picker JS served
  - README: "Slack-Style Reactions" feature bullet
  - specs/disjoint.md: `emojis` + `reactions` tables in the schema section, cascade note in the ephemerality section, reactions item in "Thread View & Interaction"
  - Dev DB backup `disjoint.db.bak-pre00011` still in repo root — delete when you're happy with the feature
- [x] Reactions on the index page  (DONE)
  - `db/query.sql`: `ListFrontPageThreads` gains `first_post_id` (MIN post id per thread, CAST to keep it int64); new `ListReactionsByPostIDs :many` (IN-list via `sqlc.slice`) fetches a whole page's opening-post reactions in one query; `sqlc generate`
  - `internal/handlers/front.go`: `FrontPageThreadItem` gains `Reactions` + `Emojis`; `IndexPage` loads reactions for the page's opening posts (skipped on an empty page — empty `IN ()` is a syntax error), groups via `groupReactions` with the viewer's id, and attaches the full emoji list per row
  - `web/templates/index.html`: the same `reaction_bar` partial under each row's title — chips toggle via the existing `POST /posts/{id}/reactions`, and the `+` picker works on the front page too
  - `web/css/site.css`: `.board-row .emoji-picker` opens downward (top-anchored) so it can't run off the viewport top on early rows; vendored CSS regenerated
  - `handlers_test.go` `TestFrontPageReactions`: per-row chips + grouped counts + custom `<img>` chip, active state per viewer (ally 2 / bob 1), pickers on every row, `hx-post` targets the opening post, toggle from the front page returns the swappable bar — all pass; `-race` suite green
  - Live smoke (fresh instance): react on an opening post, front page shows the active chip + pickers on both rows
  - README + specs/disjoint.md updated
- [x] First-class post link field (posts.url)  (DONE)
  - `db/migrations/00012_posts_url.sql`: `ALTER TABLE posts ADD COLUMN url TEXT` (NULL = no link); schema.sql snapshot updated
  - `db/query.sql`: `CreatePost` takes `url`; `ListPostsByThread` / `GetPostByID` return `COALESCE(p.url, '') AS url`; `sqlc generate` (field is `Url`, not `URL`)
  - `internal/handlers`: `cleanPostURL` (trim; empty ok; ≤2048; must parse with http/https scheme + host — the scheme check is the XSS boundary since templates would pass javascript: hrefs through); thread + reply creation accept `url`, and a link alone now satisfies the content requirement (body OR link OR attachment)
  - Templates: optional "Link" input in `new_thread.html` + the reply form; `post_card.html` renders the link card (`target="_blank" rel="noopener noreferrer"`, full URL in `title`) between body and reaction bar — covers thread page + HTMX partial
  - `web/css/site.css`: `.post-url` card (link color, ellipsis truncation); vendored CSS regenerated
  - `handlers_test.go` `TestPostURLField`: link-only thread, link-only reply, body+link, rejected `javascript:`/`ftp:`/no-scheme (thread → flash redirect, reply → 400, no rows created), escaped `&amp;` in href, verbatim round trip — all pass; `-race` suite green
  - Live smoke: link-only thread renders the card on a fresh instance; `javascript:` rejected; existing dev DB migrates 00012 cleanly
  - README + specs/disjoint.md updated

---

# Task List: Mentions & Replies Inbox

Plan: [tasks/plan-inbox.md](plan-inbox.md) — DONE (all v1 tasks; stretch task 8 open)

Model: one inbox row per (recipient, thread), not per event. Notified users are
derived from `markdown.Mentions` (the same goldmark AST the renderer uses), so
"notified" == "rendered pill" by construction. Explicit read: opening the
thread or pressing "Mark all read"; the inbox never clears itself. No
email/push.

- [x] Task 1: Migration `00015_notifications.sql` + schema snapshot + queries + sqlc  (DONE)
  - `notifications(user_id, thread_id, post_id, reason, last_event_at, read_at)`, PK `(user_id, thread_id)`, `CHECK reason IN ('mention','reply')`, all FKs `ON DELETE CASCADE`; schema snapshot + `sqlc generate`
  - Indexes: `idx_notifications_inbox (user_id, last_event_at DESC)`, partial `idx_notifications_unread (user_id) WHERE read_at IS NULL`
  - Queries: `UpsertNotification` (bump + `CASE` reason upgrade + `read_at = NULL`), `ListInbox` (expiry-filtered), `CountUnreadNotifications` (joins threads, expiry-filtered), `MarkAllNotificationsRead`, `MarkThreadNotificationRead`, `ListThreadRecipients`, `ListUserIDsByUsernames` (`sqlc.slice`)
  - Verified: fresh DB → goose 15; copy of dev `disjoint.db` (v14) → 15; `goose down` → 14 (table dropped) → up → 15; `PRAGMA foreign_key_check` clean

- [x] Task 2: `markdown.Mentions(source) []string`  (DONE)
  - AST walk for `KindMention`, first-appearance order, deduped
  - `TestMentionsMatchRenderedPills` pins the invariant against the `data-mention` attributes in `Render(body)` for 16 bodies (multi, dup, code span, fence, email, link label, emphasis, 2/33-char, table, blockquote); `TestMentionsOrderAndDedupe` covers the table explicitly

- [x] Task 3: Notifier + write path (`internal/handlers/notify.go`)  (DONE)
  - `notifyPostRecipients(ctx, qtx, …)`: mentions first (`reason=mention`), then `ListThreadRecipients` (`reason=reply`); actor excluded; `maxMentionNotifications = 20` cap applied before the IN query; empty name list skips the query (empty `IN ()` is a syntax error); recipients sorted for deterministic writes
  - Called inside the post tx in `CreatePostSubmit` and `CreateThreadSubmit` (no special case for new threads)
  - **Side fix:** `CreateThreadSubmit` declared `post, err :=` *inside* the `if err == nil` block, shadowing the outer `err` — a failed opening post or attachment would have committed a title-only thread. Now `var post database.Post` + `=`, so all three inserts share one error path
  - Tests: `TestInboxRecipientsAndReasonUpgrade` (actor excluded, participants, one row per thread, bump, sticky reason, reply→mention upgrade, re-raise), `TestInboxMentionExtraction` (self/unknown/email/code span), `TestInboxEditsDoNotNotify`, `TestInboxMentionCap` (25 mentions → 20 rows)

- [x] Task 4: `GET /inbox` + `POST /inbox/read` — handlers, routes, `inbox.html`  (DONE)
  - `InboxPage`: auth gate, `?page=` (30/page), `ListInbox`, snippet (URL fallback for link-only posts), empty state, pagination; **marks nothing read**
  - `MarkInboxReadSubmit`: `MarkAllNotificationsRead` + flash "Inbox cleared." + 303 `/inbox`; error path flashes and redirects without a success message
  - Header button rendered only when `.UnreadNotifications` > 0
  - `plainSnippet(string)` extracted from `commentSnippet`; exposed as the `snippet` template func
  - Reuses `.board-row` / `.unread-dot`; `<time>` with `isoTime`/`formatTime`/`formatRelative`
  - Tests: `TestInboxPageBadgeAndMarkAllRead` (guest GET+POST redirect, row content incl. `#post-` anchor, reload keeps highlight, mark-all clears + flash, re-raise), `TestInboxPagination` (30/1/empty at page 99), `TestInboxLinkOnlySnippetFallback`

- [x] Task 5: Navbar badge  (DONE)
  - `render()` injects `UnreadNotifications` via `CountUnreadNotifications`; omitted on error so chrome can never break a page
  - Bell in `navbar-end` (signed-in only), count in `aria-label`, visual pill `aria-hidden`, `99+` cap
  - CSS: `.inbox-link`, `.inbox-badge`, `.inbox-mark-all`, `.inbox-actor`, `.inbox-row(.is-unread)`, `.inbox-verb`, `.inbox-snippet`; `make css` regenerated
  - `TestInboxBadgeCapAndGuestBell`: guest pages carry no bell, 100 unread renders `99+` while `aria-label` keeps "100 unread"; badge presence/absence after mark-all and after a re-raise covered in the page test

- [x] Task 6: Thread read-through  (DONE)
  - `MarkThreadNotificationRead` beside `MarkThreadRead` in `ThreadPage` (best-effort)
  - `TestInboxReadThroughOnThreadVisit`: opening one thread clears only its entry; badge shows the remaining 1

- [x] Task 7: Docs + end-to-end smoke  (DONE)
  - `specs/disjoint.md` §D: new **Mentions & Replies Inbox** bullet (row model, recipients, extraction invariant, explicit read + read-through + mark-all, badge/expiry filter, cascades, no notify on edits); `README.md` feature bullet
  - Live smoke on a fresh temp instance (v15): `/setup` → invite → register bob → admin thread → bob replies `@admin` → admin inbox shows badge `1`, `is-unread`, "mentioned you in", "Mark all read" → reload unchanged → mark-all clears all three → new reply re-raises → opening the thread clears `read_at`; badge CSS served
  - `go test -race -count=1 ./...` green; `gofmt -s -l` + `go vet` clean

- [x] Task 8 (was the stretch item): badge liveness — implemented as part of the
      liveness pass below (`GET /inbox/count` + 60s poll, paused when hidden)

---

# Task List: Inline Emoji in Post Bodies

Plan: [tasks/plan-inline-emoji.md](plan-inline-emoji.md) — DONE

Rendering-only: bodies keep `:name:` verbatim. The parser resolves names
against a process-wide emoji snapshot and returns `nil` for unknown ones, so
literal fallback (typos, deleted emoji, code spans) is free. No migration, no
route, no per-render query.

- [x] Task 1: Cache + token grammar + renderer (`internal/markdown/emoji.go`)  (DONE)
  - `Emoji{Name, Glyph, StoragePath, Custom}`, `SetEmojis([]Emoji)` behind `atomic.Pointer[map[string]Emoji]`, `lookupEmoji`
  - `emojiParser`: trigger `:`, `pc.IsInLinkLabel()` guard, `isWordRune(PrecendingCharacter())` left boundary, `[a-z0-9_]{2,64}` + required closing colon, unknown → `nil` (token never consumed)
  - `emojiRenderer`: glyph for unicode; `<img class="emoji-img emoji-inline" src="/uploads/…" alt=":name:" loading="lazy">` for custom with hand-rolled `util.EscapeHTML` on the DB-sourced path and the name
  - `emojiExt` registered in `md` beside `&mentionExt{}`
  - `emoji_test.go`: 19-case render table (glyph, image, adjacency, emphasis, blockquote/heading/table, unknown, 1-char, 65-char, `::`, `:Joy:`, missing close, glued word, `10:30:00`, URL port, code span, fence, link label), escaping test (`<script>` in storage path), empty-cache literal test, `Mentions()` unaffected with both extensions loaded
  - `util.EscapeHTML` emits `&quot;` (not `&#34;`) — assertion corrected against the real output

- [x] Task 2: Refresh wiring  (DONE)
  - `app.refreshEmojiCache(ctx)` in `emojis.go`: `ListAllEmojis` → `markdown.SetEmojis`, best-effort
  - Called from `NewApp` (after template parsing) and the success paths of `CreateEmojiSubmit` / `DeleteEmojiSubmit`
  - Live: seeded `:joy:` renders on the dev-style DB; an upload is usable immediately, no restart

- [x] Task 3: Handler + template integration tests  (DONE)
  - `TestInlineEmojiInPostsAndBios`: real `POST /emojis` upload → thread body renders `🙂` + the exact `<img … alt=":shipit:">`; `:nope:` literal (no img); code span preserved; HTMX reply partial renders both; bio on the user card renders both; delete → thread page and bio fall back to literal `:shipit:` with the storage path gone everywhere (cache invalidation, not just parser skip)

- [x] Task 4: Docs + smoke  (DONE)
  - `specs/disjoint.md` §D **Inline Emoji** bullet; README feature bullet
  - Live smoke on a fresh temp instance: setup → upload `shipit` PNG → thread `glyph :joy: custom :shipit: unknown :nope: code \`:joy:\`` → glyph + img + literal + code observed → delete emoji → 0 img tags, `:shipit:` literal, storage path absent → bio `:joy:` renders; no errors in the app log

- [x] Checkpoint: Complete
  - [x] `go test -race -count=1 ./...` green; `gofmt -s -l` / `go vet` clean
  - [x] No CSS change needed (`.emoji-img` sizing from the reaction chips is correct inline)
  - [ ] Review with human

---

# Task List: Liveness Pass (badge polling + thread new-replies pill)

No new dependency, no websockets: two small polling endpoints on the existing
HTMX setup. Everything returns 200 with the element it replaces, so a swap can
never delete the poller.

- [x] Task 1: Inbox badge poll  (DONE)
  - Bell extracted to `web/templates/inbox_bell.html` (`{{define "inbox_bell"}}`), parsed into every page set and into its own set for the endpoint; `layout.html` includes it, so the page and the poll render the same markup
  - The anchor itself carries `hx-get="/inbox/count" hx-trigger="every 60s [!document.hidden]" hx-swap="outerHTML"` — the endpoint returns the whole anchor, so the trigger survives each swap (a child-only swap would kill polling)
  - `GET /inbox/count` → `InboxCount`: auth gate (HX 401 + `HX-Redirect`, plain 303), `CountUnreadNotifications`, renders the fragment
  - Visibility filter verified against the vendored htmx 2.0.4: the `every` trigger spec parses an `eventFilter` and the poll timer re-evaluates it each tick, so hidden tabs skip the request and visible tabs resume
  - Tests: guest 401/303; fragment has the anchor + badge + poll attrs and is not a page; cleared inbox drops the badge; full page ships the trigger

- [x] Task 2: Thread "N new replies" pill  (DONE)
  - `NewPostsAfter` query `(thread_id, after, viewer_id)` → count + `first_new_id`, excluding the viewer's own posts (their reply is already in their DOM)
  - `NewPostsView{Count, UpdatesURL, ReloadURL, Poll}` + `newPostsView`; `ThreadPage` passes the newest rendered post id
  - `web/templates/new_posts.html` container polls `GET /threads/{id}/updates?after=N` every 20s (hidden-tab filter, `outerHTML`); the pill is a **reload link** `/threads/{id}?from={first}#post-{first}` (`?from=` only forces the navigation; a fragment-only change would not reload)
  - Why reload instead of an HTMX append: the viewer's own HTMX replies can interleave with others' posts, so appending only the unseen ones would break chronological order; a reload also refreshes the read/notification markers
  - `ThreadUpdates` always answers 200 with the container; missing/expired thread → non-polling container (stops the loop quietly, since a 4xx would not be swapped and an empty body would delete the poller)
  - CSS: `.new-posts.has-pill` (only takes space with a pill) + `.new-posts-pill` shadow; `make css`
  - Tests: page container `after={opening}`; one/two replies raise the count; own posts never count; reload pins `after` to the newest and drops the pill; expired and unknown threads return non-polling containers

- [x] Task 3: Docs + smoke  (DONE)
  - `specs/disjoint.md` §D **Liveness (polling, no SSE)** bullet; README **Live Updates** bullet
  - Live smoke on a fresh instance: admin thread → bob `@admin` reply → thread page container `after=1` → `updates?after=1` returns "1 new reply" linking `?from=2#post-2` while still polling → bob's own poll shows no pill → `inbox/count` returns the badge → opening the thread clears the mention (read-through) → new mention re-raises the badge → mark-all zeroes it; polling attrs and CSS served

- [x] Checkpoint: Complete
  - [x] `go test -race -count=1 ./...` green; `gofmt -s -l` / `go vet` clean
  - [ ] Review with human

---

# Task List: Image Wall (`/gallery`) — DONE

Plan: [tasks/plan-gallery.md](plan-gallery.md)

Read-only view over existing data: every image attachment on a live thread
(opening posts **and** replies), newest first, CSS-columns masonry over natural
aspect ratios, existing lightbox on click, caption links to the post. No
migration, no schema change, no JS, no new dependency.

- [x] Task 1: Query + handler + route + template + navbar + CSS  (DONE)
  - `ListGalleryImages` (image MIME allowlist, `expires_at` filter, `ORDER BY a.id DESC`, LIMIT/OFFSET), `make generate`
  - `internal/handlers/gallery.go`: `GalleryImage` + `GalleryPage` (48/page, `?page=`, `HasNext` from a full page, no auth code — middleware gates it)
  - `GET /gallery`, `gallery.html` registered in `pages`, navbar "Gallery" beside "Emoji"
  - CSS: `.gallery-grid` (`columns: 4 15rem` + `break-inside: avoid`), `.gallery-tile`, `.gallery-caption`; `make css`
  - Manual: wall renders on a seeded DB; lightbox + caption links work; pagination

- [x] Task 2: Handler tests  (DONE)
  - guest 303 `/login`; empty DB → empty state
  - opening-post **and reply** images render; PDF/video excluded
  - expired thread's images absent; worker purge removes them (cascade)
  - newest-first ordering; pagination 48/1/empty-at-99
  - tile contract: `href="/uploads/{path}"` + `lightbox-link`, `alt` = filename, caption `href="/threads/{tid}#post-{pid}"` + thread title

- [x] Task 3: Docs + smoke  (DONE)
  - README feature bullet; `specs/disjoint.md` §E **Gallery** note (all live images incl. replies, 48/page, masonry, lightbox, thumbnail `srcset`, expiry cascade)
  - Live smoke: thread with two images and one PDF → wall shows 2, not the PDF → caption jumps to the reply → expire the thread → wall empty

- [x] Checkpoint: Complete
  - [x] `go test -race -count=1 ./...` green; `gofmt`/`go vet` clean
  - [x] No migration, no new JS
  - [ ] Review with human

---

# Task List: Image Thumbnails

Plan: [tasks/plan-thumbnails.md](plan-thumbnails.md) — DONE

Serve `/thumbs/{storage_path}?w=320|640|1280` from a lazily populated on-disk
cache. No migration, no upload-path change, no background job, no external
image tooling; full size stays available through the lightbox.

- [x] Task 1: `internal/thumbs` package (decode → orient → scale → encode)  (DONE)
  - `Ensure(uploadDir, storagePath, width) (path, derivative)`, `Remove`, `AllowedWidth`, `IsImage`, `Version = "v1"`, `DefaultWidth = 640`
  - bounds: `MaxSourcePixels = 40_000_000` checked with `image.DecodeConfig` (header only), semaphore `maxConcurrent = 2` held across decode→encode; atomic `CreateTemp` + `Rename`; cache name `<uuid>.<width>.<version>.<ext>`
  - encoder chosen by alpha scan (JPEG q82 / PNG); no upscale (visual width ≤ requested → original); GIF excluded; visual width accounts for EXIF 5–8 swapping the axes
  - `golang.org/x/image@v0.41.0` (`draw` + `_ webp` decode); `CGO_ENABLED=0` still builds
  - tests: 1000×500 → 320×160; 200px source at w=640 → original with no `thumbs/` dir; GIF + PDF → original; opaque → `.jpg`, transparent → `.png`; corrupt and missing → original; a real 100000×100000 IHDR (CRC-valid, `DecodeConfig`-verified) rejected without allocating; 4 concurrent `Ensure` calls → 1 cache file, no temp files; `Remove` clears every width

- [x] Task 2: EXIF orientation  (DONE)
  - `readJPEGOrientation` (SOI → segment walk with fill-byte handling, APP1/`Exif\0\0`, TIFF `II`/`MM`, IFD0 tag `0x0112`, every offset bounds-checked) and `applyOrientation` (all 8 cases, applied to the *scaled* image so the pixel loop is ~300k pixels, not 12M)
  - tests: TIFF parser table (both byte orders, missing tag, bad magic/order, truncated, OOB IFD, out-of-range value); JPEG walk table (APP0 skipped, no EXIF, non-JPEG, truncated, fill bytes); 3×2 pixel-matrix assertion for all 8 orientations; alpha preserved through the transform; 5–8 swap dimensions

- [x] Task 3: Handler + route + templates  (DONE)
  - `ServeThumb`: `filepath.Base` + identity check, image-extension allowlist, width allowlist (default 640, unknown → 404), `immutable` on derivatives / `max-age=86400` on fallbacks, `http.ServeFile`
  - `GET /thumbs/{filename}` beside `/uploads/`
  - post card: `src` 640 + `srcset` 320/640/1280 + `sizes` + `decoding="async"`; front-page strip `?w=320`; lightbox `href` still `/uploads/…`

- [x] Task 4: Worker cleanup + handler tests  (DONE)
  - `thumbs.Remove(w.uploadDir, att.StoragePath)` right after the original is deleted
  - tests: 200 + `image/jpeg` + 320×160 + cache file created; second request byte-identical; default width 640; `w=999` / `w=abc` / missing file / PDF → 404; traversal does not succeed; guest 303; fallbacks (`small.png`, fake PNG, GIF) serve original bytes with the original type and a non-immutable policy and write no cache file; worker purge removes the cached width and the original

- [x] Task 5: Docs + smoke  (DONE)
  - README image bullet; `specs/disjoint.md` §E **Thumbnails** paragraph (route, widths, lazy generation, EXIF, encoding, atomic write, immutable caching + versioning, fallbacks, semaphore/pixel cap, worker cleanup, CGO-free)
  - live smoke on a fresh instance: a 1200×600 JPEG carrying EXIF orientation 6 served `?w=320` decodes to **320×640** (upright, axes swapped) and `?w=640` to 640×1280, while `/uploads/` still returns the untouched 1200×600 original; the cache directory holds only the generated variants

- [x] Checkpoint: Complete
  - [x] `go test -race -count=1 ./...` green; `gofmt -s -l` / `go vet` clean
  - [x] `CGO_ENABLED=0` build succeeds; no migration, no new table/column
  - [ ] Review with human

---

# Task List: Lightbox Navigation (swipe / arrows / keys)

Plan: [tasks/plan-lightbox-navigation.md](plan-lightbox-navigation.md) — DONE

Client-side only: the dialog opens with the whole `[data-lightbox-group]`'s
images (thread post order), with a counter, arrow buttons, ←/→ & Home/End, and
pointer-based swipe. Thumbnail paints first, the original swaps in when
loaded. No endpoints, no schema, no dependency.

- [x] Task 1: Markup + group attribute + CSS  (DONE)
  - `layout.html` dialog: prev/next overlay buttons (`aria-label`), `aria-live` counter + filename meta, `draggable="false"`, `aria-label="Image viewer"`
  - `thread.html`: `data-lightbox-group` on `#posts-container`
  - CSS: overlay arrows (2.75rem / 2.25rem mobile), `:disabled` opacity, meta line, explicit `[hidden]` overrides (the `display` trap), `touch-action: pan-y` + drag/selection suppression on the image; **no** `position: relative` on `.lightbox` (it would break the modal containing block); `make css`
  - `TestLightboxNavigationMarkup`: group attribute, both labelled nav buttons, live counter, `draggable="false"`, 2 `.lightbox-link`s pointing at `/uploads/` with `/thumbs/` srcset, dialog also present on the front page but no group there
  - Side fix: `TestInboxPagination` matched the bare words "Prev"/"Next", which the new aria-labels also contain — now matches the pagination labels (`&larr; Prev` / `Next &rarr;`)

- [x] Task 2: Navigation core in `bindLightbox`  (DONE)
  - `closest('[data-lightbox-group]')` scope with single-image fallback; DOM-order collection; `{full, thumb: currentSrc, alt}`; `render()` sets counter/filename, disabled+clamped ends, hides chrome when `items.length < 2`; `go(delta)` clamps; state reset on the async `close` event
  - arrow buttons + dialog `keydown` (←/→/Home/End; modifier combos ignored; `preventDefault` only for handled keys; Escape untouched)

- [x] Task 3: Swipe  (DONE)
  - dialog pointerdown/up/cancel; drags starting on `.lightbox-nav`/`.lightbox-close` ignored; `|dx| >= 40 && |dx| > 1.5|dy|`; left → next, right → prev

- [x] Task 4: Progressive full-size  (DONE)
  - detached `Image` preloads the original; `src` is swapped only after `onload` and only while the token still matches (stale loads warm the cache instead of repainting)

- [x] Task 5: Docs + verification  (DONE)
  - README **Image Viewer** bullet; `specs/disjoint.md` §D **Image Viewer** bullet (group scope, key map, swipe threshold, thumb-first rendering, clamps, no server involvement)
  - `node --check web/static/disjoint.js` passes
  - **Real headless Chromium (CDP) run** on a 4-image thread (3 opening + 1 reply): clicking the 3rd opens `3 / 4` with the right filename; thumbnail first, then the original; ArrowRight/ArrowLeft/Home/End and end-clamping; swipe left ×2 and swipe right; vertical drag ignored; a drag starting on the arrow ignored; prev/next buttons; close clears state and `src`; a single-image thread hides both arrows and the counter. Also covered: dialog present but empty before opening, and no group on the front page

- [x] Checkpoint: Complete
  - [x] `go test -race -count=1 ./...` green; `gofmt -s -l` / `go vet` clean
  - [x] No server/schema/template-data changes; `node --check` passes
  - [ ] Review with human

---

# Task List: @mention Autocomplete — DONE

Plan: [tasks/plan-mention-autocomplete.md](plan-mention-autocomplete.md)

- [x] Task 1: Query + handler + route  (DONE)
  - `ListMentionCandidates` (case-insensitive prefix, presence-first then username, LIMIT 10) + sqlc
  - `GET /api/mentions` returns `{"users":[{"name","avatar"}]}`, escapes LIKE metacharacters, rejects q > 32 with 400
  - Handler tests: guest redirect, prefix/case, empty q (10 max), presence ordering, `%`/`_` escaping, long q
- [x] Task 2: JS detection + fetch + dropdown  (DONE)
  - `web/static/mentions.js`: caret token rule, 120 ms debounce, AbortController, DOM-built options (no innerHTML)
- [x] Task 3: Keyboard + mouse selection  (DONE)
  - ArrowUp/Down wrap, Enter/Tab/click insert `@name ` with trailing space via `setRangeText`, Escape/blur close, Ctrl/Cmd+Enter still submits
- [x] Task 4: Accessibility + polish  (DONE)
  - listbox/option roles, `aria-expanded`/`aria-controls`/`aria-activedescendant`, themed dropdown CSS, avatar rows
- [x] Task 5: All compose boxes  (DONE)
  - Reply, new thread, edit post, and bio textareas bind automatically; form reset closes the list
- [x] Task 6: Spec + docs  (DONE)
  - README feature bullet + `specs/disjoint.md` endpoint/trigger/insertion paragraph
- [x] Checkpoint: Complete
  - [x] `go test -race -count=1 ./...` green; `gofmt`/`go vet` clean
  - [x] No schema change, no new dependency
  - [ ] Review with human
