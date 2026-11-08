# Implementation Plan: Markdown Preview in the Composer

**Status: implemented.** Automated tests (including `-race`), `go vet`, `node --check`, and a live curl smoke (setup → authenticated `POST /preview` → composer markup) pass; interactive browser verification remains a manual check.

## Overview

The new-thread, reply, and edit-post forms are write-blind: authors see raw
Markdown until the post is published. This adds a **Preview** toggle to all
three composers. Opening the panel sends the current `body` and optional
`url` to `POST /preview`, which runs the **same** `markdown.Render` pipeline
and renders the **same** link-card partial as a published post. Nothing is
persisted.

The endpoint is deliberately small and read-only: no schema change, no
migration, no upload handling, no new dependency.

## Goals

- Preview Markdown exactly as it will render in a post: GFM, hard wraps,
  mentions, inline emoji, tables, code, quotes.
- Preview the optional first-class link as the same link card the post card
  renders.
- Work in all three composers: new thread, reply, edit post.
- Toggle open/closed; reopening refreshes with the current text.
- Keep the composer usable when preview fails: invalid URL and over-long body
  are shown inline, not as an HTMX 4xx that silently leaves the panel stale.
- No attachments are uploaded or previewed by this endpoint. The existing
  paste/drop chips already preview local files client-side.
- Stay within the existing CSP (`script-src 'self'`), HTMX, and server-rendered
  template architecture.

## Non-goals

- A live preview on every keystroke (the endpoint supports it; the UI does not
  wire it yet).
- Previewing attachments, video, or PDFs.
- Previewing the profile bio. That field also uses `renderMarkdown`, but it is
  a profile form rather than a post composer; add it later if wanted.
- Fetching link metadata / unfurling URLs. The link card remains a plain
  server-validated URL card.
- A client-side Markdown renderer. It could never match mentions, custom emoji,
  the link card, or the server's goldmark configuration.

## Current-state facts that shape the design

- `internal/markdown/markdown.go` exposes `Render(source string) template.HTML`.
  It uses goldmark with GFM, `@mention`, `:emoji:`, and `html.WithHardWraps()`.
  It is the single render path for posts and bios (`renderMarkdown` template
  func in `internal/handlers/app.go`).
- `cleanPostURL` in `internal/handlers/posts.go` is the existing security
  boundary for the first-class link: trimmed, max 2048 chars, absolute
  `http(s)` URL with a host. Preview must reuse it, not reimplement it.
- `web/templates/post_card.html` currently contains the link-card markup inline
  under the Markdown body. `post_card` is parsed into every page set *and* into
  the HTMX single-post partial (`postPartialTmpl` in `app.go`). The preview
  needs the identical markup, so extract it into a shared partial.
- The three composers are:
  - `web/templates/new_thread.html` — standard multipart form.
  - `web/templates/thread.html` — reply form, multipart + HTMX.
  - `web/templates/edit_post.html` — standard form.
- The reply form and new-thread form have `files` inputs. Preview must send
  only `body` and `url`; HTMX's `hx-params="body,url"` whitelist is the
  mechanism. It filters the form values after HTMX has collected them, so the
  file inputs are never encoded or read.
- HTMX is vendored (`web/static/htmx.min.js`) with
  `allowEval:false` / `allowScriptTags:false` in `layout.html`. No inline
  scripts or `hx-on` expressions may be added. Behaviour belongs in
  `web/static/disjoint.js`, which already uses delegated click handlers for
  toggles, quote insertion, pickers, etc.
- `web/static/disjoint.css` is generated. Edit `web/css/site.css`, then run
  `make css`; never hand-edit the generated file.
- Global middleware (`FirstRunAndAuth`) already gates every non-public route,
  so `POST /preview` is authenticated automatically. The handler should still
  do the local `user == nil` check used elsewhere, for defense-in-depth and
  HTMX-specific 401 behaviour.
- HTMX does not swap 4xx/5xx responses by default. A validation error returned
  as `400` would leave the preview panel showing stale content. Therefore
  **user-input errors are returned as a 200 fragment** with an inline message;
  only infrastructure failures use `http.Error`.
- Existing tests already cover the link card (`TestPostURLField`), Markdown
  rendering, mentions, emoji, and HTMX partials. The new endpoint should
  extend those, not replace them.

## Architecture Decisions

### A. One endpoint: `POST /preview`

Request body is `application/x-www-form-urlencoded` with exactly two fields:

- `body` — the raw Markdown, trimmed.
- `url` — the optional link, validated with `cleanPostURL`.

The handler:

