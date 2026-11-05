> **Status:** implemented — see tasks/todo.md.

# Implementation Plan: @user Mention Autocomplete in Compose Boxes

## Overview
When a user types `@` (or `@partial`) in any compose textarea — reply box,
new-thread first post, edit-post, or bio — a small suggestion dropdown lists
matching usernames. Selecting one completes the mention. Purely a typing
affordance: rendering of `@username` tokens already exists (goldmark
`Mention` nodes) and is left untouched.

## Current-state facts that shape the design
- **No JSON API exists yet.** The app is HTML + HTMX throughout
  (`internal/handlers/app.go`, `Routes()`). This feature introduces the first
  JSON endpoint, deliberately small and self-contained.
- **All routes are behind session-cookie auth** via
  `middleware.FirstRunAndAuth` wrapping the whole mux, so a new
  `GET /api/mentions` route is protected for free. `fetch()` from same-origin
  sends the cookie automatically.
- **Usernames match `[A-Za-z0-9]{3,32}`** (see `internal/markdown/mention.go`
  and the spec). This bounds query input and means the only LIKE-wildcard
  characters that can ever arrive are none — but we escape defensively anyway.
- **SQLite `LIKE` is case-insensitive for ASCII by default**, which exactly
  matches the username charset — no `LOWER()` needed.
- **Compose textareas** (all server-rendered, all plain `<textarea>`):
  - `web/templates/thread.html` — `#reply-body` (HTMX reply form)
  - `web/templates/new_thread.html` — `#body`
  - `web/templates/edit_post.html` — `#body`
  - `web/templates/user_card.html` — `#bio` (profile form)
  HTMX-appended content never contains compose boxes, so no rebinding is
  needed; a single `DOMContentLoaded` pass (the existing `init()` pattern in
  `web/static/disjoint.js`) is sufficient.
- **`web/static/disjoint.js`** is the shared vanilla-JS file, auto-binding
  forms via a `DOMContentLoaded`/`readyState` pattern; the new code follows
  the same style (no modules, no framework).
- **Mentions of unknown users are legal** (render as 404 links), so
  autocomplete is suggestion-only — it must never block or warn on
  non-matches.

## Architecture Decisions

### Decision 1 — Small JSON endpoint, not HTMX partials
`GET /api/mentions?q=prefix` returns `{"users":[{"name":"...","avatar":"..."}]}`.
An HTMX-driven dropdown would require server-rendered list markup for every
keystroke and fight the keyboard model; a tiny JSON fetch is simpler and
isolates the app's first API behind a single, obviously-named route.

### Decision 2 — Token detection mirrors the markdown parser's boundary rule
The suggestion triggers only when the text immediately before the caret
matches a mention-shaped token: the last run of `[A-Za-z0-9]` ending at the
caret, preceded by start-of-text or a non-word character (anything not
letter/digit/`_`). This matches `mentionParser.Parse`'s left-boundary rule,
so we never suggest inside `user@host` e-mails or glued words like
`abc@def` — exactly the tokens the renderer will refuse to link.
Right side: the partial may be 0–32 chars; longer runs never trigger.

### Decision 3 — Insert completed mention plus a trailing space
Replacing `@par` with `@fullname ` (note the space) guarantees the mention
terminates: without it, the next alphanumeric keystroke would extend the
token past 32 chars or change its meaning. The space is also what users
would naturally type next.

### Decision 4 — Anchor the dropdown to the textarea, not the caret
Caret-anchored positioning requires a hidden mirror-div to measure line
geometry — disproportionate complexity for this app. The dropdown is
positioned absolutely inside the textarea's `.control` wrapper, directly
below the textarea. It is always visible, cheap, and consistent across all
four compose boxes.

### Decision 5 — New static file `web/static/mentions.js`
`disjoint.js` currently owns file-paste; keeping autocomplete in its own
vendored-style file (`<script src="/static/mentions.js">` in
`layout.html`) keeps each file's single responsibility intact.

## Backend

### 1. Query (`db/query.sql`)
```sql
-- name: ListMentionCandidates :many
-- Autocomplete candidates: case-insensitive ASCII prefix match,
-- most recently active first (presence), then alphabetical.
SELECT u.username,
       u.avatar_path,
       u.avatar_nonce
FROM users u
WHERE u.username LIKE ? ESCAPE '\'
ORDER BY
  (SELECT p.last_seen_at FROM user_presence p WHERE p.user_id = u.id) DESC NULLS LAST,
  u.username ASC
LIMIT 10;
```
(If ordering by session presence is judged too clever for v1, fall back to
plain `ORDER BY u.username ASC` — the endpoint contract is unchanged.)
Regenerate with `sqlc generate` (`make generate`).

