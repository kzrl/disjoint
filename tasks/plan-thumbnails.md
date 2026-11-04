# Implementation Plan: Image Thumbnails

## Overview

The app serves every attachment at its original size. A front-page strip tile
is ~144 px wide and a post-card image is a 4:3 box, yet both download the full
12-megapixel phone photo; the planned image wall would make that 48 originals
per page. This plan adds a **lazily generated, on-disk thumbnail cache** served
by `/thumbs/{storage_path}?w={320|640|1280}` and switches the existing image
surfaces to it.

No migration, no schema change, no upload-path change, no background job, no
external image tooling. Full-size viewing is unchanged — the lightbox still
opens the original.

## Goals

- A 144 px tile transfers ~25 KB instead of 3 MB, with no visible quality loss
  at display size (and a 2× variant for retina).
- Existing images work immediately: generation is lazy, so the whole back
  catalogue is covered without a backfill script or a migration.
- Correct pixels: honour EXIF orientation so portrait phone photos are not
  sideways, and never crop (the post card's `object-fit: contain` contract).
- Bound the cost: a large photo cannot exhaust memory or stall the server.

## Non-goals

- Resizing avatars (5 MB, displayed at 32 px — a separate, smaller win; the
  package will be reusable, but avatars are not wired here).
- Transcoding, format conversion for compatibility (WebP output), AVIF/HEIC
  input, or animation handling.
- A CDN, `srcset` art direction, or client-side responsive logic beyond
  `srcset`/`sizes`.
- Pre-generating at upload time (see Decision 2).
- A UI for the cache (clearing, stats).

## Current-state facts that shape the design

- **Storage is flat and immutable**: `saveSingleFile` writes
  `<uuid><ext>` into `UPLOAD_DIR`, where `ext` is derived from the *sniffed*
  MIME type (`uploads.go` `allowedMIMEExtensions`). A storage path is never
  reused or overwritten, so a derivative of it is immutable too.
- **Nothing decodes images at upload.** `saveUploadedFiles` sniffs 512 bytes
  and stores the bytes; a file whose magic bytes say JPEG but whose body is
  garbage is accepted today (the test suite relies on exactly this).
  Therefore thumbnail generation **must** have a failure path, and it is
  testable with existing fixtures.
- **Uploads are capped at 50 MB** (`maxUploadSize`), and the allowed image
  types are jpeg/png/webp/gif.
- **`ServeUpload`** sanitises with `filepath.Base`, sets
  `Content-Disposition: inline` for media, `Cache-Control: public,
  max-age=86400`, then `http.ServeContent` (range + conditional support).
  The thumbnail handler copies this shape.
- **No image dependency exists**: `go.mod` has no `golang.org/x/image` and the
  build is `CGO_ENABLED=0` (`Makefile`), so ImageMagick/libvips are out and any
  resampling/decoding must be pure Go.
- **Post cards** render images in a fixed `aspect-ratio: 4/3` box with
  `object-fit: contain` (`site.css`), so a thumbnail is never cropped and the
  card layout does not depend on intrinsic dimensions.
- **The front-page strip** (`index.html`) uses `/uploads/{{.StoragePath}}` for
  ~144 px tiles; the lightbox (`disjoint.js` `bindLightbox`) loads the anchor's
  `href`, which is the original.
- **The worker** is the only thing that deletes attachment files
  (`worker.CleanupOnce` → `os.Remove(filepath.Join(uploadDir, att.StoragePath))`
  after the DB purge). Any derivative must be cleaned in the same place.
- **The image wall is planned, not built** (`tasks/plan-gallery.md`); it is a
  consumer of this feature, not a prerequisite.

## Architecture Decisions

### Decision 1 — Serve thumbnails from a new route, not `/uploads/?w=`

`GET /thumbs/{storage_path}?w=640` keeps two very different caching and
sanitisation contracts apart: originals are `max-age=86400` with
`Content-Disposition`, thumbs are immutable derivatives with no disposition
concerns. It also makes the cache key visible in logs and in the HTML, and
keeps `ServeUpload` (already security-sensitive) untouched.

### Decision 2 — Generate lazily on first request, cache on disk

Rejected: generate at upload. It adds decode CPU and latency to every post
submit (worse for multi-file posts), can fail *after* the post succeeded, and
still needs a backfill for the existing library — i.e. two code paths.

Rejected: a `attachments.thumb_path` column. It needs a migration plus a
backfill that must decode every file on disk, and it introduces a second source
of truth that can drift from the filesystem. The filesystem *is* the cache:
`Ensure()` is idempotent, and "does the file exist" is the only state.

Cost accepted: the first viewer of an image waits for one decode. `loading="lazy"`
means a gallery load fetches only viewport tiles, so this is a handful of
concurrent decodes, bounded by Decision 6.

### Decision 3 — Whitelisted widths: 320, 640, 1280 (default 640)

An open `?w=` is a memory/CPU amplifier and an unbounded cache-key space. Three
widths cover the real surfaces: 320 for strip tiles (2× of 144 px), 640 for
post-card boxes and gallery tiles, 1280 for wide retina cards and the lightbox
preview when the original is enormous. Unknown widths are a 404, not a clamp —
silent clamping hides templating mistakes.

### Decision 4 — Honour EXIF orientation; drop it afterwards

Browsers apply JPEG EXIF orientation to `<img>` by default, but the Go JPEG
decoder ignores it. Re-encoding without handling it would make portrait phone
photos **sideways in thumbnails while correct in the lightbox** — the exact
regression this feature must not introduce.

So: a small, bounds-checked reader for the JPEG `APP1/Exif` orientation tag
(1–8), applied before scaling, then re-encoded without EXIF (pixels are
already upright). This is ~100 lines and fully unit-testable with synthetic
byte slices; no dependency for it.

Scope: JPEG only. PNG/WebP/GIF carry no orientation tag in practice.

### Decision 5 — Output JPEG for opaque pixels, PNG when alpha is present; never crop

After scaling into an `image.NRGBA`, scan for any `A < 255`. If found → PNG
(transparency preserved); otherwise → JPEG quality 82. Scanning 640×480 is
~300 k byte reads, negligible, and it avoids both "all PNGs are huge" and
"white boxes where stickers were transparent".

The scale is a plain fit-to-width (`dstW = min(w, srcW)`, height by aspect);
cropping stays a CSS decision (`object-fit: contain` on cards, natural height
on the wall). An image narrower than the requested width is served as the
**original bytes** — no upscale, no needless re-encode, and it keeps small
animations intact.

**GIF is deliberately excluded** from the pipeline (serve the original). A
re-encoded first frame would silently freeze animated GIFs that animate today;
the wall showing live GIFs is a feature, and these files are already capped at
50 MB. Static GIF previews are a possible later change, not a v1 regression.

### Decision 6 — Two bounds: a source-pixel cap and a global semaphore

- `image.DecodeConfig` reads only the header, so an oversized image is rejected
  **before** allocating its pixels: `maxSourcePixels = 40_000_000` (~40 MP).
- A package-level semaphore (`cap = 2`) serialises decode+scale work across all
  requests, so worst-case transient memory is ~2 decoded sources (~300 MB),
  not "one per concurrent viewer".

Both are constants in `internal/thumbs`, documented; they are the knobs if a
deployment is memory-constrained.

No `singleflight`: two simultaneous requests for the same tile may both
generate it, but the write is atomic (temp file + rename) and the result is
identical, so the duplicate is bounded waste, not corruption. Adding a
dedupe table would be more code than the problem.

### Decision 7 — Atomic writes, immutable caching

Encode to `os.CreateTemp(thumbsDir, …)` → close → `os.Rename` to the final
name. A reader never sees a partial file, and a crash leaves a temp file (not a
corrupt tile). Because `<uuid>` paths never change and the algorithm is versioned
in the filename (`…640.v1.jpg`), the response is
`Cache-Control: public, max-age=31536000, immutable` — a year, no revalidation.

The `v1` token is the escape hatch: changing the resampler or quality bumps it,
and old files are orphaned (removed by `thumbs.Remove`, or left for a future
prune task).

### Decision 8 — On failure, serve the original through the thumb URL

If the source is undecodable, over the pixel cap, or unreadable mid-generation,
the handler serves the original file bytes with the original content type
(cached for a day, like `/uploads/`). `Ensure` reports this by returning the
original path with `derivative=false`; there is no separate error branch. An
`<img>` cannot fall back after a 404 without JavaScript, and a broken image is
worse than a heavy one for the rare outlier. The fallback is not negatively
cached, so a fixed file recovers on the next request.

Non-image attachments (`application/pdf`, video) are a 404 on `/thumbs/` —
there is nothing to generate and nothing renders them as an image.

### Decision 9 — Cache layout: one flat directory, key includes width + version

```
UPLOAD_DIR/
  <uuid>.jpg                      # original (unchanged)
  thumbs/
    <uuid>.320.v1.jpg             # or .png, depending on alpha
    <uuid>.640.v1.jpg
    <uuid>.1280.v1.png
```

Lookup tries `.jpg` then `.png` (a `stat` each) because the encoder is chosen
at generation time. Cleanup is `thumbs/<uuid>.*` — storage paths are UUIDs, so
a prefix glob cannot match a different attachment. GIF sources never get a
cache entry.

## Package (`internal/thumbs`, new)

```go
package thumbs

const (
	Version = "v1"
	// Widths is the allowlist for ?w= (default 640 in the handler).
	DefaultWidth  = 640
	maxSourcePixels = 40_000_000
	maxConcurrent   = 2
)

// Ensure returns the file to serve and whether it is the immutable cached
// derivative (true) or the original bytes (false). The original is returned
// when the source is not thumbnailable at this width: a GIF (animation is
// preserved), a source narrower than the requested width (no upscaling), or a
// source that cannot be decoded / exceeds the pixel cap. Serving something is
// always preferred to a broken <img>, so there is no error state here.
func Ensure(uploadDir, storagePath string, width int) (path string, derivative bool)

// Remove deletes every cached width/version for one attachment. Called by the
// worker when a thread (and its attachment rows) is purged.
func Remove(uploadDir, storagePath string)
```

`Ensure` internals, in order: non-thumbnailable extension (`.gif`) or
`DecodeConfig` width `<= width` → return the original path, false; stat
`.jpg`/`.png` → hit; acquire semaphore;
open source; read orientation (JPEG); `Seek(0)`; `DecodeConfig` + pixel cap;
`Seek(0)`; `Decode`; apply orientation; scale with `draw.ApproxBiLinear`;
alpha scan; encode to a temp file; `Rename`.

`ApproxBiLinear` (not `CatmullRom`) for v1: on a 12 MP source it is several times
faster and visually indistinguishable at 640 px; bumping `Version` makes a
later quality change safe.

### EXIF orientation (`internal/thumbs/orientation.go`)

- `readJPEGOrientation(r io.ReadSeeker) int`: scan JPEG segments from SOI,
  stop at SOS; find `FFE1` whose payload starts `Exif\0\0`; parse the TIFF
  header (`II`/`MM`, magic 42, IFD0 offset); walk IFD0 entries for tag `0x0112`
  (SHORT); return 1–8 or 1.
- `applyOrientation(img image.Image, o int) image.Image`: the eight standard
  cases (identity, flip H, rotate 180, flip V, transpose, rotate 90 CW,
  transverse, rotate 270 CW).
- Bounds-check every offset; any malformed input returns 1 (identity) rather
  than an error — orientation is best-effort, decoding is not.

## Handler & route (`internal/handlers/thumbs.go`, new)

```go
func (app *App) ServeThumb(w http.ResponseWriter, r *http.Request)
```

1. `name := filepath.Base(filepath.Clean(r.PathValue("filename")))`; reject
   empty, `"."`/`".."`, or a name that differs from its base (traversal).
2. Reject names whose extension is not an image (jpeg/png/webp/gif) → 404;
   video and PDF tiles do not exist. (`.gif` is an image but is not thumbnailed;
   `Ensure` hands back the original.)
3. `w` from `?w=`: absent → 640; must be 320/640/1280 → else 404.
4. `path, derivative := thumbs.Ensure(app.uploadDir, name, w)`.
5. `derivative` → `Cache-Control: public, max-age=31536000, immutable`;
   otherwise `Cache-Control: public, max-age=86400` (the original's policy).
6. `http.ServeFile(w, r, path)` — it handles HEAD/range and Content-Type by
   extension in both cases.

Route (`internal/handlers/app.go`, next to `/uploads/`):

```go
mux.HandleFunc("GET /thumbs/{filename}", app.ServeThumb)
```

## Worker change

In `worker.CleanupOnce`, alongside the existing `os.Remove` of the original:

```go
thumbs.Remove(w.uploadDir, att.StoragePath)
```

`Remove` strips the source extension and deletes every
`thumbs/<uuid>.<width>.<version>.<ext>` match — widths and algorithm versions
included.

Ordering stays "DB rows first, then files": leftover thumbs are harmless
orphans; a cache entry pointing at a deleted original is not.

## Template changes

- **Post card** (`web/templates/post_card.html`), image attachments:
  `src="/thumbs/{{.StoragePath}}?w=640"` plus
  `srcset="/thumbs/…?w=320 320w, …?w=640 640w, …?w=1280 1280w"` and
  `sizes="(max-width: 768px) 100vw, 25vw"` (the attachment grid is
  `repeat(auto-fill, minmax(13.75rem, 1fr))`, so a card image occupies roughly
  a quarter of a desktop viewport, not its full width).
- **Front-page strip** (`web/templates/index.html`): `?w=320` (tiles are ~144 px;
  320 covers 2× DPR).
- **Lightbox**: unchanged — the `<a href>` stays `/uploads/{{.StoragePath}}`, so
  clicking still gives full resolution.
- **Image wall** (when `tasks/plan-gallery.md` lands): tile `src="/thumbs/…?w=640"`
  with the same 320/640/1280 `srcset`, caption/lightbox links unchanged.
- Video/document attachments are untouched.

## Out of scope (explicit)

- Avatar resizing (reuse `thumbs` later; 5 MB avatars at 32 px are the next win).
- GIF/WebP animation, first-frame previews, or a "static preview" badge.
- WebP/AVIF output, chroma subsampling tuning, colour-profile preservation
  (the thumbnail is display-only; the original remains the reference).
- Pre-warming, cache GC for orphans, a settings UI, per-user variants.
- `srcset` for the lightbox (it always loads the original).

## Tasks (incremental, verifiable slices)

1. **Package: decode → orient → scale → encode.** Add `golang.org/x/image`
   (`draw` for scaling, `_ "golang.org/x/image/webp"` for decoding). Pin
   `golang.org/x/image@v0.41.0` — already in the module cache, so this works
   offline — then `go mod tidy`.
   Implement `Ensure`, `Remove`, `Path`/lookup, semaphore, pixel cap, versioned
   naming. Unit tests in `internal/thumbs/thumbs_test.go`:
   - aspect preserved (1000×500 → 320×160); no upscale (200×100 at 640 →
     `derivative=false`, original path); GIF → `derivative=false`;
   - opaque source → `.jpg`, transparent source → `.png` (assert the cache file
     extension and decoded type);
   - corrupt bytes → `ok=false`; unsupported mime → `ok=false`;
   - pixel cap: a crafted PNG IHDR claiming 100000×100000 is rejected **without
     decoding** (assert fast failure);
   - concurrent `Ensure` calls for the same key both succeed and leave one file;
   - `Remove` deletes all widths and versions for a path.
2. **EXIF orientation.** `readJPEGOrientation` + `applyOrientation` with a real
   synthetic JPEG (build one in-test with an APP1 payload) and a table of the
   eight orientations asserting the transformed pixel matrix on a 3×2 image
   with distinct colours; malformed/truncated EXIF → identity; both
   `II`/`MM` byte orders. Also verify a rotated thumbnail decodes to swapped
   dimensions.
3. **Handler + route + templates.** `ServeThumb`, route, post-card `srcset`,
   strip `?w=320`. Manual check on the dev DB: existing images render, tiles
   are small, lightbox still full-size; network panel shows 320/640 variants.
4. **Worker cleanup + tests.** `thumbs.Remove` in `CleanupOnce`; handler tests:
   `?w=320` returns 200 + `image/png|jpeg` + decoded 320-wide pixels and creates
   the cache file; second request succeeds (cache hit); unknown width → 404;
   missing `?w` → 640; traversal (`../`) → 404; PDF requested → 404; a fake
   (undecodable) image returns 200 with the original bytes; `Cache-Control`
   immutable; a purged thread leaves no `thumbs/` files behind.
5. **Docs + smoke.** `README.md` (media bullet: thumbnails, full size on
   click), `specs/disjoint.md` §E (route, widths, cache layout, EXIF handling,
   GIF exclusion, failure fallback, worker cleanup). Fresh-instance smoke:
   upload a portrait JPEG with EXIF orientation 6 → tile is upright and 320
   wide, lightbox is the full-size original; delete the thread → cache dir empty.
6. **Checkpoint.** `go test -race -count=1 ./...` green; `gofmt`/`go vet` clean;
   `CGO_ENABLED=0 make build` still succeeds; no migration.

## Risks / notes

- **EXIF is the subtle bug.** A thumbnail that ignores orientation looks right
  on a landscape test image and wrong on every portrait phone photo. The
  orientation transform must be unit-tested per case, not eyeballed.
- **Memory.** Decode allocates the full source (~4 bytes/px). The pixel cap and
  the 2-slot semaphore bound it; if a deployment is small, lower the cap first,
  then concurrency.
- **First-view latency.** A gallery of 48 new images decodes progressively
  (lazy loading limits it to the viewport). If it ever feels slow, the fix is
  pre-generating at upload *in addition*, not replacing the lazy path — the
  lazy path is what covers the existing library.
- **GIF stays heavy.** Deliberate (Decision 5). A 20 MB GIF animates in the
  feed and on the wall. If that becomes a problem, the change is "thumbnail the
  first frame and add a play badge", not a quality setting.
- **Quality/version coupling.** The cache filename carries `v1`; changing the
  resampler without bumping it leaves old tiles pixel-identical to the old
  algorithm forever. Make the version bump part of the same commit as any
  algorithm change.
- **Orphan thumbs** can accumulate if a thread is deleted outside the worker
  (manual DB surgery, restoring a backup that predates an attachment). They are
  inert files in one directory; a prune tool is a future nicety.
- **`object-fit: contain` means letterboxing**, so the thumbnail's own aspect
  ratio must be preserved exactly — any rounding that changes it shows up as
  a shifted crop. Keep `dstH = round(srcH * dstW / srcW)` and assert it in tests.
- **No new index, no new column, no new table.** The only persistent artefact is
  a directory of derived files that duplicates nothing authoritative.

## Acceptance criteria

- [ ] Post-card and strip images are requested from `/thumbs/…` at 320/640/1280
      with `srcset`; the lightbox still opens the original.
- [ ] Thumbnails preserve aspect ratio and EXIF orientation, and are JPEG for
      opaque images (PNG when transparency is present).
- [ ] Existing images work with no migration, backfill, or reboot step.
- [ ] Undecodable/oversized sources (and GIFs, and sources narrower than the
      requested width) serve the original instead of breaking, and no partial
      cache file is ever observable.
- [ ] Thread purge deletes the originals **and** their cached thumbs.
- [ ] `CGO_ENABLED=0 make build` succeeds; `go test ./... -race` green.
