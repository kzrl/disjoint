# Implementation Plan: Image Wall (`/gallery`)

## Overview

Every image shared in the app is currently reachable only by remembering which
thread it was in — the front page shows a capped strip of *opening-post* images,
and replies' images are buried. This plan adds a single `/gallery` page: every
image on every live thread, newest first, as one scrolling wall, with the
existing lightbox on click and a caption linking back to the post.

No migration, no schema change, no upload change, no JavaScript. It is a new
read view over data that already exists.

## Goals

- One place to browse every image the group has shared while it is still alive.
- Two clicks from a photo to its conversation (`figcaption` → post anchor).
- Full-size viewing without leaving the wall (reuse the existing lightbox).
- Cheap: one query, one template, one handler, one CSS block.

## Non-goals

- Uploading from the gallery (composing still happens in a thread; a wall with
  no context is how you get posts nobody can reply to).
- Albums, tags, search, filters by author/thread/date.
- Video and documents (the wall is images only; the strip and post cards
  already handle the rest).
- Server-side thumbnails (see Risks — the one real limitation). The
  follow-up plan is [tasks/plan-thumbnails.md](plan-thumbnails.md); once it
  lands, tiles should use `/thumbs/{path}?w=640` with a 320/640/1280 `srcset`
  instead of the `/uploads/` URLs written below.
- Infinite scroll (pagination matches the rest of the app).

## Current-state facts that shape the design

- **Image MIME allowlist is already explicit in SQL**
  (`ListFrontPageThreadImages`, `db/query.sql`): jpeg/png/webp/gif. The gallery
  query reuses exactly that predicate so "what counts as an image" has one
  definition per read path.
- **Attachments know their post, posts know their thread**
  (`attachments.post_id → posts.thread_id → threads.id`), so a single JOIN
  covers the wall and the caption.
- **Expiry is a thread predicate**: `threads.expires_at IS NULL OR
  expires_at > CURRENT_TIMESTAMP`, the same rule `ListFrontPageThreads` and
  `ThreadPage` use. The worker's purge cascades attachments (and their files),
  so an expired photo disappears from the wall for free.
- **The lightbox is already document-delegated** (`web/static/disjoint.js`,
  `bindLightbox`): any `<a class="lightbox-link" href="/uploads/…">`
  containing an `<img>` opens the dialog, with the thumbnail's `alt` copied in.
  The wall reuses it verbatim — no new JS, no new markup contract, apart from
  the `data-lightbox-group` attribute that
  [tasks/plan-lightbox-navigation.md](plan-lightbox-navigation.md) adds (put it
  on the gallery grid and the wall gets swipe/arrow browsing for free).
- **Uploads are served with `Cache-Control: public, max-age=86400`**
  (`uploads.go:195`) and paths are UUID-named, so scrolling back and forth
  costs one fetch per image.
- **Pagination is a solved pattern**: `IndexPage` parses `?page=`, uses
  `LIMIT/OFFSET`, and derives `HasNext` from `len(rows) == pageSize`;
  `index.html` has the Prev/Next block to copy.
- **The upload cap is 50 MB** (`uploads.go:17`) and the gallery serves
  originals — that is the main performance risk, addressed under Risks.
- **All routes are behind `middleware.FirstRunAndAuth`**, so `/gallery` is
  member-only with no handler-level auth code (guest requests redirect to
  `/login` before reaching it).
- **`attachments` has only `idx_attachments_post_id`**. The wall's query walks
  `attachments` in `id DESC` order (rowid) and filters; at this scale that is a
  scan of a small table, not a missing-index problem. No new index.

## Architecture Decisions

### Decision 1 — A top-level `/gallery` page, not `/?view=media`

The front page is a thread list with its own search/page state; bolting a
second rendering mode onto it means the search bar, pagination, "on the air"
strip, and reaction bars all need a "not in this view" branch. A separate
route is one template and zero conditionals. It is also linkable and has an
obvious place in the navbar.

### Decision 2 — Include reply images; the wall is not the strip

The front-page strip is deliberately opening-post-only and capped at 8. The
wall includes every image attachment on a live thread. That is the point: a
moodboard should not hide the screenshots people paste into replies.

### Decision 3 — CSS multi-column masonry, natural aspect ratios

`columns: 4 15rem` + `break-inside: avoid` lays out uncropped images at their
own aspect ratios with no stored dimensions, no layout JS, and no upload-time
work. The alternative — a uniform `grid` with `aspect-ratio` + `object-fit:
cover` — keeps strict row-major order but crops tall screenshots and photos.

Trade-off accepted: multi-column layout fills **column-major**, so "newest
first" means the newest image is the top of the first column, not the start of
a left-to-right row. For a visual wall people scan rather than read
chronologically, that is acceptable; it is documented in the template comment
so nobody "fixes" the order by adding JS masonry.

Rejected: adding `width`/`height` columns to `attachments`. Existing rows would
need a backfill that decodes every file on disk, and the row-major grid it
enables is the worse-looking option.

### Decision 4 — Tile opens the lightbox; the caption opens the post

