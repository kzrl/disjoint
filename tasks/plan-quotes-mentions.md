# Implementation Plan: Quoting in Replies + @user Mentions

## Overview
Two small, orthogonal forum features on the thread page:

1. **Quoting** — a per-post "Quote" control that fills the reply box with a
   Markdown blockquote of that post (plus a "view original" permalink). The
   quote is stored in the reply's `body` and rendered by the existing
   Markdown pipeline. No schema change.
2. **@user mentions** — `@username` tokens in any Markdown-rendered body
   (post bodies and bios) render as links to the user's card.

Both are deliberately scoped to *rendering + a small client-side affordance*.
Explicitly **out of scope**: mention notifications, `@` autocomplete, and post
editing (posts are immutable in this app).

## Current-state facts that shape the design
- Posts are immutable: there is no edit/delete, so quoted text can never go
  stale — a stored blockquote always matches the original.
- `posts.user_id` / `threads.user_id` are `NOT NULL` (anonymity purged in
  migration `00006`), so every post has a real author — no "Anonymous" case to
  special-case in quotes or mentions.
- Usernames match `^[A-Za-z0-9]{3,32}$` (ASCII alnum) — a safe, self-describing
  mention token with no characters that need escaping in an `href`.
- Markdown renders through a package-level goldmark instance
  (`internal/markdown.Render`), called from the `renderMarkdown` template func.
  It is used in `thread.html`, `post_partial.html`, and `user_card.html` (bio).
- The thread page uses HTMX: new replies are appended to `#posts-container`
  via `hx-swap="beforeend"`. Any per-post UI (quote button) must also work on
  HTMX-appended cards → use event delegation, not per-element listeners.
- No CSP `script-src` is set (only `frame-ancestors`), so a small inline
  `<script>` is consistent with the existing `hx-on::after-request` handlers.
- `userLink` (app.go) already produces author links that can 404 for deleted
  users — a mention that 404s for a non-user is consistent with existing
  behavior.

## Architecture Decisions

### Decision 1 — Quoting = client-side Markdown insertion (no migration)
A "Quote" button on each post card reads the *rendered* text of that post from
the DOM and inserts a Markdown blockquote into the reply `textarea`:

```
> **AUTHOR wrote:**
> first line
> second line
>
> — [view original](/threads/{threadID}#post-{postID})
```

- Every physical line is `> `-prefixed so goldmark keeps it as one
  `<blockquote>`.
- Quoting the *rendered* text (what the user sees) matches forum convention
  (phpBB-style) and needs no access to the raw source.
- The reply body already round-trips through `renderMarkdown`, so the quote
  renders for free. No new column, query, or handler logic.
- "View original" is a real permalink (`#post-{id}` anchors already exist in
  the DOM), giving the one benefit a structured quote would provide, without
  the storage cost.

