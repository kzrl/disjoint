# Implementation Plan: Mentions & Replies Inbox

## Overview

Today nothing in Disjoint tells you that someone mentioned you or answered a
thread you care about. `@username` renders as a pill, but the mentioned user
only finds out by re-reading the front page; a reply to your thread is equally
silent. This plan adds a **server-rendered inbox of conversations**, a navbar
badge, and the write path that fills both.

Deliberately *not* an event log: the inbox holds **one row per (recipient,
thread)**. Twenty replies to your thread are one entry, not twenty. That keeps
the badge meaningful ("threads with something new for you"), makes the query a
single indexed scan, and means the inbox cannot be flooded by a chatty thread.

No new dependencies, no JS framework, no email/SMTP, no push. Everything is
HTMX-free server rendering except one optional stretch task.

## Goals

- A mentioned user learns about it without polling the front page.
- A thread author learns about replies; so does anyone else who posted in the
  thread (the "conversations I'm in" model from the app's pitch).
- The navbar carries an unread count; `/inbox` lists the conversations.
- Opening a thread clears that thread's inbox entry (you have read it).
- A “Mark all read” button in the inbox header clears the badge in one click;
  the inbox never clears itself.
- Notifications die with their thread (the expiry worker must not leave dead
  links) and with their user.

## Non-goals

- Email, web push, or any out-of-band delivery.
- Per-thread mute, digest batching, or per-user notification preferences.
- Notifying on edits, reactions, or thread renames.
- Backfilling historical mentions/replies (the inbox starts empty).
- Real-time badge push (see the stretch task; polling only, if at all).

## Current-state facts that shape the design

- **Anonymous posting no longer exists** (`db/migrations/00006_drop_and_purge_anonymous.sql`
  rebuilt `posts`/`threads` with `user_id NOT NULL`). The earlier plan's rule
  "never notify for anonymous posts or the actor column re-links anonymous
  content" **does not apply** — every post has a real, displayable author.
- **The mention parser already exists and is authoritative**
  (`internal/markdown/mention.go`): `KindMention`, `*Mention{Name}`, a
  `[A-Za-z0-9]{3,32}` token, left-boundary rule, code-span/link-label/email
  exclusions. The renderer emits `data-mention="@name"`. Notifications must be
  derived from *this* parser, never from a hand-rolled regex, or the inbox and
  the rendered pills will disagree.
- **One shared goldmark instance** (`internal/markdown/markdown.go:10`)
  configured with GFM + `mentionExt`; `Render` is the only consumer.
- **Post creation is transactional** in both paths:
  `CreatePostSubmit` (`internal/handlers/posts.go:40`) and
  `CreateThreadSubmit` (`internal/handlers/threads.go:72`) each open a tx,
  insert post + attachments through `qtx`, and commit. Notifications belong in
  the same tx (same precedent as attachments).
- **`ThreadPage` already writes a best-effort read marker**
  (`internal/handlers/threads.go:389`, `MarkThreadRead`); the inbox read-through
  goes directly beside it.
- **`render()` (`internal/handlers/app.go:212`) is the single choke point** for
  every full page render, and it already injects `CurrentUser`, `PageTitle`,
  flash, and theme into the page map. It is the natural place for the navbar
  badge count (all 13 `app.render(...)` calls pass a `map[string]any`).
- **`commentSnippet` (`internal/handlers/front.go:252`)** already flattens a
  markdown body to a ~120-char plain-text preview, whitespace-folded and
  escaped by the template. The inbox reuses it (needs a small extraction so a
  template func can call it on a plain `string`).
- **`thread_reads`** (`db/migrations/00014_thread_reads.sql`) is the existing
  precedent for a per-user, per-thread marker table with cascading FKs — the
  notifications table copies that shape.
- **LIFO of migrations**: latest is `00014`; the new one is `00015`.
- **The soft-404 catch-all** is `GET /` (`app.go:315`). Go 1.22's mux prefers
  the more specific pattern, so `GET /inbox` needs no ordering trick.

## Architecture Decisions

### Decision 1 — One row per (recipient, thread), not per event

`PRIMARY KEY (user_id, thread_id)`. A new trigger upserts: points `post_id` at
the newest triggering post, bumps `last_event_at`, clears `read_at`, and
upgrades `reason` `reply → mention` (a mention is never downgraded to reply).

Why: a thread that gets 20 replies should be one inbox entry and **one** badge
increment. It also makes "mark read" a one-row-per-thread operation keyed by
`thread_id`, which is exactly what visiting a thread needs.

Rejected: an append-only `events` table. It needs aggregation/dedup queries for
every render, grows unboundedly, and makes the badge a count of *events*
(100 unread replies from one thread looks alarming). Rejected: one row per
post with a read flag — same flooding problem, and the badge becomes a
count of posts rather than of threads.

Trade-off accepted: `reason` is sticky. Once you are mentioned in a
long-running thread, later plain replies keep the row labelled "mentioned you"
even though the *latest* trigger was a reply. Documented in the schema comment;
the alternative (recompute `reason` from the whole thread each time) is not
worth a second query.

### Decision 2 — No `actor_id` column; the actor is derived

The inbox needs "who did this". That is the author of `notifications.post_id`,
so `ListInbox` joins `posts p ON p.id = n.post_id` and `users u ON u.id =
p.user_id`. No denormalized actor can go stale, and deleting a user cascades
their posts, which cascades the notification (no dangling "someone" rows).

### Decision 3 — Reply recipients = everyone who posted in the thread

`SELECT DISTINCT user_id FROM posts WHERE thread_id = ?` — this already
includes the thread author (the opening post is theirs), so one query covers
"author" and "participants". The actor is removed in Go. A brand-new thread's
only participant is the author, so the same helper is a no-op for
`CreateThreadSubmit` — **one code path for both creation flows, no special
casing**.

Why not author-only: the app is pitched as private-chat-adjacent; being
notified when a conversation you took part in continues is the expected
behaviour. The flooding risk is bounded by Decision 1 and by expiry (threads
die, taking their inbox rows with them).

### Decision 4 — Explicit read: nothing is cleared behind your back

Visiting `/inbox` does **not** mark anything read. Rows keep their unread
highlight and the badge keeps its count until one of exactly two things
happens:

- **You open the conversation.** The row links to
  `/threads/{id}#post-{pid}`; `ThreadPage`'s read-through clears that
  thread's entry (see Handlers). This is the per-row read path and needs no
  extra endpoint.
- **You press “Mark all read”** in the inbox header → `POST /inbox/read` →
  `MarkAllNotificationsRead` → flash → `303 /inbox`.

Why: read-on-view makes a “Mark all read” button meaningless (everything is
already read by the time the page renders) and silently discards rows you
never looked at. Explicit read makes the badge a claim about what you have
*dealt with*, not about which page you happened to load — which is what the
button is for.

Rejected: read-on-view **plus** a button (a dead control on first paint, and
it lies on reload). Rejected: a per-row read button (a route, a form, and a
round trip per row for an action the thread visit already performs).

CSRF: the new route is a plain `POST` form. The session cookie is
`SameSite=Lax` (`internal/auth/auth.go:74`), so no cross-site POST carries
it — the same baseline every existing state-changing form (logout, reactions,
invites) relies on. No token plumbing is introduced.

### Decision 5 — Badge count in `render()`, not in every handler

`render()` already injects per-request chrome. One indexed
`CountUnreadNotifications` per full-page response is cheap and guarantees the
badge is correct on every page without touching 13 handlers. On error the
badge is simply omitted; the page must never fail because a count failed.

### Decision 6 — Notifications are written inside the post's transaction

`qtx.UpsertNotification` runs between the post insert and `tx.Commit()` in both
creation paths, matching how attachments are handled. A failed notification
aborts the post — acceptable because a failure implies the DB is failing
anyway; consistency beats silently missing posts. (If this ever proves
brittle, moving the writes after `Commit` is a two-line change with the
opposite trade-off.)

### Decision 7 — Extraction lives in `internal/markdown`, not in handlers

`markdown.Mentions(body) []string` walks the goldmark AST for `KindMention`
nodes. This is the whole correctness argument for the feature: **notified users
== rendered pills**, by construction. A regex would notify on
`user@gmail.com`, `@foo` inside a code span, and `[@x](/y)`.

### Decision 8 — No mute in v1

Expiry already bounds noise: a thread that stops being interesting stops
existing. Adding `thread_mutes` now would triple the surface area (table,
toggle, thread header UI, tests) for a problem that has not been observed yet.
Recorded as the first follow-up if the inbox proves noisy.

## Schema (`db/migrations/00015_notifications.sql`)

```sql
-- +goose Up
-- The inbox: one row per (recipient, thread). An inbox entry is a
-- conversation that needs attention, not a single event, so a busy thread
-- bumps its row instead of adding one. reason is the strongest trigger seen
-- ('mention' is never downgraded to 'reply'); read_at is cleared by any new
-- trigger. Both FKs cascade: purging an expired thread or deleting a user
-- removes the entry for free, so the inbox can never link to a dead thread.
CREATE TABLE notifications (
    user_id       INTEGER NOT NULL,
    thread_id     INTEGER NOT NULL,
    post_id       INTEGER NOT NULL,             -- newest triggering post
    reason        TEXT NOT NULL CHECK (reason IN ('mention', 'reply')),
    last_event_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    read_at       DATETIME,                     -- NULL = unread
    PRIMARY KEY (user_id, thread_id),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE,
    FOREIGN KEY (post_id) REFERENCES posts(id) ON DELETE CASCADE
);

-- Inbox listing: newest activity first, per user.
CREATE INDEX idx_notifications_inbox ON notifications(user_id, last_event_at DESC);
-- Badge count: partial index, only unread rows are ever counted.
CREATE INDEX idx_notifications_unread ON notifications(user_id) WHERE read_at IS NULL;

-- +goose Down
DROP TABLE notifications;
```

Also add the table to `db/schema.sql` (the sqlc input snapshot) verbatim, then
`make generate`.

Notes:
- No `created_at`: the first trigger time is not displayed anywhere, and
  `last_event_at` is the only ordering key. Add it if a "since …" line is ever
  wanted.
- `post_id` may be deleted independently of its thread (nothing does that
  today); the cascade would drop the inbox row rather than leave a dangling
  pointer, which is the right failure mode.

## Queries (`db/query.sql`)

```sql
-- name: UpsertNotification :exec
-- Record or bump one inbox entry. A new trigger makes the row unread again,
-- points it at the newest triggering post, and upgrades 'reply' to
-- 'mention' (never the reverse). Called inside the post-creation tx.
INSERT INTO notifications (user_id, thread_id, post_id, reason)
VALUES (sqlc.arg('user_id'), sqlc.arg('thread_id'), sqlc.arg('post_id'), sqlc.arg('reason'))
ON CONFLICT(user_id, thread_id) DO UPDATE SET
    post_id       = excluded.post_id,
    reason        = CASE WHEN excluded.reason = 'mention' THEN 'mention' ELSE notifications.reason END,
    last_event_at = CURRENT_TIMESTAMP,
    read_at       = NULL;

-- name: ListInbox :many
-- The viewer's inbox: one row per thread, newest activity first. The actor
-- is the author of the triggering post (no denormalized actor column).
SELECT n.thread_id,
       n.post_id,
       n.reason,
       n.last_event_at,
       n.read_at,
       t.title,
       p.body,
       p.url,
       u.username     AS actor_username,
       u.avatar_path  AS actor_avatar,
       u.avatar_nonce AS actor_avatar_nonce
FROM notifications n
JOIN threads t ON t.id = n.thread_id
JOIN posts p   ON p.id = n.post_id
JOIN users u   ON u.id = p.user_id
WHERE n.user_id = sqlc.arg('user_id')
  AND (t.expires_at IS NULL OR t.expires_at >= CURRENT_TIMESTAMP)
ORDER BY n.last_event_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountUnreadNotifications :one
-- Expired-but-not-yet-purged threads are excluded: the worker only runs
-- once a minute, and the badge must not advertise a thread that already
-- 404s. The join is on the threads PK, so it is still a cheap count.
SELECT COUNT(*) FROM notifications n
JOIN threads t ON t.id = n.thread_id
WHERE n.user_id = sqlc.arg('user_id')
  AND n.read_at IS NULL
  AND (t.expires_at IS NULL OR t.expires_at >= CURRENT_TIMESTAMP);

-- name: MarkAllNotificationsRead :exec
-- Backs the inbox's "Mark all read" button (POST /inbox/read). Nothing marks
-- notifications read implicitly except opening the thread (see
-- MarkThreadNotificationRead).
UPDATE notifications SET read_at = CURRENT_TIMESTAMP
WHERE user_id = sqlc.arg('user_id') AND read_at IS NULL;

-- name: MarkThreadNotificationRead :exec
UPDATE notifications SET read_at = CURRENT_TIMESTAMP
WHERE user_id = sqlc.arg('user_id')
  AND thread_id = sqlc.arg('thread_id')
  AND read_at IS NULL;

-- name: ListThreadRecipients :many
-- Everyone who has posted in the thread (the opening post means the author
-- is always included). The caller removes the actor.
SELECT DISTINCT user_id FROM posts WHERE thread_id = sqlc.arg('thread_id');

-- name: ListUserIDsByUsernames :many
-- Resolve mentioned names to real users in one round trip. Usernames are
-- case-sensitive everywhere else (login, user cards), so membership is
-- exact; an unknown mention notifies nobody and still renders as a 404 pill.
SELECT id, username FROM users
WHERE username IN (sqlc.slice('usernames'));
```

`sqlc.arg`/`sqlc.slice` usage matches the existing file
(`ListUnreadThreadIDs`, `ListReactionsByPostIDs`).

## Extraction (`internal/markdown/mention.go`)

```go
// Mentions returns the usernames mentioned in source, in first-appearance
// order, deduplicated. It parses with the same goldmark instance that Render
// uses, so the result is exactly the set of rendered mention pills: code
// spans/fences, link labels, and e-mail local parts are excluded by the
// parser itself, not by a second, drifting rule.
func Mentions(source string) []string {
    doc, err := md.Parser().Parse(text.NewReader([]byte(source)))
    if err != nil {
        return nil
    }
    seen := make(map[string]bool)
    var names []string
    _ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
        if !entering || n.Kind() != KindMention {
            return ast.WalkContinue, nil
        }
        if m, ok := n.(*Mention); ok && !seen[m.Name] {
            seen[m.Name] = true
            names = append(names, m.Name)
        }
        return ast.WalkContinue, nil
    })
    return names
}
```

Contract test (this is the feature's correctness proof): for a table of
bodies, `Mentions(body)` must equal the `data-mention="@…"` values extracted
from `string(Render(body))`. Cases: multiple mentions, duplicate mention,
inside code span/fence, `user@gmail.com`, `[@x](/y)`, `**@x**`, unknown user
(extraction is existence-agnostic), 2- and 33-char tokens.

## Write path (`internal/handlers/notify.go`, new)

```go
// maxMentionNotifications bounds recipients per post: a body stuffed with 500
// mentions must not write 500 rows or build a giant IN list.
const maxMentionNotifications = 20

// notifyPostRecipients records inbox entries for one freshly created post.
// recipients = (thread participants ∪ mentioned users) − actor, with
// reason = 'mention' for mentioned users and 'reply' for the rest. Must be
// called with the post-creation tx's queries.
func (app *App) notifyPostRecipients(ctx context.Context, qtx *database.Queries, threadID, postID, actorID int64, body string) error
```

Steps:
1. `reasons := map[int64]string{}` (userID → reason).
2. Mention pass: `names := markdown.Mentions(body)`, truncate to
   `maxMentionNotifications`. **Skip the query entirely when `len(names) == 0`**
   — an empty `IN ()` is a syntax error (same trap the front-page image query
   documents). Otherwise `ListUserIDsByUsernames(names)`; set
   `reasons[id] = "mention"` for `id != actorID`.
3. Reply pass: `ListThreadRecipients(threadID)`; for each `id != actorID` not
   already in the map, set `"reply"`.
4. `UpsertNotification` per map entry (returning the first error).

Wiring:
- `CreatePostSubmit`: after the attachment loop succeeds, before `tx.Commit()`
  — `qtx` is already in scope.
- `CreateThreadSubmit`: same position. The only participant is the author
  (excluded), so this path effectively writes mention rows only; no branching.
- `UpdatePostSubmit` (edits) deliberately **does not** notify: an edit is not
  a new message, and re-notifying on every typo fix would be noise. Documented
  in the spec.

Ordering inside the tx is fixed: post → attachments → notifications → commit.

## Handlers & routes

### `GET /inbox` — `InboxPage` (`internal/handlers/notifications.go`, new)

- Auth gate identical to other member pages: no user → `303 /login`.
- `page` from `?page=`, clamped to ≥ 1; `limit = 30`, `offset = (page-1)*30`
  (same shape as `IndexPage`).
- `rows := ListInbox(userID, limit, offset)`.
- Build `items` with `Unread: !row.ReadAt.Valid` (no snapshot needed: the page
  no longer mutates read state), plus `Snippet` from the body and
  `ThreadID/PostID/Title/Actor…/Reason/Time`.
- **No read marking on view** (Decision 4).
- `HasNext: len(rows) == 30`, `Page`, then
  `app.render(w, r, "inbox.html", map[string]any{...}, "Inbox")`.

### `POST /inbox/read` — `MarkInboxReadSubmit` (`internal/handlers/notifications.go`)

- Auth gate as above (non-HTMX form, so a plain `303 /login` on a guest).
- `MarkAllNotificationsRead(userID)`. On error: `setFlash(flash_error, …)`,
  `303 /inbox`, and **return** (no success flash) — the button can be pressed
  again, and nothing in the page depends on the write having landed.
- On success: `setFlash(w, "flash_success", "Inbox cleared.")`, `303 /inbox`.
- No HTMX variant: the redirect re-renders the whole page with every highlight
  and the badge cleared, which is the correct end state. (An `hx-post` swap of
  just the list would leave the navbar badge stale.)

### `GET /threads/{id}` — one extra line

Next to the existing `MarkThreadRead` call (`threads.go:389`), best-effort:
```go
_ = app.queries.MarkThreadNotificationRead(ctx, database.MarkThreadNotificationReadParams{
    UserID: viewerID, ThreadID: thread.ID,
})
```

### `render()` — badge injection

Inside the existing `if m, ok := data.(map[string]any); ok` block, after
`m["CurrentUser"] = user`:

```go
if user != nil {
    if n, err := app.queries.CountUnreadNotifications(r.Context(), user.ID); err == nil {
        m["UnreadNotifications"] = n
    }
}
```

Guests simply have no key (template `{{if .UnreadNotifications}}` is false).

### Route (`internal/handlers/app.go`, `Routes()`)

```go
// Mentions & replies inbox
mux.HandleFunc("GET /inbox", app.InboxPage)
mux.HandleFunc("POST /inbox/read", app.MarkInboxReadSubmit)
```
Placed with the other member routes (after the users block).

## Templates & CSS

### `web/templates/inbox.html` (new; register in the `pages` slice, `app.go:167`)

```html
{{define "content"}}
<div>
  <section class="card forum-section my-5">
    <div class="card-header">
      <p class="card-header-title">Inbox</p>
      {{if .UnreadNotifications}}
        <form method="POST" action="/inbox/read" class="inbox-mark-all">
          <button type="submit" class="button is-small is-secondary">Mark all read</button>
        </form>
      {{end}}
    </div>
    {{if .Items}}
      <div>
        {{range .Items}}
          <div class="board-row inbox-row{{if .Unread}} is-unread{{end}}">
            <div class="board-main">
              <div class="board-title">
                {{if .Unread}}<span class="unread-dot" role="img" title="New" aria-label="New"></span>{{end}}
                {{authorAvatar .ActorAvatar .ActorUsername .ActorAvatarNonce}}
                {{userLink .ActorUsername}}
                {{if eq .Reason "mention"}}mentioned you{{else}}replied{{end}}
                in <a href="/threads/{{.ThreadID}}#post-{{.PostID}}">{{.Title}}</a>
              </div>
              <div class="is-size-7 muted">{{if .Snippet}}{{.Snippet}}{{else}}{{.URL}}{{end}}</div>
            </div>
            <div class="board-activity">
              <time class="is-size-7 muted" datetime="{{isoTime .Time}}" title="{{formatTime .Time}}">{{formatRelative .Time}}</time>
            </div>
          </div>
        {{end}}
      </div>
      <!-- pagination: same Prev/Next block as index.html -->
    {{else}}
      <div class="forum-section py-6 has-text-centered">
        <p class="muted mb-4">Nothing here yet. Mentions and replies land in this inbox.</p>
      </div>
    {{end}}
  </section>
</div>
{{end}}
```

Reuses `.board-row`, `.unread-dot`, `.muted`, `.is-size-7` — all existing.
Titles and snippets are auto-escaped; no `renderMarkdown` in the inbox (a
notification is a pointer, not a post view). The snippet falls back to the
post's `url` for link-only posts (whose body is empty), mirroring how the
front-page row degrades.

The button's visibility rides on `.UnreadNotifications`, which `render()`
injects into the *same* map the page template executes against — so the page
and the navbar badge read one number from one query. At zero unread the form
is absent from the DOM (not merely disabled): there is nothing to clear, and a
visible dead button invites the “did it work?” question.

### `web/templates/layout.html` — bell in `navbar-end` (line 47 block)

Before the user tag, signed-in only:

```html
<a href="/inbox" class="navbar-item inbox-link"
   title="Mentions and replies"
   aria-label="Inbox{{if .UnreadNotifications}}, {{.UnreadNotifications}} unread{{end}}">
  <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor"
       stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9"></path>
    <path d="M13.73 21a2 2 0 0 1-3.46 0"></path>
  </svg>
  {{if .UnreadNotifications}}
    <span class="inbox-badge" aria-hidden="true">{{if gt .UnreadNotifications 99}}99+{{else}}{{.UnreadNotifications}}{{end}}</span>
  {{end}}
</a>
```

Count lives in the `aria-label`, so the visual pill can be `aria-hidden` and
there is no duplicate announcement.

### `web/css/site.css` (+ `make css` → `web/static/disjoint.css`)

```css
.inbox-link { position: relative; }
.inbox-badge { /* small primary pill, absolute top-right, inherits theme vars */ }
.inbox-mark-all { display: flex; align-items: center; padding: 0 0.75rem; }
.inbox-row.is-unread .board-title { font-weight: 600; }
```
Only four small rules: everything else is reused Bulma/site classes. Regenerate
with `make css` (assets are `go:embed`-ed; changes need an app restart).

### Snippet reuse

Add `plainSnippet(s string) string` in `front.go` holding the current
flatten+cap logic; `commentSnippet` becomes a thin `sql.NullString` wrapper and
the func map gains `"snippet": plainSnippet`. `inbox.html` uses `{{snippet
.Body}}`.

## Out of scope (explicit)

- Email / SMTP / web push / PWA notifications.
- Per-thread mute, per-user preferences, digest batching (Decision 8).
- Reaction notifications ("3 people reacted to your post") — cheap follow-up,
  needs its own aggregation shape.
- Notifications for edits, thread renames, or deletions.
- Historical backfill; a fresh migration does not scan old posts for mentions.
- Per-row read buttons and an “unread only” filter. Reading a row happens by
  opening the thread (`ThreadPage` read-through), not by a per-row control.
- Undo for “Mark all read” (marking is monotonic; further activity re-raises
  the thread).
- Localized copy (`mentioned you` / `replied` are hardcoded, like the rest of
  the UI).

## Tasks (incremental, verifiable slices)

1. **Migration + queries + sqlc.** `00015_notifications.sql`, `schema.sql`
   snapshot, six queries, `make generate`, `go build ./...`. Verify on a fresh
   DB (goose 1→15) *and* a copy of the dev `disjoint.db` (14→15), plus
   `goose down` → 14 → up again. `PRAGMA foreign_key_check` clean.
2. **`markdown.Mentions` + contract tests.** Implement the AST walk; add
   `TestMentionsExtraction` (the pill-identity table from the Extraction
   section). No handler changes yet; suite green.
3. **Notifier + write path.** `notify.go` with the two passes and the 20-recipient
   cap; call it in both creation paths inside the tx; `snippet` func extracted
   in the same slice if convenient. Tests: mention notifies exactly the
   mentioned users; `user@gmail.com` and code-span mentions notify nobody;
   self-mention and self-reply notify nobody; reply notifies the author and
   participants but not the actor; opening a thread with a mention notifies;
   one row per (user, thread) after two replies; `reply → mention` upgrade and
   `read_at` clearing on re-trigger; >20 mentions capped.
4. **Inbox page + mark-all-read.** `InboxPage`, `MarkInboxReadSubmit`, both
   routes, `inbox.html`, header button, empty state, pagination. Tests: guest
   `GET` → 303 `/login` and guest `POST /inbox/read` → 303 without touching
   rows; rows render actor/verb/title/anchor/snippet; newest-first with cap
   30; visiting `/inbox` leaves `read_at` NULL (Decision 4); the button is
   absent at zero unread and present otherwise; `POST /inbox/read` sets every
   row's `read_at`, flashes, redirects to `/inbox`, and a reload shows no
   highlights and no button; unknown/expired thread rows absent (cascade).
5. **Navbar badge.** `render()` injection, bell markup, CSS + `make css`.
   Tests: count appears for the recipient and not for the actor; guest page has
   no badge; `>99` renders `99+`; a `CountUnreadNotifications` failure still
   renders the page (badge omitted) — can be simulated with a closed DB handle
   in a focused test.
6. **Thread read-through.** One line in `ThreadPage`; test: opening the thread
   clears that thread's notification and leaves other threads' unread.
7. **Docs + end-to-end smoke.** `specs/disjoint.md` §D gains an
   **Inbox (Mentions & Replies)** bullet (one-row-per-thread model, recipients,
   explicit read + mark-all button, badge, cascade, edits don't notify);
   `README.md` gains one feature bullet. Two-session smoke: A mentions B in a
   reply → B's badge = 1 → B reloads `/inbox` (still highlighted, badge still
   1) → B presses “Mark all read” → badge gone and no highlights → A replies
   again → badge returns → B opens the thread from the row → that row clears.
   Purge the thread (short TTL + worker) and confirm the inbox row disappears.
8. **Stretch (separate decision): badge liveness.** `GET /inbox/count`
   returning just the bell fragment, with `hx-get` + `hx-trigger="every 60s"`
   on the `navbar-end` link. Adds the app's first polling endpoint; do not
   bundle it with the inbox/mark-all-read feature or its tests.

## Risks / notes

- **Per-render COUNT.** One indexed query per full page load. If it ever shows
  up in profiling, the fix is a single cache in the layout, not moving the
  query into 13 handlers.
- **The badge is visible on `/inbox`.** Intentional: unread rows exist until
  you open them or press “Mark all read”, so the page shows the same number the
  navbar does. This is the visible consequence of Decision 4 — do not
  “simplify” it back to read-on-view, which would make the button dead on
  arrival.
- **Marking read is monotonic.** Nothing un-reads an entry except a new
  trigger (`UpsertNotification` clears `read_at`). No undo for a mis-clicked
  “Mark all read”; the rows are still in the database, just not highlighted,
  and any further activity re-raises the thread.
- **`reason` stickiness** (Decision 1) is visible in copy: a thread you were
  mentioned in keeps saying "mentioned you" for later replies. Accepted;
  fixing it means recomputing the reason from the thread, which is not worth a
  query.
- **Notification rows are written in the post tx** (Decision 6): a failure
  aborts the post. Same trade-off as attachments today.
- **Usernames are case-sensitive** end to end. `@Alice` notifies nothing when
  the account is `alice`, and already renders a 404 pill; the inbox is
  consistent with the renderer rather than trying to be clever with
  case-folding.
- **`IN` list bound**: `maxMentionNotifications` caps the mention query
  parameters; also cap the parsed names *before* the query, not after. An
  empty name list must never reach `ListUserIDsByUsernames` (empty `IN ()`).
- **Expiry window**: the worker purges on a 1-minute ticker, so an expired
  thread can survive up to ~60s. Both `ListInbox` and the badge count filter
  `expires_at` in SQL so neither ever links to or counts a thread that
  `ThreadPage` would 404.
- **No `thread_reads` interaction**: unread dots and notifications are
  independent signals (a thread can be unread with no notification, e.g. after
  pressing “Mark all read”).
- **Soft-404**: `GET /inbox` is a more specific pattern than `GET /`, so Go
  1.22 routing resolves it first regardless of registration order.

## Acceptance criteria

- [ ] A reply to my thread, and a reply in a thread I posted in, each appear
      once in `/inbox`, with the actor and the thread title linking to the
      triggering post.
- [ ] An `@mention` in a post notifies exactly the users the renderer would
      link, and nobody else (no e-mails, no code spans, no link labels).
- [ ] The navbar badge counts unread *threads*, does **not** clear when I
      merely open `/inbox`, and never shows a dead thread.
- [ ] “Mark all read” clears every highlight and the badge in one click, and
      is absent when there is nothing unread.
- [ ] Opening a thread clears its inbox entry.
- [ ] An expired thread's inbox entries disappear with it (immediately from
      the listing and the badge, by SQL filter; physically once the worker
      purges); a deleted user's do too.
- [ ] Guests get no badge and are redirected from `/inbox`.
- [ ] `go test ./... -race` green; fresh and pre-existing DBs migrate cleanly
      up and down.