1. Rejects an anonymous request the same way other handlers do
   (`303 /login`, or `401` + `HX-Redirect` for HTMX).
2. Applies a per-user preview rate limit. A dedicated
   `previewLimiter` (suggested `120` requests per `5 * time.Minute`) avoids
   spending the existing search/reaction budget. On limit, return a 200
   fragment with a friendly message and `Retry-After: 300`; do **not**
   redirect, because that would navigate away from the composer and lose the
   draft.
3. Caps the request body at **1 MiB** with `http.MaxBytesReader` before
   `ParseForm` (a 256 KB body plus URL-encoding overhead fits comfortably).
4. Trims `body`, rejects it if `len(body) > maxPostBodyBytes` (256 KB, the
   same limit submission enforces), and validates `url` with `cleanPostURL`.
5. Renders `preview.html` with:

```go
type PreviewView struct {
    Body  string
    Url   string
    Error string // joined user-input problems, or an empty string
}
```

No DB reads or writes. No attachment handling. No user lookup. `markdown.Render`
already handles mentions/emoji from the process-wide cache.

Response headers: `Content-Type: text/html; charset=utf-8`,
`Cache-Control: no-store` (the preview reflects private text).

### B. Extract the link card into `web/templates/link_card.html`

New partial:

```html
{{define "link_card"}}
<a class="post-url" href="{{.Url}}" target="_blank" rel="noopener noreferrer" title="{{.Url}}">
  <span class="post-url-icon" aria-hidden="true">&#x1F517;</span>
  <span class="post-url-text">{{.Url}}</span>
</a>
{{end}}
```

`post_card.html` replaces its inline block with:

```html
{{if .Post.Url}}{{template "link_card" .Post}}{{end}}
```

The partial is parsed into every page set, `postPartialTmpl`, and the new
`previewTmpl`. Both `database.ListPostsByThreadPageRow` and
`database.GetPostByIDRow` already expose `Url` (from `COALESCE(p.url, '')`),
and `PreviewView` uses the same field name. `html/template` continues to escape
the URL in both `href` and `title`.

This guarantees the preview card and the published card cannot drift.

### C. HTMX trigger lives on the panel, JS only toggles visibility

Put the request on the preview panel, not on the button:

```html
<button type="button" class="button is-small is-secondary"
        data-preview-toggle="#preview-reply"
        aria-controls="preview-reply" aria-expanded="false">Preview</button>

<div id="preview-reply" class="composer-preview is-hidden" aria-live="polite"
     hx-post="/preview"
     hx-trigger="preview"
     hx-target="this"
     hx-swap="innerHTML"
     hx-params="body,url"
     hx-sync="this:replace"></div>
```

Why this shape:

- `hx-post` on the panel means the request only fires when JS explicitly
  triggers it on open. Closing the panel never wastes a render.
- The panel is inside the form, so HTMX collects the enclosing form's values;
  `hx-params="body,url"` whitelists only the two text fields. File inputs are
  never read.
- `hx-trigger="preview"` is a custom event; it does not fire on page load.
- `hx-sync="this:replace"` makes rapid reopen/refresh clicks replace the
  in-flight request instead of racing stale responses.
- The panel is a normal `<div>`, so the mention-autocomplete dropdown anchored
  to the textarea's `.control` wrapper is unaffected. Place the panel *outside*
  `.control`.
- `aria-live="polite"` announces the rendered preview. `aria-expanded` on the
  button reflects the toggle state.

A small delegated handler in `web/static/disjoint.js`:

```js
function bindComposerPreview() {
  document.addEventListener('click', function (event) {
    var btn = event.target.closest ? event.target.closest('[data-preview-toggle]') : null;
    if (!btn) return;
    var panel = document.querySelector(btn.getAttribute('data-preview-toggle'));
    if (!panel) return;

    var opening = panel.classList.contains('is-hidden');
    panel.classList.toggle('is-hidden', !opening);
    btn.setAttribute('aria-expanded', opening ? 'true' : 'false');
    btn.textContent = opening ? 'Hide preview' : 'Preview';

    if (opening && window.htmx) window.htmx.trigger(panel, 'preview');
  });
}
```

Add it to `init()`. Keyboard activation works through the native button click.
If HTMX is unavailable the button only toggles an empty panel; HTMX is already
required for the existing reply flow, so this is not a new dependency.

### D. Preview template (`web/templates/preview.html`)

