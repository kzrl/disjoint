# Implementation Plan: Lightbox Navigation (swipe / arrows / keys)

## Overview

The lightbox opens exactly one image: the one you clicked. In a thread with a
dozen screenshots, viewing them means close → find the next thumbnail → click,
repeat, and the viewer loses your place each time.

This plan turns the viewer into a **gallery browser for the thread**: clicking
any image opens it with the whole thread's images in post order, a counter, and
prev/next controls driven by on-screen arrows, ←/→ (plus Home/End), and
horizontal swipe on touch. It is client-side only — no endpoints, no schema, no
server-rendered image list.

## Goals

- Browse every image on the thread without closing the viewer.
- One input model per platform: arrows/keys on desktop, swipe on touch — both
  available everywhere.
- Instant navigation: the already-cached thumbnail paints immediately, the
  full-resolution original replaces it when ready (no flash, no blank frame).
- Zero regression for single-image surfaces and for the front page.

## Non-goals

- Video/document navigation (the viewer is images only).
- Zoom/pan, rotate, download buttons, or slideshow autoplay.
- Cross-thread or cross-page galleries (the front-page strip still navigates to
  the thread).
- Server-side image lists or per-thread JSON endpoints.
- Reordering or excluding images (post order is chronological order).

## Current-state facts that shape the design

- **One `<dialog>` for the whole app**, in `layout.html`,
  `aria-label="Image preview"`, containing a close button and
  `<img class="lightbox-img">`. Native `<dialog>` supplies Escape handling and
  `::backdrop`.
- **`bindLightbox()` in `web/static/disjoint.js` is document-delegated**: a
  click on any `.lightbox-link` sets `img.src = link.href`, copies the
  thumbnail's `alt`, and calls `showModal()`. Backdrop clicks close it (the
  guard is `event.target === dialog`), and closing clears `src`.
- **Delegation already survives HTMX.** Replies appended to `#posts-container`
  bring their own `.lightbox-link`s, and the delegated handler covers them.
- **Post cards render the original in the link and the derivative in the
  image**: `<a class="lightbox-link" href="/uploads/{path}">` wrapping
  `<img src="/thumbs/{path}?w=640" srcset="…">`. The viewer therefore has both
  URLs available without a network call.
- **Images live in `#posts-container` in DOM order**, which is chronological
  post order (posts are rendered in `created_at, id` order). Nothing else on a
  thread page uses `.lightbox-link`.
- **CSS already gives the dialog `max-width: min(96vw, 90rem)` and an
  absolutely-positioned close button;** on narrow screens the close button
  moves inside the image corner (`top/right: 0.25rem`).
- **There is no JavaScript test harness.** Precedent: `node --check` for
  syntax, plus Go tests that assert the markup the script depends on.

## Architecture Decisions

### Decision 1 — The group is discovered from the DOM, not sent by the server

At open time, collect `group.querySelectorAll('.lightbox-link')` in document
order. No endpoint, no embedded JSON, no duplicated list to keep in sync with
what is rendered, and HTMX-appended posts are included automatically.

### Decision 2 — Group scope is an explicit attribute, not "all images on the page"

The clicked link's `closest('[data-lightbox-group]')` defines the group;
`#posts-container` gets the attribute. Without an ancestor, the viewer behaves
exactly as today (single image) — so adding a `.lightbox-link` somewhere new
(avatar, admin page) cannot accidentally join a thread's image set.

The planned image wall (`tasks/plan-gallery.md`) reuses the same contract by
putting the attribute on its grid; nothing in this plan needs to change then.

### Decision 3 — Rebuild the item list on every open, not on load

The list is computed each time the dialog opens, so replies added by HTMX in
the meantime are included. Rebuilding on `htmx:afterSwap` instead would mean
tracking DOM mutations for no user-visible gain — the viewer is closed while
new posts arrive.

Accepted limitation: images appended *while the dialog is open* are not in the
current group until it is reopened. Rare, and the alternative is a live
observer for a modal that is rarely open.

### Decision 4 — Thumbnail first, original when ready (no flicker)

