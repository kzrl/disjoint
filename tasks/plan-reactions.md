# Implementation Plan: Slack-Style Reactions

## Overview
Add Slack-style reactions to every post on the thread page. Each post gets a
reaction bar; clicking a reaction **toggles** the current user's reaction
(Slack semantics: one reaction per user per emoji per post). Two kinds of
emoji:

1. **Built-in unicode emoji** — a seeded set (`joy`, `excited`, …), rendered
   as glyphs.
2. **Custom emoji** — user-uploaded images (PNG/GIF), Slack-style,
   sourced e.g. from [slackmojis.com](https://slackmojis.com/). Stored as
   files, served from a new `/emoji/` route, named `:snake_case_name:`.

The whole feature rides on existing HTMX partial-swap patterns — no new
JavaScript framework, no polling.

**Scope**
- Reactions on posts: the thread's opening post *is* a post, so "react to
  thread" and "react to comment" are the same feature — one target type.
- A community emoji page: browse all emoji, upload new custom emoji, delete
  your own / (admin) any.
- Reaction chips show counts; active state when *you* reacted; a picker to
  add any emoji (custom set will be large enough that chips alone aren't
  viable — the picker is the primary affordance).
- Explicitly out of scope: `:name:` emoji tokens in Markdown post bodies,
  reaction notifications, who-reacted popover, per-emoji usage stats,
  reactions on the front page, admin moderation beyond delete.

## Current-state facts that shape the design
- Every post has a real author and lives in `posts` (thread title is separate,
  but the opening post is a normal `posts` row created in the same transaction).
- Migrations run through **goose** (`db/migrations/NNNNN_*.sql`, single-file
  `+goose Up` / `+goose Down`); `db/schema.sql` is the cumulative snapshot that
  **sqlc** reads (`sqlc.yaml` → `internal/database`). Both must be updated
  together (precedent: every migration since 00002).
- File upload plumbing already exists: `saveUploadedFiles` (multipart →
  UUID-named file on disk), the `attachments` table (filename, storage_path,
  mime_type, file_size), and `ServeUpload` at `/uploads/{filename}`. Custom
  emoji uploads reuse this pipeline nearly verbatim.
- The thread page is HTMX: new replies append to `#posts-container` via
  `hx-swap="beforeend"`, so per-post UI must work on HTMX-appended cards.
- Inline `<script>` snippets are already used (quote button,
  `hx-on::after-request`), and no `script-src` CSP is set — a small inline
  script for the picker is consistent.
- `handlers_test.go` covers DB state + rendered HTML for other features;
  reaction/emoji tests follow the same pattern.
- Expired-thread purge worker deletes `posts` → FK `ON DELETE CASCADE` takes
  reactions with them for free.

## Architecture decisions

### D1 — One `emojis` table (built-ins + customs) and one `reactions` table
```sql
CREATE TABLE emojis (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,       -- 'joy' or 'mario_fire' (:name: form)
    source TEXT NOT NULL,            -- 'unicode' | 'custom'
    glyph TEXT,                      -- unicode emoji only, e.g. '🙂'
    storage_path TEXT,               -- custom emoji only; NULL for unicode
    mime_type TEXT,                  -- custom emoji only: image/png etc.
    file_size INTEGER,               -- custom emoji only
    created_by INTEGER,              -- custom emoji only; FK users ON DELETE SET NULL
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL
);

CREATE TABLE reactions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    post_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    emoji_id INTEGER NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (post_id, user_id, emoji_id),
    FOREIGN KEY (post_id) REFERENCES posts(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (emoji_id) REFERENCES emojis(id) ON DELETE CASCADE
);
CREATE INDEX idx_reactions_post_id ON reactions(post_id);
```
- One table for both kinds means reactions, the picker, and the toggle
  handler all deal with `emoji_id` — no branching between "unicode name" and
  "custom name" anywhere in the reaction path.
- Built-ins are **seeded rows** (`source='unicode'`) in migration 00011, so
  the DB is the single source of truth for what exists. Adding a built-in
  later is just an INSERT (new migration); stored reactions never break.
- `UNIQUE (post_id, user_id, emoji_id)` enforces Slack's one-per-user-per-emoji
  at the DB level; toggle = `INSERT OR IGNORE` and, if 0 rows inserted,
  `DELETE` (sqlc `:execrows` tells us which branch ran).
- Deleting a custom emoji cascades its reactions (Slack does the same).
- No `thread_id` column: `post_id → posts.thread_id` when we ever need it.

### D2 — Custom emoji: names, storage, serving
- **Names** are Slack-style: `^[a-z0-9_]{2,64}$`, unique across built-ins and
  customs. The regex guarantees the name is safe inside `hx-vals`, `href`,
  and HTML attributes — no escaping edge cases. Built-in names (`joy`, …) are
  implicitly reserved because the UNIQUE constraint covers them.
- **Storage**: reuse `saveUploadedFiles` (UUID filename on disk in the
  existing uploads dir); the `emojis` row records storage_path/mime/size
  exactly like `attachments` does. **Serving**: reuse the existing
  `GET /uploads/{filename}` handler (`ServeUpload`) — no new route, no new
  disk directory, no new cache-header code.
- **Validation at upload**: allow only `image/png` and `image/gif`
  (GIFs animate natively in `<img>` — slackmojis.com staples). WEBP was in
  the original plan but is **dropped**: `http.DetectContentType` does not
  recognize webp magic bytes (sniffs as `application/octet-stream`), and a
  stdlib webp decoder does not exist, so a webp upload could not be verified
  without a new dependency — and this app is zero-dependency.
  Cap file size at **256KB** (Slack's limit is 128KB; a little headroom);
  decode with `image.Decode` to reject non-images and enforce max
  **256×256px** (first frame for GIF).
- **Upload/management page** `GET /emojis` (community emoji page):
  - grid of all emoji (glyph or thumbnail), each showing `:name:` and, for
    customs, an uploader + delete button (owner or admin, mirroring the
    invite-delete pattern);
  - upload form: image file + `:name:` input, `POST /emojis` (multipart).
  - Errors (duplicate name, bad type/size/dimensions) flash back to the form,
    temp file removed (same cleanup pattern as `CreateThreadSubmit`).
  - Nav link on the layout (next to Users/Invites).

### D3 — Toggle endpoint returns the post's reaction bar partial
`POST /posts/{id}/reactions` (form field `emoji_id`; post IDs are global, no
thread segment needed). Handler:
1. Auth gate identical to `CreatePostSubmit` (HTMX 401 + `HX-Redirect: /login`).
2. Verify post exists via `GetPostByID` (404 otherwise — covers purged posts).
3. Verify `emoji_id` exists via `GetEmojiByID` (400/404).
4. `AddReaction` (`INSERT OR IGNORE`, `:execrows`); if 0 → `RemoveReaction`
   (`:execrows`). No transaction needed — each statement is atomic and the
   UNIQUE constraint makes concurrent toggles safe (one wins the insert, the
   other deletes; final state = one user's latest click, same as Slack).
5. Load the post's reactions (joined with `emojis`), group them, and render
   **only** the reaction bar (`reaction_bar.html`), which the chip's
   `hx-swap="outerHTML"` replaces in place. No full post re-render, no
   attachment reload.

### D4 — Reaction bar partial, rendered on every post card
New template `reaction_bar.html` (`{{define "reaction_bar"}}`) containing a
compact row of the post's *existing* reactions, then a picker trigger:
```html
<button type="button"
        class="reaction-chip{{if .You}} is-active{{end}}"
        hx-post="/posts/{id}/reactions"
        hx-target="closest .reaction-bar"
        hx-swap="outerHTML"
        hx-vals='{"emoji_id":7}'
        title="joy (3)">🙂 3</button>
```
- Chip content is the glyph (`<span>`) for unicode emoji or
  `<img class="emoji-img" src="/uploads/{storage_path}">` for custom —
  a single template helper (`emojiContent`) handles both.
- Included from `post_card.html` and `post_partial.html` right after
  body/attachments. `PostViewItem` / `SinglePostViewItem` each gain a
  `Reactions` field.

### D5 — Picker with zero frameworks (the primary affordance)
Since the custom set will be sizable, the bar's `+` button opens the picker:
a server-rendered `<div class="emoji-picker">` (hidden by default) with a
compact grid of **all** emoji — each cell a `<button>` carrying the same
`hx-post`/`hx-vals` as chips, glyph or 32px thumbnail. ~10 lines of inline JS
toggle it closed/open and closes on outside-click (consistent with the
existing quote-button approach). Optional, cheap enhancement: a single
client-side `<input>` text filter over the grid (`:name:` substring,
inline JS) — include if trivial, skip without fuss. All buttons are real
`<button>`s, so keyboard works natively.

### D6 — Thread page data flow (two extra queries, grouped in Go)
`ThreadPage` runs `ListReactionsByThreadID :many` (reaction rows joined with
`emojis` name/glyph/storage_path for the whole thread) and groups in Go into
`map[postID][]ReactionGroup` where `ReactionGroup{Emoji Emoji, Count int, You
bool}`. The picker on every card needs the full emoji list, so `ThreadPage`
also runs `ListAllEmojis :many` once and passes it to the layout-level picker
rendering (cards embed `pickerEmoji` from the shared data). Threads are small
enough that N+1-per-post is unnecessary. The HTMX partial for a brand-new
post renders the bar with an empty group set (just the `+`).

## Tasks

- [x] **Task 1: Migrations + queries + sqlc**  (DONE)
  - `db/migrations/00011_emojis_and_reactions.sql` (goose single-file; plain
    additive DDL — `CREATE TABLE` ×2 + `INSERT` seed of ~12 built-in unicode
    rows; no `NO TRANSACTION` needed, cf. 00002/00009)
  - Add both tables to `db/schema.sql` snapshot
  - `db/query.sql`: `AddReaction :execrows` (INSERT OR IGNORE),
    `RemoveReaction :execrows`, `ListReactionsByThreadID :many` (JOIN emojis),
    `ListAllEmojis :many`, `GetEmojiByID :one`, `GetEmojiByName :one`,
    `CreateEmoji :one`, `DeleteEmoji :exec`
  - `sqlc generate`
  - Verify: fresh DB migrates to v11 with built-in rows, existing
    `disjoint.db` upgrades, `goose -dir=db/migrations down` works;
    `go build ./...`

- [x] **Task 2: Custom emoji upload + management page**  (DONE)
  - `internal/handlers/emojis.go`: `EmojiPage` (grid + upload form),
    `CreateEmojiSubmit` (multipart; name/type/size/dimension validation;
    flash + temp-file cleanup on error), `DeleteEmojiSubmit` (owner or admin)
  - Routes in `app.go`: `GET /emojis`, `POST /emojis`,
    `POST /emojis/{id}/delete`; nav link in `layout.html`
  - `web/templates/emojis.html` (Bulma grid, consistent with users/invites
    pages)
  - `handlers_test.go`: upload happy path (rows + file on disk + rendered
    grid), duplicate name, bad MIME, oversize, non-image bytes, oversized
    dimensions; delete permission (owner yes, other user no, admin yes) and
    cascade of reactions

- [x] **Task 3: Reaction toggle handler + route**  (DONE)
  - Note: `ListReactionsByPostID :many` added (the per-post rows the toggle
    response renders; `ListReactionsByThreadID` stays for the thread page)
  - Note: `reaction_bar.html` partial created here (chips + `+` button) since
    the handler renders it; Task 4 adds the picker + thread-page wiring
  - `ToggleReaction` handler (auth gate, post 404, emoji 404/400, toggle
    logic via `:execrows`, render `reaction_bar.html`)
  - Route `POST /posts/{id}/reactions` in `app.go`
  - `handlers_test.go`: toggle adds → chip shows count 1 + active; second
    toggle removes; two users on same emoji → count 2; unicode + custom emoji
    both render (glyph vs `<img src>`); anonymous HTMX → 401 +
    `HX-Redirect`; missing post → 404; deleting an emoji removes its
    reactions

- [x] **Task 4: Reaction bar, picker, thread-page wiring, CSS**  (DONE)
  - Note: picker JS lives in `web/static/disjoint.js` (document-level
    delegation), not an inline snippet — the file already exists for the
    paste/drop feature and loads on every page
  - Note: the bar renders between the body and the attachments in
    `post_card.html` (one include covers `thread.html` and the HTMX
    `post_partial.html`, which just delegates to `post_card`)
  - Note: guests never see thread pages (auth middleware requires a session
    on every route), so the bar's guest path is untested by design
  - Extend `web/templates/reaction_bar.html` (chips + `+` already exist from
    Task 3) with the picker grid; the chip img/glyph branching is inline in
    the template (no `emojiContent` helper needed)
  - Wire `Reactions` + full emoji list into `PostViewItem` /
    `SinglePostViewItem` and `ThreadPage` (reuse the `groupReactions` helper
    from reactions.go); include the bar in `post_card.html` +
    `post_partial.html`
  - Site stylesheet: `.reaction-chip`, `.reaction-bar`, `.emoji-picker`,
    `.emoji-img` (1.25em inline, dark-first, consistent with quote button)
  - Verify: HTMX-appended post's bar works; refresh preserves state; picker
    works from every card on the page

- [ ] **Task 5: End-to-end verification**
  - Fresh DB + pre-existing DB both pass `go build ./...` && `go test ./...`
  - Manual smoke: upload a few slackmojis.com GIF/PNGs, react with them on
    the opening post (thread) and on replies, unreact, delete an emoji and
    confirm its reactions disappear; logout → chip redirects to login
  - README feature list + `tasks/todo.md` entry

## Risks / open questions
- **Concurrent toggles** (two tabs, same user, same emoji): last click wins,
  same as Slack; acceptable. If we ever want it bulletproof, wrap add/remove
  in `BEGIN IMMEDIATE` — not worth it now.
- **GIFs**: animate fine in `<img>`; size limit (256KB) keeps animated GIFs
  small; dimension check uses the first frame, which is all `<img>` cares
  about for layout.
- **slackmojis.com content**: these are user-contributed community uploads
  (many trademark-adjacent, e.g. character faces). They're fine for a
  self-hosted private group; worth a one-liner in the emoji page UI hint
  ("upload emoji for your own community") and in the README.
- **Picker scale**: with a large custom set the picker grid grows linearly;
  at hundreds of emoji, add the client-side filter (D5) or server-side
  `?q=` search. Not needed until the set is actually big.
- **Front page reaction totals**: cheap follow-up (one GROUP BY over recent
  threads) — deliberately deferred.