```html
{{define "preview"}}
<div class="preview-render">
  <div class="is-size-7 is-uppercase muted mb-2">Preview</div>
  {{if .Error}}
    <p class="notification is-danger is-light is-small mb-3" role="alert">{{.Error}}</p>
  {{end}}
  {{if .Body}}
    <div class="content is-medium">{{renderMarkdown .Body}}</div>
  {{end}}
  {{if .Url}}
    {{template "link_card" .}}
  {{end}}
  {{if and (not .Body) (not .Url) (not .Error)}}
    <p class="muted is-size-7 mb-0">Nothing to preview yet.</p>
  {{end}}
</div>
{{end}}
```

If the body is too long, do not render it (the error explains why). If the URL
is invalid, omit the card but still render a valid body. If both are valid,
render body then card, matching `post_card.html`.

### E. CSS

Add to `web/css/site.css` near the composer styles:

```css
.composer-preview {
  margin-top: 0.75rem;
  padding: 0.75rem;
  border: 1px solid var(--bulma-border);
  border-radius: var(--bulma-radius);
  background-color: var(--bulma-scheme-main-ter);
  min-width: 0;
  overflow-wrap: anywhere;
}

.composer-preview .content {
  margin-bottom: 0;
}

.composer-preview.htmx-request {
  opacity: 0.6;
  transition: opacity 120ms ease;
}
```

`is-hidden` is Bulma's `display:none !important`, already used by the inline
edit forms. Run `make css` to regenerate `web/static/disjoint.css`.

## Task List

### Phase 1 — Shared link card (no behaviour change)

- [x] Task 1: Extract `link_card` partial
  - Create `web/templates/link_card.html`.
  - Replace the inline link-card block in `web/templates/post_card.html`.
  - Add `templates/link_card.html` to:
    - the `pages` parse list in `NewApp`,
    - `postPartialTmpl`,
    - the new `previewTmpl` set.
  - Verify `TestPostURLField` still produces the same published markup
    (hx-post, escaped `&amp;`, verbatim URL in `title`, `rel="noopener noreferrer"`).

### Phase 2 — Endpoint + template

- [x] Task 2: Add `PreviewView`, `PreviewSubmit`, `previewLimiter`, route, template
  - New `internal/handlers/preview.go`:
    - `PreviewView{Body, Url, Error string}`.
    - `PreviewSubmit` with auth check, `allowUser(app.previewLimiter, ...)`,
      1 MiB `MaxBytesReader`, `ParseForm`, body cap, `cleanPostURL`, inline
      200 error fragment, `no-store`.
    - `renderPreview` helper using `app.previewTmpl.ExecuteTemplate(w, "preview", view)`.
  - `internal/handlers/app.go`:
    - add `previewTmpl *template.Template` and `previewLimiter *middleware.RateLimiter`,
    - initialize the limiter in `NewApp`,
    - parse `templates/link_card.html` + `templates/preview.html` into
      `previewTmpl` with the existing `funcMap`,
    - register `mux.HandleFunc("POST /preview", app.PreviewSubmit)` near the
      other POST routes.
  - `web/templates/preview.html`.
  - Tests:
    - `TestPreviewRendersMarkdownAndLinkCard`: authenticated `POST /preview`
      with `**bold**`, `@user`, `:joy:`, and `https://example.com/a?b=1&c=2`;
      assert 200, no page chrome, `<strong>`, mention pill, emoji glyph, link
      card with `&amp;` in the href, `Cache-Control: no-store`, and zero
      rows written to `posts`/`threads`.
    - `TestPreviewValidationAndAuth`: guest 303 `/login`; HTMX guest 401 +
      `HX-Redirect`; `javascript:` URL produces an inline error and no
      `href="javascript:`; body over 256 KB produces an inline error without
      echoing the payload; empty body + empty URL shows the empty state.
    - Optional: rate-limit test (119 allowed, 120th returns the inline
      "temporarily rate-limited" fragment). Skip if suite time matters.

### Phase 3 — Composer UI + JS + CSS

- [x] Task 3: Add the toggle button and panel to all three composers
  - `web/templates/new_thread.html`: panel `#preview-new-thread`, button after
    the body field.
  - `web/templates/thread.html`: panel `#preview-reply`, button after the reply
    body field, before attachments.
  - `web/templates/edit_post.html`: panel `#preview-edit`, button after the
    body field.
  - Common attributes: `data-preview-toggle`, `aria-controls`,
    `aria-expanded="false"`, `hx-post="/preview"`, `hx-trigger="preview"`,
    `hx-target="this"`, `hx-swap="innerHTML"`, `hx-params="body,url"`,
    `hx-sync="this:replace"`, `class="composer-preview is-hidden"`,
    `aria-live="polite"`.