The grid has already downloaded the 640/1280 derivative, so the viewer paints
`thumb.currentSrc` synchronously. A detached `new Image()` loads the original
in the background; on `load`, the visible `src` is swapped. Because the browser
has the bytes cached by then, the swap is a paint, not a reload — instant
navigation *and* full resolution, without a second layered `<img>` or an
opacity dance.

A token counter guards the swap: if the user moved on before the original
arrived, the stale load is ignored (it still warms the cache for that image).

### Decision 5 — Clamp, don't wrap

Prev is disabled on the first image and next on the last. A counter tells you
where you are; wrapping hides that and makes "keep swiping" unpredictable.

### Decision 6 — Pointer events for swipe, with `touch-action: pan-y`

One implementation covers touch and trackpad/mouse drag. `touch-action: pan-y`
is required: without it, iOS/Android hand horizontal drags to the browser as a
scroll gesture and we get `pointercancel` instead of `pointerup`. Vertical
drags stay native (the dialog can still scroll if an image is taller than the
viewport) and are ignored by the swipe classifier.

Classification: horizontal travel ≥ 40 px **and** `|dx| > 1.5·|dy|`. Anything
less/ambiguous is a tap, not a swipe.

### Decision 7 — Keyboard lives on the dialog; Escape stays native

A `keydown` listener on the dialog handles ←/→/Home/End. Escape is not
intercepted — `<dialog>` already closes on it and re-implementing it is how you
break platform behaviour. Modifier combinations (⌘←, meta/ctrl/alt) are
ignored so browser shortcuts keep working.

### Decision 8 — One DOM source of truth for the chrome

Counter, filename, and both arrow buttons are static elements inside the
dialog; the script toggles `hidden` and text. No innerHTML, no templating in
JS. For a single-image group the whole chrome is hidden (no dead arrows, no
"1 / 1").

## Markup changes (`web/templates/layout.html`)

```html
<dialog id="lightbox" class="lightbox" aria-label="Image viewer">
  <button type="button" class="lightbox-close" aria-label="Close viewer">&times;</button>
  <button type="button" class="lightbox-nav lightbox-prev" aria-label="Previous image" hidden>&lsaquo;</button>
  <img class="lightbox-img" alt="" draggable="false">
  <button type="button" class="lightbox-nav lightbox-next" aria-label="Next image" hidden>&rsaquo;</button>
  <div class="lightbox-meta" hidden>
    <span class="lightbox-counter" aria-live="polite"></span>
    <span class="lightbox-name"></span>
  </div>
</dialog>
```

And one attribute in `web/templates/thread.html`:

```html
<div id="posts-container" class="posts-container" data-thread-id="{{.Thread.ID}}" data-lightbox-group>
```

`draggable="false"` stops the desktop image-drag ghost from interfering with
mouse swipes.

## CSS (`web/css/site.css`, then `make css`)

```css
/* Overlay arrows sit on the image, so the dialog never widens past the
   viewport on phones. The dialog is already a containing block for absolutely
   positioned children (the modal UA stylesheet gives it position: fixed), so
   do NOT add `position: relative` here — that would override the modal
   positioning and break centering. */
.lightbox-nav {
  position: absolute;
  top: 50%;
  transform: translateY(-50%);
  z-index: 1;
  display: flex;
  align-items: center;
  justify-content: center;
  width: 2.75rem;
  height: 2.75rem;
  border-radius: 50%;
  border: 1px solid var(--bulma-border);
  background: color-mix(in srgb, var(--bulma-scheme-main-bis) 75%, transparent);
  color: var(--bulma-text-strong);
  font-size: 1.75rem;
  line-height: 1;
  cursor: pointer;
}

.lightbox-prev { left: 0.5rem; }
.lightbox-next { right: 0.5rem; }

.lightbox-nav:disabled {
  opacity: 0.35;
  cursor: default;
}

.lightbox-meta {
  display: flex;
  gap: 0.5rem;
  align-items: baseline;
  justify-content: center;
  padding-top: 0.5rem;
  font-size: 0.8125rem;
  color: var(--bulma-text-weak);
}

.lightbox-name {
  max-width: 60vw;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* [hidden] must win over the display rules above (the same trap the
   title-edit form hit with Bulma's flex helpers). */
.lightbox-nav[hidden],
.lightbox-meta[hidden] {
  display: none;
}

.lightbox-img {
  /* Let horizontal drags reach the pointer handlers; keep vertical panning
     native. Also suppress the desktop text/image drag selection. */
  touch-action: pan-y;
  user-select: none;
  -webkit-user-drag: none;
}

@media (max-width: 768px) {
  .lightbox-nav { width: 2.25rem; height: 2.25rem; font-size: 1.5rem; }
  .lightbox-prev { left: 0.25rem; }
  .lightbox-next { right: 0.25rem; }
}
```

