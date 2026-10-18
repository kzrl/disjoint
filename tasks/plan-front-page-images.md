# Implementation Plan: Image Strips on the Front Page (mobile-scrollable)

**Status: Implemented.** All 5 tasks done (2026-10-18).

## Overview

The front page (`/`) currently shows thread title, reactions, and a last-comment preview — no visual content. Threads whose opening post carries image attachments should show those images as a **horizontal thumbnail strip** on the row. On mobile the strip is **horizontally scrollable** (native touch scroll + scroll-snap), so users can swipe through a thread's images without opening it.

No JavaScript, no new routes, no schema change: one batched SQL query, a handler change, a template block, and CSS.

## Architecture Decisions

- **A. Only the opening post's images, images only.** The row already mirrors opening-post state (reactions) plus the latest comment. Scope = attachments of `first_post_id` with image MIME types (`image/jpeg`, `image/png`, `image/webp`, `image/gif` — the set the upload handler already accepts). Videos and files stay on the thread page.
- **B. One batched query per page, not N+1.** A new `ListFrontPageThreadImages` takes the page's `first_post_id`s (same batching pattern as `ListReactionsByPostIDs` in `front.go`) and returns `thread_id, storage_path, filename` ordered by `a.id`. Skipped when the page is empty (empty `IN` list is a SQL error — same guard as reactions).
- **C. Cap per thread in Go (8 thumbnails + "+N" chip).** Per-row SQL `LIMIT` is awkward (row-number window for 30 rows gains nothing); the handler truncates at 8 and records `MoreCount`. Cap keeps page weight bounded: worst case 30 threads × 8 images, all `loading="lazy"` (off-screen ones don't load).
- **D. Thumbnails link to the thread, not the raw image.** Consistent with the title link; keeps click behavior predictable and the template trivial. (Full-size view + lightbox is a possible follow-up, not this plan.)
- **E. Serve full-size files as thumbnails; no thumbnail generation in this plan.** The upload handler stores originals only. `object-fit: cover` crops them visually, but the full file downloads. For self-hosted scale and ≤8 lazy-loaded per row this is acceptable. **Follow-up candidate:** generate a downscaled thumbnail on upload (needs a resize library or `image/draw` scaling of the original in pure Go) — out of scope here, flagged in Open Questions.
- **F. Mobile = horizontal scroll; desktop = same strip, scrollable too.** One code path: `overflow-x: auto` + `scroll-snap-type: x proximity` on all viewports. Native `-webkit-overflow-scrolling: touch` behavior means no JS and no separate mobile markup. (Alternative considered: wrap on desktop — rejected, extra CSS and layout jump.)
- **G. No anonymity surface.** The strip carries file metadata only — no usernames, no new `JOIN users` paths. Existing anonymity tests remain the bar.

## Task List

### Task 1: sqlc query

Add to `db/query.sql` (next to `ListFrontPageThreads`):

```sql
-- name: ListFrontPageThreadImages :many
-- Front-page image strips: image attachments on the listed opening posts.
-- One round trip per page (same batching pattern as ListReactionsByPostIDs).
SELECT
    p.thread_id,
    a.id,
    a.storage_path,
    a.filename
FROM attachments a
JOIN posts p ON a.post_id = p.id
WHERE p.id IN (sqlc.slice('postIds'))
  AND (a.mime_type = 'image/jpeg' OR a.mime_type = 'image/png'
       OR a.mime_type = 'image/webp' OR a.mime_type = 'image/gif')
ORDER BY a.id ASC;
```

(`sqlc.slice('postIds')` is the pattern `ListReactionsByPostIDs` already uses with this driver; the handler must skip the call for an empty slice — same guard as reactions.)

Run `sqlc generate`.

**Acceptance criteria:**
- [x] `sqlc generate` clean; generated code compiles
- [x] `go build ./...` green

**Verification:** `go build ./...`
**Files:** `db/query.sql`, `internal/database/*` (generated)
**Scope:** Small

### Task 2: Handler — batch load + cap images in `IndexPage`

In `internal/handlers/front.go`:

1. New type:
   ```go
   type FrontPageImage struct {
       StoragePath string
       Filename    string
   }
   ```
   and add to `FrontPageThreadItem`:
   ```go
   Images    []FrontPageImage // opening-post image attachments, capped
   MoreImages int             // count beyond the cap (>0 renders the "+N" chip)
   ```
2. After the reactions batch (reusing the same `postIDs` slice and empty-page guard), call `ListFrontPageThreadImages`, group rows by `thread_id` into a `map[int64][]FrontPageImage`.
3. In the items loop: pull the slice, set `Images = first min(8, n)` and `MoreImages = n − 8`. Cap constant `frontPageImageCap = 8` next to `frontPagePageSize`.

**Acceptance criteria:**
- [x] Page with 3 image-bearing threads: each item carries its opening-post images, in attachment order
- [x] Threads with zero/no-image attachments get empty `Images`, `MoreImages = 0`
- [x] A thread with 10 opening-post images yields 8 + `MoreImages = 2`
- [x] Empty page issues no image query (guard)

**Verification:** extend `TestFrontPage` in `internal/handlers/handlers_test.go`:
- new thread + opening post with 2 image attachments (multipart, like existing attachment tests) → both storage paths present on the front-page HTML, each wrapped in a link to `/threads/{id}`
- thread with only a PDF attachment → no strip markup
- anonymity negative assertions unchanged
**Files:** `internal/handlers/front.go`, `internal/handlers/handlers_test.go`
**Scope:** Small–Medium
**Dependencies:** Task 1

### Task 3: Template strip in `index.html`

Inside `.board-main`, under the reaction bar:

```html
{{if .Images}}
  <div class="front-strip" role="list" aria-label="Images in this thread">
    {{range .Images}}
      <a class="front-strip-thumb" role="listitem" href="/threads/{{$.Row.ID}}" loading-hint>
        <img src="/uploads/{{.StoragePath}}" loading="lazy" alt="{{.Filename}}">
      </a>
    {{end}}
    {{if gt .MoreImages 0}}
      <a class="front-strip-more" href="/threads/{{.Row.ID}}">+{{.MoreImages}}</a>
    {{end}}
  </div>
{{end}}
```

(Notes: `$.Row.ID` to reach the thread id from the inner range; `loading` isn't an attribute on `<a>` — lazy goes on the `<img>`, as above. `alt` = original filename is fine; strip markup is skipped entirely when there are no images, so image-less rows are byte-identical to today.)

**Acceptance criteria:**
- [x] Image thread: strip renders, thumbs link into the thread, "+N" chip appears when capped
- [x] Image-less thread: row HTML identical to before the change (diff test or manual check)
- [x] Dark and light themes both render correctly (strip uses existing `--bulma-*` variables)

**Files:** `web/templates/index.html`
**Scope:** Small
**Dependencies:** Task 2

### Task 4: CSS — `.front-strip` in `site.css`

```css
/* --- Front-page image strips -----------------------------------------------------
   Horizontal, snap-scrollable thumbnail strip per thread row. Native touch
   scroll on mobile (no JS); same behavior on desktop. */
.front-strip {
  display: flex;
  gap: 0.5rem;
  margin-top: 0.5rem;
  overflow-x: auto;
  scroll-snap-type: x proximity;
  -webkit-overflow-scrolling: touch;
  scrollbar-width: thin;
}
.front-strip-thumb,
.front-strip-more {
  flex: 0 0 auto;
  scroll-snap-align: start;
}
.front-strip-thumb img {
  display: block;
  width: 7.5rem;      /* 120px */
  height: 4.5rem;     /* 72px  */
  object-fit: cover;
  border: 1px solid var(--bulma-border-weak);
  border-radius: var(--bulma-radius);
}
.front-strip-more {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 7.5rem;
  height: 4.5rem;
  border: 1px dashed var(--bulma-border-weak);
  border-radius: var(--bulma-radius);
  color: var(--muted);
  font-size: 0.75rem;
}
```

Mobile tweak inside the existing `@media (max-width: 768px)` block: shrink thumbs (`width: 6.25rem; height: 3.75rem;`) so two are visible per screenful — the point of the scroll.

**Acceptance criteria:**
- [x] ≤768px: strip scrolls horizontally by touch/swipe; ~2 thumbs visible; snap settles on a thumb
- [x] >768px: strip renders, still scrollable if >5–6 thumbs, no layout jump on the row
- [x] No horizontal scrollbar bleed on desktop (thin scrollbar, contained in the strip)

**Files:** `web/css/site.css`
**Scope:** Small
**Dependencies:** Task 3

### Task 5: Docs + final verification

- README feature bullet: front-page rows show a scrollable strip of the opening post's images.
- `specs/disjoint.md`: front-page description gains the strip (scope: opening-post images, cap 8, "+N", link target).

**Verification (checklist):**
- [x] `go build ./...` + `go test ./... -race`; `go vet` clean
- [x] Manual, real device or DevTools mobile emulation (375px and 768px):
  - thread with 2 images → strip visible, swipe works, taps open the thread
  - thread with 10 images → 8 thumbs + "+2" chip, chip opens the thread
  - thread with a PDF only → no strip
  - mixed page: image and image-less rows both align cleanly
- [x] Desktop 1280px: no regressions to row layout, pagination, reactions
- [x] Light + dark themes

**Files:** `README.md`, `specs/disjoint.md`
**Scope:** Small
**Dependencies:** Tasks 1–4

## Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| `sqlc.slice` placeholder rendering with modernc.org/sqlite | Low | `ListReactionsByPostIDs` already proves the pattern works end-to-end on this driver; Task 1 verification (build + existing reaction tests) confirms it |
| Full-size images served as thumbnails → heavy pages for photo-heavy threads | Med | Cap of 8 + `loading="lazy"` + 30-row page; follow-up: on-upload thumbnail generation (Open Questions) |
| Strips make rows taller, front page scrolls more | Low | 72px one-line strip; acceptable trade for discoverability |
| `alt="{{.Filename}}"` could leak something in a filename | Low | Filenames are user-chosen but not identity-bearing (no usernames in names); escape is template-default; thread page already uses the same `alt` |
| Scroll-snap jank on low-end mobile | Low | `proximity` (not `mandatory`); native scroll, zero JS |

## Open Questions
- Cap of 8 — default; trivial to tune (one constant).
- Should the strip also reflect **latest-comment** images (a photo just posted)? Out of scope (opening post only) — revisit if users want "new photo in thread" signals.
- Thumbnail generation on upload — follow-up plan if page weight becomes a complaint.
- Click target: thread vs. raw image in a new tab — chosen thread; a lightbox would be a follow-up.