A `<figure>` holds the lightbox link (image) and a `<figcaption>` (thread title
link + relative time). Two anchors, neither nested inside the other, both
keyboard-reachable. Clicking the photo zooms in place; clicking the title
navigates to `/threads/{id}#post-{pid}`.

### Decision 5 — 48 images per page, newest first by `attachments.id DESC`

48 fills a 4-column wall with 12 rows without asking a browser to fetch 100
originals. `created_at` has one-second granularity (`CURRENT_TIMESTAMP`), so
several images uploaded in the same second would tie; `id DESC` is the stable
"newest first" and matches insertion order.

### Decision 6 — No new query parameter for "opening posts only"

If a filter is ever wanted, it becomes a `?thread=`/`?author=` variant of
`ListGalleryImages`. Not now: a wall with a filter bar is a different product.

## Query (`db/query.sql`)

```sql
-- name: ListGalleryImages :many
-- The image wall: every image attachment on a non-expired thread, newest
-- upload first. Same MIME allowlist as the front-page strips, but reply images
-- are included on purpose (the wall is a browsing surface, not a row preview).
-- id DESC rather than created_at: uploads can share a one-second timestamp.
SELECT
    a.storage_path,
    a.filename,
    a.created_at,
    p.id        AS post_id,
    p.thread_id,
    t.title     AS thread_title
FROM attachments a
JOIN posts p   ON p.id = a.post_id
JOIN threads t ON t.id = p.thread_id
WHERE (a.mime_type = 'image/jpeg' OR a.mime_type = 'image/png'
       OR a.mime_type = 'image/webp' OR a.mime_type = 'image/gif')
  AND (t.expires_at IS NULL OR t.expires_at > CURRENT_TIMESTAMP)
ORDER BY a.id DESC
LIMIT ? OFFSET ?;
```

`make generate` (sqlc). No schema change, so no migration and nothing to add to
`db/schema.sql`.

## Handler (`internal/handlers/gallery.go`, new)

```go
const galleryPageSize = 48

// GalleryImage is one tile on the wall.
type GalleryImage struct {
	StoragePath string
	Filename    string
	ThreadID    int64
	PostID      int64
	ThreadTitle string
	Time        time.Time
}

func (app *App) GalleryPage(w http.ResponseWriter, r *http.Request)
```

- Parse `?page=` exactly like `IndexPage` (clamp to ≥ 1), query with
  `LIMIT galleryPageSize OFFSET (page-1)*size`.
- Map rows to `GalleryImage` (no snippet, no markdown — the wall shows pixels,
  not text).
- `HasNext: len(rows) == galleryPageSize`, then
  `app.render(w, r, "gallery.html", map[string]any{...}, "Gallery")`.
- No auth code: the middleware already gates every non-public route, and the
  handler has no per-user state (no read markers, no reactions).

Route (`internal/handlers/app.go`), next to the emoji block:

```go
// Image wall
mux.HandleFunc("GET /gallery", app.GalleryPage)
```

Register `"gallery.html"` in the `pages` slice.

## Template (`web/templates/gallery.html`, new)

```html
{{define "content"}}
<div>
  <section class="card forum-section my-5">
    <div class="card-header">
      <p class="card-header-title">Gallery</p>
      {{if .CurrentUser}}
        <a class="card-header-icon" href="/threads/new" title="Share something" aria-label="Start a new thread">+</a>
      {{end}}
    </div>
    {{if .Items}}
      <!-- Multi-column masonry: the browser balances columns, so unfilled
           images keep their aspect ratio and no dimensions are stored. This
           fills column-major: the newest image is the top of the first
           column. Do not "fix" the order with JS masonry. -->
      <div class="gallery-grid">
        {{range .Items}}
          <figure class="gallery-tile">
            <a class="lightbox-link" href="/uploads/{{.StoragePath}}" aria-label="View {{.Filename}} full size">
              <img src="/uploads/{{.StoragePath}}" alt="{{.Filename}}" loading="lazy" decoding="async">
            </a>
            <figcaption class="gallery-caption">
              <a href="/threads/{{.ThreadID}}#post-{{.PostID}}">{{.ThreadTitle}}</a>
              <time class="is-size-7 muted" datetime="{{isoTime .Time}}" title="{{formatTime .Time}}">{{formatRelative .Time}}</time>
            </figcaption>
          </figure>
        {{end}}
      </div>
      <!-- Prev/Next block copied from index.html (no search term to carry) -->
    {{else}}
      <div class="forum-section py-6 has-text-centered">
        <p class="muted mb-4">No photos yet. Attach an image to a post and it shows up here.</p>
        {{if .CurrentUser}}
          <a href="/threads/new" class="button is-primary">Share the first one</a>
        {{end}}
      </div>
    {{end}}
  </section>
</div>
{{end}}
```

Navbar (`web/templates/layout.html`, `navbar-start`, signed-in block): add
`<a href="/gallery" class="navbar-item">Gallery</a>` beside `Emoji`.

## CSS (`web/css/site.css`, then `make css`)