## Script design (`bindLightbox` in `web/static/disjoint.js`, rewritten)

State: `items` (array of `{ full, thumb, alt }`), `index`, `token`, and
`dragStart` (`null` or `{x, y, id}`).

```js
function linksFor(link) {
  var group = link.closest('[data-lightbox-group]');
  var links = group
    ? Array.prototype.slice.call(group.querySelectorAll('.lightbox-link'))
    : [link];
  return links;
}

function itemFor(link) {
  var thumb = link.querySelector('img');
  return {
    full: link.getAttribute('href'),
    thumb: thumb ? (thumb.currentSrc || thumb.src) : '',
    alt: thumb ? thumb.alt : ''
  };
}

function render() {
  var item = items[index];
  if (!item) return;
  var mine = ++token;
  img.alt = item.alt;
  img.src = item.thumb || item.full;

  if (item.thumb && item.thumb !== item.full) {
    var full = new Image();
    full.onload = function () { if (mine === token) img.src = item.full; };
    full.src = item.full;   // ignored if the viewer has moved on
  }

  counter.textContent = (index + 1) + ' / ' + items.length;
  nameEl.textContent = item.alt;
  prevBtn.disabled = index === 0;
  nextBtn.disabled = index === items.length - 1;
  var multiple = items.length > 1;
  prevBtn.hidden = nextBtn.hidden = meta.hidden = !multiple;
}
```

- **Open**: `links := linksFor(clickedLink)`, `items := links.map(itemFor)`,
  `index = links.indexOf(clickedLink)` (0 if somehow absent),
  `dialog.showModal()`, `render()`.
  The existing backdrop-close, close button, and `close` → clear `src`/state
  stay as they are.
- **Navigation**: `go(delta)` clamps to `[0, items.length-1]` and calls
  `render()`. Arrow buttons and keys call it.
- **Keyboard** (`dialog` listener): `ArrowLeft`/`ArrowRight` → `go(∓1)`;
  `Home`/`End` → first/last. `preventDefault()` only for handled keys;
  ignore when `altKey || ctrlKey || metaKey`.
- **Swipe** (`dialog` pointer listeners): on `pointerdown` ignore events
  starting on `.lightbox-nav`/`.lightbox-close`; record `{x, y, id}`. On
  `pointerup` compute `dx, dy` and, when `Math.abs(dx) >= 40 &&
  Math.abs(dx) > Math.abs(dy) * 1.5`, call `go(dx < 0 ? 1 : -1)`.
  `pointercancel` clears the start.
- **No preloading.** The thumbnail paints instantly; the original loads per
  viewed image. Rapid swiping may leave a few abandoned `Image` downloads, but
  it never downloads images the user did not look at — unlike a neighbour
  preloader.

## Out of scope (explicit)

- Zoom/pan, double-tap zoom, rotate.
- Serif captions, EXIF display, download/share buttons.
- Autoplay/slideshow, thumbnails strip inside the viewer.
- Cross-thread navigation (front-page strip still links to the thread).
- Video attachments in the viewer.
- Touch-specific gestures (pinch, long-press).

## Tasks (incremental, verifiable slices)

1. **Markup + group attribute + CSS.** Update the dialog, add
   `data-lightbox-group` to `#posts-container`, add the CSS, `make css`.
   Go test: the thread page contains the group attribute, both nav buttons
   with labels, the `aria-live` counter, and `draggable="false"`; post cards
   still carry `.lightbox-link` with the `/uploads/` href and the `/thumbs/`
   `srcset` (regression guard for the current behaviour).