- [x] Task 4: JS + CSS
  - Add `bindComposerPreview` to `web/static/disjoint.js` and call it in
    `init()`.
  - Add the CSS to `web/css/site.css`; run `make css`.
  - `node --check web/static/disjoint.js`.
  - Handler test `TestComposerPreviewMarkup`: GET `/threads/new`, a thread page,
    and `/posts/{id}/edit`; assert each has a toggle button with the right
    `data-preview-toggle`/`aria-controls`, and a panel with `hx-post="/preview"`,
    `hx-trigger="preview"`, and `hx-params="body,url"`.

### Phase 4 — Docs + verification

- [x] Task 5: Docs
  - README feature bullet: **Composer Preview** — preview Markdown and the
    link card before posting from new-thread, reply, and edit forms.
  - `specs/disjoint.md` §D: bullet describing the endpoint, same render
    pipeline, same link card, urlencoded `body`+`url` only, auth gate,
    rate limit, 256 KB cap, no persistence, 200 inline validation errors, and
    refresh-on-reopen.
- [x] Task 6: Full verification
  - `go test -race -count=1 ./...`
  - `gofmt -s -l` clean, `go vet ./...`
  - `node --check web/static/disjoint.js`
  - Live smoke (below).

## Test Plan

Automated:

- `TestPreviewRendersMarkdownAndLinkCard` — rendering, escaping, no writes,
  headers.
- `TestPreviewValidationAndAuth` — guest redirects, invalid URL, oversized
  body, empty state.
- `TestComposerPreviewMarkup` — all three composers carry the toggle and panel
  contract.
- Existing `TestPostURLField` — proves the extracted link-card partial did not
  change published posts.
- Existing markdown/mention/emoji tests — prove preview reuses the same
  pipeline (no new renderer).

Manual:

1. New thread: type Markdown + a link, click Preview, confirm body and link
   card match a published post; click Hide; edit the text; reopen and confirm
   it refreshes.
2. Reply: same, including `@username` and `:emoji:`; confirm the mention
   autocomplete still anchors to the textarea.
3. Edit post: preview an existing body, change it, reopen, confirm refresh.
4. Invalid link (`javascript:alert(1)`) shows an inline error and no link card;
   submitting still rejects it as before.
5. A reply with a pasted image: preview must not upload or preview the file;
   the local paste chip remains the only image preview.
6. Keyboard: Tab to Preview, Enter/Space toggles, `aria-expanded` updates.
7. Dark and light themes; mobile width; long unbroken URL/Markdown wraps.
8. Devtools: hiding the panel issues no request; opening issues exactly one
   `POST /preview` with only `body` and `url` in the payload.

## Files touched

- `internal/handlers/app.go` — preview template + limiter + route.
- `internal/handlers/preview.go` — new handler.
- `internal/handlers/handlers_test.go` (or a new `preview_test.go`) — tests.
- `web/templates/link_card.html` — new shared partial.
- `web/templates/preview.html` — new fragment.
- `web/templates/post_card.html` — use the partial.
- `web/templates/new_thread.html`, `thread.html`, `edit_post.html` — toggle UI.
- `web/static/disjoint.js` — toggle handler.
- `web/css/site.css` — preview panel styles.
- `web/static/disjoint.css` — regenerated via `make css`.
- `README.md`, `specs/disjoint.md` — docs.

No migration, no schema change, no new dependency, no data written by the
endpoint.

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Preview drifts from published rendering | Both use `markdown.Render` and the shared `link_card` partial. |
| Link-card extraction changes post output | Existing `TestPostURLField` is the regression gate. |
| File inputs are accidentally sent | `hx-params="body,url"` whitelist; manual devtools check. |
| HTMX does not swap validation errors | Handler returns 200 with an inline error fragment, never a 4xx for user input. |
| Preview abuse consumes CPU | Per-user `previewLimiter` (120/5 min), 1 MiB request cap, 256 KB body cap. |
| Stale preview after editing | Reopening the panel triggers a fresh request; `hx-sync` replaces in-flight requests. |
| Inline JS / CSP violation | All JS lives in `disjoint.js`; templates only carry `data-*` and `hx-*` attributes. |
| Long preview strings overflow the layout | `overflow-wrap: anywhere` + existing `.content` styles. |
| Mention dropdown breaks | Panel lives outside `.control`, so the existing anchor is untouched. |

## Rollback

Revert the route, templates, JS/CSS, and docs. Because the endpoint writes
nothing and there is no migration, rollback has no data consequence.
