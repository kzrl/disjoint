# Implementation Plan: Retro Pixel (ANSI) Avatar Generator

## Overview

Generate each user's default avatar as a small, deterministic **retro pixel-graphics** image — identicon/Space-Invader-style, colored from the classic **ANSI 16-color palette** — from their username. Users who upload a photo (existing feature, `users.avatar_path`) always win; the generator is the fallback for everyone else. Replaces the DiceBear plan: instead of a third-party style library we write a small, dependency-free Go generator (~200 lines) that matches the site's retro theme and keeps the SVG output under our full control (palette, shape, size).

## Why this instead of DiceBear

- **Zero new dependencies** (stdlib only) vs. `dicebear-go` + `dicebear/schema` + `jsonschema` + a `replace` to a local checkout plus a vendored tree just for a fallback image.
- **On-brand**: crisp pixel art in ANSI colors suits the retro-modern theme far better than flat vector faces, and the palette is a tunable constant, not a fixed style definition.
- **Even simpler security story**: the username is only *hashed* — it never appears in the SVG at all (no escaping to rely on).
- Cost: we own the "does it look good" risk. Mitigated with a local preview tool (step 4) so we iterate on taste before it ships.

## Final Decisions (agreed)

- **12×12 grid** (not 8×8): more detail, still legible at 16px.
- **3 fill colors** per avatar (dominant 60% / secondary 30% / accent 10%).
- **Transparent background**: palette is the bright half of ANSI 16 (indices 9–15) so sprites read on both dark and light themes. (Note: bright blue `#0000ff` is faint on the dark theme — acceptable; it's one of 7 equally-likely colors.)

## As-Built Notes (2026-07-18)

- **Re-rolls ("shuffle"):** avatars were originally fixed per username; on request a per-user nonce was added. Migration `00009_avatar_nonce.sql` adds `users.avatar_nonce INTEGER NOT NULL DEFAULT 0` (0 = original roll, so existing digests/renders stay stable until shuffled). The seed is now `v1 + NUL + username + NUL + nonce` (NUL separators make (username, nonce) pairs unambiguous — tested). A "Shuffle my default avatar" checkbox in the profile form POSTs `shuffle_avatar=on`; `UpdateProfileSubmit` stores a fresh `crypto/rand` nonce in [1, 2^31). The nonce travels in the avatar URL as `?v={nonce}` (author queries expose `author_avatar_nonce`) so a shuffle busts the browser's 1-day cache without changing the route or cache policy. Shuffling is independent of uploads — the nonce is always kept current so a later avatar removal resurfaces the latest roll, not a stale one. `tools/avatar-preview` gained a `-nonce` flag.
- Route is **`GET /avatars/{name}`** (no `.svg` suffix): Go's `ServeMux` requires wildcards to occupy a whole path segment, so `/avatars/{name}.svg` is not a valid pattern.
- The route is **auth-gated like the rest of the site** (the whole forum, including `/uploads`, requires a session; the middleware 303s anonymous requests to `/login`). Pages embedding the avatars are already logged-in, so the browser sends the session cookie. No middleware changes were needed.
- `TestRenderDigest` pins SHA-256 of three renderings (`alice`, `Bob`, `Ω`) so an accidental PRNG/palette/threshold change fails the build instead of silently re-rolling every avatar.
- The existing `TestUserAvatarUploads` step 7 assertion changed: after removing an uploaded avatar, the card now shows the pixel default (`/avatars/{name}`) instead of nothing.

## Design

### Generation (pure function, no I/O)

```
Render(username string) (svg string)
```

1. **Seed**: `fnv.New64a` over the exact username → `uint64`. Exact username because disjoint usernames are case-sensitive (no `ToLower` in auth). A rename changes the avatar — correct, avatar follows identity.
2. **PRNG**: `splitmix64` over the FNV hash — tiny, fast, well-distributed, deterministic across platforms/versions (no math/rand global state).
3. **Grid**: **12×12, left-right mirrored** (generate 6 columns = 72 cells, mirror). Mirroring gives the "character sprite" feel and halves the entropy needed.
4. **Cell fill**: each of the 72 half-cells filled with probability 0.45 (tune in preview). Reject all-empty and all-full grids (bounded resample) so no avatar is a blank square.
5. **Palette**: the **bright half of the ANSI 16-color palette** (indices 9–15: bright red/green/yellow/blue/magenta/cyan/white). Background is transparent (final decision). Per avatar the PRNG picks **3 distinct fill colors** (Fisher–Yates over the 7); each filled cell picks a fill with a 60/30/10 dominance bias so avatars read as one figure rather than confetti. Palette is a single `var palette = []string{…}` — the one constant the taste-tweaks will touch.
6. **Version salt**: seed input is `"disjoint-avatar-v1\x00" + username` so we can re-roll the whole family later without ambiguity.

### Rendering

- **SVG output**, `viewBox="0 0 8 8"`, `shape-rendering="crispEdges"`, `width/height` unset (CSS class sizes it). One `<rect>` per **horizontal run** of same-colored cells (run-length merge) → typically ~20–40 rects, 1–2 KB. Scales losslessly to 16/48/96px via the existing `.avatar-sm` / `.avatar-lg` CSS; crisp edges stay crisp.
- No PNG encoding, no image library, no `golang.org/x/image`.
- Render cost is microseconds — **no in-process cache needed**; HTTP caching is enough.

### Serving (same shape as the old DiceBear plan)

- New route **`GET /avatars/{name}`** (auth-gated like the rest of the site, alongside `/uploads`):
  - unknown username → 404 (route is not a free render endpoint — no arbitrary-seed CPU/abuse surface);
  - user has an upload → **302** to `/uploads/{path}` (one URL always shows the *current* avatar);
  - else → render, `Content-Type: image/svg+xml`, `Cache-Control: public, max-age=86400` (a stale copy after an upload is at worst one day and then replaced by the redirect for new requests).
- **Templates**:
  - `authorAvatar` helper in `internal/handlers/app.go` changes from `func(path string)` to `func(path string, username string)`: upload → current `/uploads/…` img; else → `<img class="avatar avatar-sm" src="/avatars/{username-escaped}.svg" alt="" loading="lazy">`. Call sites in `post_card.html` / `thread.html` add the username argument.
  - `user_card.html`: big avatar `{{if}}` gains an `{{else}}` with the `avatar-lg` variant; the edit form shows the pixel default next to the upload field ("your default avatar").

## Implementation Steps

1. **`internal/avatars` package.** `Render(username)` per the design, plus the ANSI palette constant. Unit tests (see below).
2. **Preview tool `tools/avatar-preview`.** Tiny `go run` command: takes a list of usernames (default: a fixed sample of ~24 names incl. edge cases — 1-char, unicode, uppercase, long), writes `avatar-preview.html` — a dark-background grid of every avatar at 16/48/128px. **Iterate on fill probability, color counts, palette weighting by looking at this page, not in production.**
3. **Handler + route.** `ServeAvatar` (new `internal/handlers/avatars.go`, next to `uploads.go`): `GetUserByUsername`, then redirect-or-render. Register in the `app.go` mux.
4. **Templates + helper** as above.
5. **Tests** (extend `internal/handlers/handlers_test.go`):
   - `Render` determinism: same username → byte-identical SVG; different usernames → different SVGs.
   - **Golden fixtures**: 3 fixed usernames → committed expected SVG bytes (guards against accidental PRNG/palette reshuffles landing in an unrelated commit).
   - Properties: output well-formed (parse with `encoding/xml`), only ANSI palette hexes appear, grid is left-right symmetric, no all-empty/all-full.
   - Handler: 200 + `image/svg+xml` for no-upload user; 302 to `/uploads/…` after upload; 404 unknown user.
   - Pages: user card / thread / post for a no-upload user contain the `/avatars/{name}.svg` img; existing exact-markup avatar tests (uploaded branch, ~L1853/1942/1958) still pass.
6. **Docs**: one-liner in `README.md` feature list; note the version salt in the package doc.

## Open taste knobs (decide while iterating in step 2)

- **Decided**: 12×12 grid, 3 fill colors, transparent background.
- Remaining taste knobs (tune via `tools/avatar-preview`): fill probability (45%), dominance bias (60/30/10), and whether to drop bright blue from the palette (faint on the dark theme).
- Later (not now): multiple sprite *families* (face-like vs pattern-like) picked by seed for more variety; per-user palette/style picker (a `users.avatar_style` column would absorb it without route changes).