**Rejected alternative (B) — structured quote:** add `posts.quoted_post_id
INTEGER NULL REFERENCES posts(id)`, pass it in the reply form, look up +
render a first-class quote block. Considered and rejected: it costs a
migration + sqlc query + handler + render path for a benefit ("view
original") the permalink already provides. Revisit only if quote-threading
becomes a requirement. **Decision confirmed: A.**

### Decision 2 — Mentions = stateless goldmark inline parser, link-all valid tokens
Add a custom goldmark **inline parser** (trigger byte `@`) in `internal/markdown`
that matches `@([A-Za-z0-9]{3,32})` and renders:

```html
<a class="mention" href="/users/{name}" data-mention="@{name}">@{name}</a>
```

- **Left boundary:** the byte before `@` must be start-of-line or whitespace
  (or an opening punct like `(`). This excludes emails (`user@gmail.com`) and
  `@`-glued tokens. This is the one non-trivial check.
- **Right boundary:** the greedy alnum match stops at the first non-alnum
  naturally, so no extra right-side logic is needed.
- **Code-awareness for free:** goldmark parses `code`/fenced blocks as their
  own inline nodes and does not run the `@` parser on their contents, so
  `@media` inside a code span/fence is left alone. This is the main reason to
  use a goldmark parser rather than a naive string replace.
- **Link-all (stateless):** the renderer does *not* consult the DB, so it
  needs no username set threaded through `renderMarkdown`. A mention of a
  non-existent user links to a 404 user card — consistent with the existing
  `userLink` behavior for deleted users. Keeps the render path DB-free.
- **XSS-safe:** the token is constrained to `[A-Za-z0-9]{3,32}`, so the `href`
  can never carry `javascript:` or path tricks; the name is still HTML-escaped
  at render.

**Rejected alternative (B) — validate against real users:** refactor
`markdown` from a package-level `Render` to a `Renderer` instance holding a
username lookup (cached, TTL), and only link users that exist. Considered and
rejected: a renderer refactor + cache + invalidation to avoid 404s that match
the app's existing `userLink` behavior. **Decision confirmed: A.**

**Fallback if the goldmark parser proves fiddly:** a code-aware post-pass on
the rendered HTML (transform `@token` only outside `<code>`/`<pre>` regions).
Less clean; use only if the inline parser resists.

### Decision 3 — Mentions are render-only
Mentions link the username; they do **not** create notifications, badges, or
read receipts, and there is no `@` autocomplete in the reply box. Those are
separate, larger features (see Out of Scope).

### Decision 4 — Quote length: soft cap (~500 chars + ellipsis)
The JS truncates quoted text at ~500 characters, appending `…` when it
cuts. The "view original" permalink line is always kept — it's what makes a
truncated quote still useful. `body` is TEXT, so this is purely a courtesy
cap, not a hard limit. **Decision confirmed: soft cap.**

## Task List

### Phase 1 — @user mentions (foundational, self-contained, low risk)
- [ ] Task 1: goldmark mention inline parser + renderer + unit tests
- [ ] Task 2: `.mention` styling + integration verification

### Checkpoint: Mentions
- [ ] Build + full test suite green
- [ ] Manual: `@realuser` links to the card; `@nobody` links (404); `@x` in a
      code span/fence and `user@gmail.com` are NOT linked

### Phase 2 — Quoting
- [ ] Task 3: Quote button on post cards (thread + HTMX partial)
- [ ] Task 4: Quote-insertion JS (event-delegated) + format regression test
- [ ] Task 5: Quoting edge cases (attachments excluded, long-quote cap, blockquote integrity)

### Checkpoint: Quoting
- [ ] Build + full test suite green
- [ ] Manual: Quote fills a valid blockquote; submitting (HTMX + standard)
      renders it; works on HTMX-appended posts; "view original" anchor jumps

### Phase 3 — Polish
- [ ] Task 6: Docs + final end-to-end verification

### Checkpoint: Complete
- [ ] All acceptance criteria met
- [ ] Dark + light themes checked
- [ ] Ready for review

## Risks and Mitigations
| Risk | Impact | Mitigation |
|------|--------|------------|
| Goldmark inline-parser left-boundary (lookbehind) is finicky | Med | Isolate in `internal/markdown` with focused unit tests (email, code span, fence, emphasis, length 2/3/32/33); fall back to code-aware post-pass if needed |
| Quote button breaks on HTMX-appended posts | Med | Use one event-delegated listener on `#posts-container`; test with an appended partial |
| Quoted multi-line text collapses blockquotes | Med | Prefix every line with `> `; regression-test the exact string the JS emits through `renderMarkdown` |
| Long quotes bloat `body` | Low | Soft cap in JS (Q3); `body` is TEXT (no hard limit) so nothing breaks either way |
| Mentions 404 for non-users | Low | Accepted (consistent with `userLink`); opt to validate (Q2) if unwanted |
| Inline `<script>` and CSP | Low | No `script-src` set; if a CSP is added later, move JS to a static file (note in code comment) |

## Decisions (confirmed by user)
- **Q1 — Quote mechanism: (A)** client-side Markdown blockquote, no migration.
- **Q2 — Mention validation: (A)** link-all valid tokens, DB-free renderer.
- **Q3 — Quote length cap: soft** (~500 chars + `…`), permalink line always kept.
- **Out of scope:** mention notifications, `@` autocomplete, post
  editing/deletion.

## Notes for implementation
- `renderMarkdown` is used in 3 places; mentions will appear in post bodies
  and bios automatically. Confirm that's desired (bios mentioning users is a
  feature, not a bug).
- Keep the mention token display *with* the `@` (`@kzrl`) to distinguish a
  mention from a plain author link.
- The quote "view original" permalink relies on the existing `#post-{id}`
  anchors in `thread.html` / `post_partial.html` — already present.