2. **Navigation core.** Rewrite `bindLightbox`: group collection, item
   mapping, `render`, `go`, arrow buttons, keyboard, counter, single-image
   hiding, state reset on close. Manual: 1-image thread shows no chrome;
   3-image thread shows `1 / 3`, arrows step, ends disable, Home/End jump.
3. **Swipe.** Pointer handlers + `touch-action`/drag CSS. Manual: on a real
   phone (or DevTools touch emulation) swipe left/right advances, vertical
   drag does not, a swipe starting on an arrow button does nothing.
4. **Progressive full-size.** Token-guarded thumbnail→original swap. Manual
   with network throttling: the image appears immediately at thumb quality and
   sharpens without a blank frame; navigating during the load never shows the
   wrong image.
5. **Docs + verification pass.** README feature bullet ("browse a thread's
   images with swipe or arrow keys"), `specs/disjoint.md` §D lightbox note
   (group scope, keyboard map, swipe threshold, thumb-first rendering, clamp
   behaviour). `node --check web/static/disjoint.js`. Re-run the manual matrix:
   1 / 2 / 12 images, first and last, HTMX reply then open (new image present),
   Escape, backdrop, close button, and the front page (strip unchanged).
6. **Checkpoint.** `go test -race -count=1 ./...` green; `gofmt`/`go vet`
   clean; no server, schema, or template-data changes.

## Risks / notes

- **Do not "fix" positioning on `.lightbox`.** The arrows position against the
  dialog because the modal UA stylesheet makes it `position: fixed`; changing
  that to `relative` (a tempting fix for "the arrows are relative to the
  wrong thing") breaks the modal's centering. Arrows are direct children, so
  no containing-block change is needed.
- **`touch-action` is load-bearing.** Without `pan-y`, mobile browsers claim
  horizontal drags and the handler sees `pointercancel` — the feature silently
  does nothing on the one platform it was requested for. Verify on a device,
  not only in emulation.
- **Safari `<dialog>`** requires iOS 15.4+/macOS 12.3+; the app already depends
  on it today, so this adds no new floor.
- **Rapid swiping** can start several original downloads. The token guard keeps
  the wrong one from painting, and each is cached for its own later view. If
  data use becomes a complaint, the fix is a `?w=1280` cap in the viewer, not a
  preloader.
- **`[hidden]` vs `display`** — the nav/meta rules set `display`, so the
  explicit `[hidden] { display: none }` rule is required (same class of bug as
  the title-edit form earlier).
- **Focus order**: `showModal()` focuses the close button; arrow keys still
  reach the dialog through bubbling, so no focus juggling is needed. Do not
  `preventDefault()` on keys that are not handled.
- **The image list is a snapshot at open time.** That is deliberate (Decision
  3); a live group would need a MutationObserver for no practical gain.
- **Screen-reader chatter**: the counter is `aria-live="polite"`, so each
  navigation announces "3 / 12". That is the intended behaviour for a carousel,
  but keep the label short.
- **No `alt` fallback**: if an attachment's thumbnail img has an empty alt
  (it does not today — it is the filename), the viewer would announce nothing;
  the counter still carries position.

## Acceptance criteria

- [ ] Clicking any image in a thread opens the viewer with every image in that
      thread, in post order, labelled `n / m` with the filename.
- [ ] On-screen arrows work on desktop; ←/→ and Home/End work while the viewer
      is open; first/last clamp instead of wrapping.
- [ ] Horizontal swipe advances/retreats on touch; vertical drags and swipes
      starting on a control do nothing.
- [ ] The image appears instantly at thumbnail quality and sharpens to the
      original; no blank flash, no wrong image after rapid navigation.
- [ ] Single-image threads show no arrows or counter; Escape, backdrop, and the
      close button still close the viewer.
- [ ] The front page is unchanged; HTMX-appended replies are included the next
      time the viewer opens.
- [ ] No endpoints, schema, or dependency changes; `go test ./... -race` green
      and `node --check` passes.