### 2. Handler (`internal/handlers/mentions.go`, new)
`func (app *App) MentionSuggestions(w http.ResponseWriter, r *http.Request)`:
- Read `q := r.URL.Query().Get("q")`; trim; reject len > 32 with 400.
- Escape LIKE metacharacters in `q` (`\`, `%`, `_`) with the `ESCAPE '\'`
  above, then `q = escaped + "%"` and call `ListMentionCandidates`.
  (Empty `q` → top-10 list for a bare `@`.)
- Respond `Content-Type: application/json` with the user slice; avatar URL
  only when `avatar_path` is set (reuse the existing `/avatars/{name}?n={nonce}`
  URL shape from the avatar serving handler).
- No auth beyond the global middleware; no rate limiting needed (trivial
  indexed prefix scan, auth-gated).

### 3. Route (`internal/handlers/app.go`)
In `Routes()`, add to the auth-protected section:
```go
// Mention autocomplete (JSON)
mux.HandleFunc("GET /api/mentions", app.MentionSuggestions)
```

## Frontend (`web/static/mentions.js`, new)

### Token detection
On `input` and `keyup` (and `click` to reposition after caret jumps):
- Take `textarea.value.slice(0, selectionStart)`; test against
  `/@([A-Za-z0-9]{0,32})$/` and require the preceding character (if any)
  to fail `isWordChar` (`[A-Za-z0-9_]`) — the exact Decision-2 rule.
- No match → hide dropdown, cancel any in-flight fetch, return.

### Fetching
- Debounce ~120ms on the token text.
- `fetch('/api/mentions?q=' + encodeURIComponent(token), {credentials:'same-origin'})`
  with an `AbortController`; abort the previous request whenever the token
  changes or the trigger disappears (no race/stale-dropdown bugs).
- Hide dropdown on network error or 4xx (autocomplete is a nicety, never
  blocks typing).

### Dropdown UI
- Element created once per textarea, appended to the textarea's
  `position: relative` wrapper (add the class in JS rather than touching
  four templates; or add `position:relative` via a small rule in
  `web/static/disjoint.css`).
- `<ul role="listbox">` with up to 10 `<li role="option">` rows:
  avatar img (if present) + `@username` (inserted via
  `textContent`/`createElement` — never `innerHTML` of server data).
- `aria-activedescendant` on the textarea, `aria-expanded`/`aria-controls`
  wiring; the textarea gets `role="combobox"` while the list is open.
- CSS in `web/static/disjoint.css`: `.mention-suggestions` (absolute,
  bottom: 100%, small shadow, Bulma-consistent borders, max-height with
  scroll, highlighted `li.is-active`).

### Selection
- **Click** an option → insert.
- **ArrowDown / ArrowUp** while open → move highlight (wraps at ends);
  these keys are otherwise meaningless in a textarea mid-token, safe to
  consume (`preventDefault`).
- **Enter or Tab** while open → insert highlighted (or first) option.
- **Escape** → close (also matches the existing picker-Escape convention).
- **Blur** (with a short timeout so click-then-blur can fire) → close.
- Insertion: `setRangeText('@' + name + ' ', tokenStart, caret, 'end')`
  (or the splice-and-reassign fallback), then re-focus, keep caret after
  the trailing space, and fire an `input` event so any other listeners
  (e.g. paste-pipeline state) see the change. `setRangeText` preserves
  native undo; the manual fallback would need `document.execCommand('insertText')`
  or an explicit note that undo granularity degrades — prefer
  `setRangeText` (universal in all evergreen browsers).
- After insertion, close the dropdown and reset the debounce state.

### Binding
Mirror `disjoint.js`'s `init()`: on DOM-ready, loop `document.querySelectorAll('textarea')`
and bind each (all four compose boxes are plain textareas; there are no
other textareas in the app — if one ever appears and shouldn't autocomplete,
gate on a `data-mention-autocomplete` attribute instead, decided at
implementation time).

## Out of scope (explicit)
- Mention notifications (unchanged from the quotes/mentions plan).
- Autocomplete of custom emoji, links, or Markdown syntax.
- Caret-anchored dropdown positioning.
- IME-composition handling (usernames are ASCII; `compositionstart/end`
  suppression is a possible later polish, not needed for v1).
- Caching the user list client-side (network is local; a 10-row query is
  negligible).

## Tasks (incremental, verifiable slices)
1. **Query + handler + route.** Add `ListMentionCandidates`, `make generate`,
   `MentionSuggestions`, route. Verify with `curl` + session cookie:
   prefix, case-insensitivity, empty q, >32-char q, unknown user (empty
   list), JSON shape. Add a handler test alongside `handlers_test.go`
   conventions.
2. **JS skeleton: detection + fetch + dropdown render.** Manual test in the
   reply box: typing `@` shows list; typing a letter narrows; leaving the
   token hides the list; stale requests abort.
3. **Keyboard + mouse selection and insertion.** Enter/Tab/click complete the
   mention with trailing space; arrows navigate; Esc closes; caret ends up
   after the space; native undo works.
4. **Accessibility + polish.** ARIA listbox wiring, focus/blur behavior,
   dropdown CSS, avatars in rows, `no-results` behavior (list simply empty —
   no "no users" row, per Decision-2/known-404-mentions stance).
5. **Cover the remaining textareas.** New-thread, edit-post, and bio boxes
   behave identically; verify the bio's `maxlength=500` interacts sanely
   (insertion that would exceed maxlength is clamped by the browser —
   acceptable; note it in the final summary).
6. **Spec + docs.** Add a short "Mention Autocomplete" paragraph to
   `specs/disjoint.md` (endpoint contract, trigger rule, insertion
   semantics) and one line to the README feature list.

## Risks / notes
- **First JSON endpoint**: keep it strictly additive — no shared response
  envelope, no error-handling conventions invented beyond 400/404 + JSON.
- **HTMX interference**: none expected; the reply form's
  `hx-on::after-request` reset fires only on successful submits, and the
  dropdown is closed on any form reset (listen for `reset` like
  `disjoint.js` does, revoking state).
- **SQLite LIKE escape**: must pass `ESCAPE '\'` on both the SQL and the
  Go-side escaping — easy to mismatch; covered by the handler test for
  `q=%`.
- **Concurrent edits**: a user whose bio/profile changes mid-session will
  see fresh data because every keystroke re-queries — no cache to go stale.