```css
/* --- Image wall ------------------------------------------------------------ */

/* Multi-column masonry over natural aspect ratios. `columns` takes a preferred
   width (15rem) and a maximum count (4), so it reflows without media queries. */
.gallery-grid {
  columns: 4 15rem;
  column-gap: 0.75rem;
  padding: 1rem 1.25rem;
}

.gallery-tile {
  break-inside: avoid;
  margin: 0 0 0.75rem;
}

.gallery-tile img {
  display: block;
  width: 100%;
  height: auto;
  border: 1px solid var(--bulma-border-weak);
  border-radius: var(--bulma-radius);
  background: var(--bulma-scheme-main-ter); /* avoids a flash of blank space */
}

.gallery-tile .lightbox-link {
  display: block;
}

.gallery-caption {
  padding: 0.375rem 0.125rem 0;
  font-size: 0.8125rem;
  overflow-wrap: anywhere;
}

.gallery-caption a {
  color: var(--bulma-text-weak);
}

.gallery-caption a:hover,
.gallery-caption a:focus {
  color: var(--bulma-primary);
}
```

## Out of scope (explicit)

- Server-side thumbnails / responsive `srcset` (see Risks).
- Uploading, editing captions, or deleting individual images.
- Video tiles, GIF autoplay policy, lightbox navigation between images.
- Filters (thread, author, date), search, infinite scroll, "random".
- Any per-user state: no favorites, no "seen", no read markers.

## Tasks (incremental, verifiable slices)

1. **Query + handler + route + template + navbar + CSS.** `ListGalleryImages`,
   `make generate`, `GalleryPage`, `GET /gallery`, `gallery.html`, navbar link,
   CSS + `make css`. Manual: open `/gallery` on the dev DB (or a seeded temp
   DB) and confirm images, captions, lightbox, pagination.
2. **Handler tests** (`handlers_test.go`, using `setupTestApp` + the existing
   upload helpers):
   - guest → 303 `/login`
   - empty DB → empty state, no grid
   - image attachments from a thread render; **reply** images render (the
     difference from the front-page strip)
   - a PDF/video attachment on the same post does **not** appear
   - expired thread's images are absent, and the worker's purge removes them
     from the wall entirely (cascade)
   - newest-first ordering (two uploads, assert tile order)
   - pagination: 49 images → page 1 has 48 + `Next`, page 2 has 1 + `Prev`,
     page 99 → empty state
   - each tile: `href="/uploads/{path}"`, `class="lightbox-link"`, `alt` =
     filename, caption `href="/threads/{tid}#post-{pid}"` and the thread title
3. **Docs + smoke.** `README.md` feature bullet; `specs/disjoint.md` §E gains a
   **Gallery** note (all live images incl. replies, 48/page, masonry, lightbox,
   expiry cascade, no thumbnails yet). Live smoke on a fresh instance: thread
   with two images (one in a reply, one a PDF) → wall shows 2 images, not the
   PDF → caption jumps to the reply → expire the thread → wall is empty.
4. **Checkpoint.** `go test -race -count=1 ./...` green; `gofmt`/`go vet`
   clean; no migration; no new JS.

## Risks / notes

- **Originals, not thumbnails.** A 12 MP phone photo is served at full size for
  a 15rem tile. `loading="lazy"` + `decoding="async"` + 48/page + the 1-day
  cache keep this tolerable for a friend group, but it is the honest ceiling of
  this design. If it hurts, the follow-up is `GET /uploads/{name}?w=640`
  backed by `golang.org/x/image/draw` (pure Go, CGO-free) plus an on-disk
  derivative cache — a real feature, not a tweak. Do not "solve" it by cropping
  with `object-fit` and calling it done.
- **Column-major order.** Newest is top-left, then down the column. Accepted and
  documented; see Decision 3.
- **Wide images dominate a column.** A panorama at 100% width makes its column
  enormous. Acceptable (the layout stays valid); revisit if it shows up.
- **No index added.** The query filters after an `id DESC` walk; if the wall
  ever gets slow, a partial index on `attachments(id) WHERE mime_type LIKE
  'image/%'` is the fix. Not worth it before there is a symptom.
- **The wall is a snapshot of live threads.** Expiry is enforced in SQL, so a
  thread that expires between page loads disappears on the next load — and its
  files are deleted by the worker, so no dead tiles can linger beyond that
  window.
- **Navbar grows.** `+ New Thread`, Emoji, Gallery, then admin links. On mobile
  the burger handles it; if a fifth item ever appears, group Emoji/Gallery
  under a "Community" label rather than adding a sixth.

## Acceptance criteria

- [ ] `/gallery` lists every image attachment (opening post **and** replies) on
      live threads, newest first, 48 per page.
- [ ] Non-image attachments never appear; expired threads' images disappear
      (immediately by SQL filter, permanently after the worker's purge).
- [ ] Each tile opens the existing lightbox at full size, and its caption jumps
      to the exact post in the thread.
- [ ] Guests are redirected to `/login`; the page has an empty state.
- [ ] No migration, no schema change, no new JavaScript, no new dependency.
- [ ] `go test ./... -race` green.
