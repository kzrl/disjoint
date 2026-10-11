# Implementation Plan: Rework CSS on Bulma v1

## Overview
Replace the hand-rolled `web/static/style.css` (786 lines, dark-first retro-modern theme) with **Bulma v1.0.4** as the base framework, plus a small site-specific stylesheet for forum-specific layout (board/thread rows, post cards, markdown bodies, on-air strip, attachments). Templates migrate from ~90 custom classes (`btn`, `form-input`, `badge`, `forum-section`, …) to Bulma classes (`button`, `field/input/textarea`, `tag`, `box/card`, `notification`, …), and inline styles referencing the site's `var(--text-dim)`-style variables are replaced with Bulma modifier/helper classes.

## Current State
- `web/static/style.css` — single hand-written stylesheet: dark theme default, light via `prefers-color-scheme`; custom classes for buttons, forms, badges, breadcrumbs, flash messages, forum sections, rows, post cards, markdown body, attachments, avatars, on-air strip, activity list.
- 13 templates in `web/templates/` plus the HTMX `post_partial.html`; heavy use of inline `style=""` blocks referencing site CSS variables.
- `web/web.go` embeds `static/*` + `templates/*` via `//go:embed`. **No build pipeline** (no package.json, no sass, no bundler) — CSS is served as-is.
- Bulma v1.0.4 is available locally at `~/src/bulma` (precompiled `css/bulma.min.css` + `sass/` sources).
- Tests assert exact avatar markup: `avatar avatar-sm` and `avatar avatar-lg` (`internal/handlers/handlers_test.go` ~L1853, L1942, L1958).

## Bulma v1 Facts That Shape This Plan
- **Built-in light/dark themes with automatic `prefers-color-scheme` switching.** The precompiled `bulma.min.css` already contains both themes and the media query — no compile step needed to get scheme switching.
- **CSS-variable-based theming.** Colors are exposed as `--bulma-*` custom properties (e.g. `--bulma-primary`), so the site accent can be re-tinted from plain CSS without sass.
- **Class-name collisions with existing site classes:** `.container`, `.breadcrumb`, `.table`. (Site's `.btn`, `.badge`, `.form-*` do *not* collide.)
- **Bulma's default primary is turquoise**, not the current site blue (~`#3b82f6` / `#2563eb`).
- **Bulma is light-first** (default light, dark only when the OS prefers dark); the current site is dark-first (default dark, light when OS prefers light). This is a deliberate behavior flip.

## Architecture Decisions

### 1. Vendored precompiled Bulma — no build step
Copy `~/src/bulma/css/bulma.min.css` into `web/static/bulma.min.css` and vendor it (the app must not depend on `~/src/bulma` at runtime or build time). Rationale: the project has zero frontend tooling; a sass build for a self-hosted Go app is unjustified.
- Add `web/static/bulma-LICENSE` (MIT, © Jeremy Thomas) and note the version (`1.0.4`) in a comment in the copy and in the README.
- Add a `make css` target that re-copies `~/src/bulma/css/bulma.min.css` → `web/static/bulma.min.css` so upgrades are one command.
- **Fallback (only if re-tinting proves insufficient):** a `make css` sass compile with `$primary` overridden, committing the generated CSS. Bulma's SCSS `@use "bulma" with (...)` path is the escape hatch, not the default.

### 2. Default scheme: accept Bulma's light-first, dark-on-preference
Accept Bulma's native direction (light by default; dark when the browser prefers dark). Rationale: it's the framework's first-class behavior, requires zero extra CSS, and the app is a self-hosted personal tool where either default is fine. If the dark-first default is wanted later, it's a one-line `html { color-scheme: dark }`-level experiment or a forced `[data-theme]` — out of scope now. Flag this to the user as the one visible behavior change.

### 3. Re-tint primary to blue via CSS variables, no sass
In the site stylesheet: `:root { --bulma-primary: hsl(217, 91%, 60%); }` (hover/bold variants are separate `--bulma-primary-*` variables; verify in-browser that button hover still reads as blue, and if a derived shade stays teal, override that specific variable too). Accept small risk of one or two off-family shades; escalate to the sass fallback only if it looks wrong.

### 4. Layering: Bulma base → site override stylesheet
`layout.html` loads `bulma.min.css` first, then the shrunk site stylesheet (rename `style.css` → `disjoint.css`). `disjoint.css` keeps only what Bulma cannot express for this app:
- App frame: `body` flex-column full-height, `.site-header`, `.site-footer`, `.logo` (mono font, underscore accent), `.user-badge`.
- Forum rows: `.board-row` / `.thread-row` 3-column grids (or Bulma columns — see Task 4), responsive stacking at 768px.
- `.post-card` two-column grid (180px sidebar + content) and its mobile stacking.
- `.post-body` markdown rendering (paragraphs, headings, lists, blockquote, `pre`/`code`, tables) — restyled on top of Bulma's typography using `--bulma-*` variables.
- Attachments grid + image/video/file cards.
- On-air strip (`.on-air*`) and activity list (`.activity-*`).
- Avatars (`.avatar`, `.avatar-sm`, `.avatar-lg`) — **class names kept** to avoid touching the template funcs and tests (see Decision 5).
- Primary re-tint + a handful of site variable shims if any inline styles survive the template pass.

Everything else (reset, buttons, forms, tags, notifications, breadcrumbs, cards/boxes, tables, helpers) comes from Bulma and the corresponding site rules are deleted.

### 5. Avatars: keep site classes, skip Bulma's `.image`
Keep `avatar avatar-sm` / `avatar-lg` (rendered by `authorAvatar` in `internal/handlers/app.go` and asserted by tests). Bulma's `.image.is-24x24` would force changes in `app.go`, both templates, and three test assertions for zero visual gain. `.avatar` rules stay in `disjoint.css`.

### 6. Class mapping (custom → Bulma)
| Site class(es) | Bulma replacement |
|---|---|
| `.btn .btn-primary` | `.button .is-primary` |
| `.btn .btn-secondary` | `.button .is-light` (dark surface look; verify both schemes) |
| `.btn .btn-sm` | `.button .is-small` |
| `.flash-msg .error` / `.success` | `.notification .is-danger` / `.is-success` (consider `.is-light` modifier for the soft look) |
| `.badge` | `.tag` |
| `.badge-unread` | `.tag .is-warning` (orange family, keep "NEW" semantics) |
| `.badge-expires` | `.tag .is-info` |
| `.badge-never` | `.tag .is-light` |
| `.breadcrumb` (div/span) | `.breadcrumb` with proper `nav > ul > li` markup |
| `.forum-section` | `.card` (or `.box` + custom header) |
| `.section-header` / `.section-title` | `.card-header` / `.card-header-title` |
| `.board-row` / `.thread-row` | Bulma columns inside the card body (`is-8/2/2`-ish), which get mobile stacking for free — replacing the hand-rolled 768px breakpoint. If the fixed 140px/240px columns read better as a grid, keep the site grid instead; decide when building Task 4. |
| `.thread-header` | `.box` containing `.level` (title+meta left, expiry tag / new-thread button right) |
| `.form-panel` | `.box` (with margin helpers) |
| `.form-group` / `.form-label` | `.field` / `.label` |
| `.form-input` / `.form-textarea` / `.form-select` | `.input` / `.textarea` / `.select` (wrapper `div.select`) |
| `.form-hint` | `.help` |
| checkbox rows | `.field .control` + `label` (drop the inline flex styles) |
| `.auth-wrapper` | centered `.container` (or `.is-flex .justify-content-center`) + `.box` |
| `.table` (users) | `.table .is-fullwidth` (Bulma's table styles it; today it's unstyled) |
| `.text-dim`, muted/dim inline colors | `.has-text-grey` / `.is-size-7` / spacing helpers (`mt-2`, `mb-3`, …) |
| `.container` | Bulma's `.container` (accept its responsive max-widths, or cap at ~1080px in `disjoint.css` if the wider default feels loose) |

Inline `style=""` attributes across templates get removed in favor of these helpers; anything that survives (e.g. `max-width: 720px` wrappers) becomes a named class in `disjoint.css`.

### 7. HTMX partials must stay in sync
`post_partial.html` is re-injected into the thread page on reply; it shares markup with `thread.html`. Both are migrated in the same task and the partial is explicitly re-tested after an HTMX reply.

## Task List

### Phase 1: Foundation
- [ ] Task 1: Vendor Bulma. Copy `~/src/bulma/css/bulma.min.css` → `web/static/bulma.min.css`; add `web/static/bulma-LICENSE`; `make css` target; note version in README.
- [ ] Task 2: Wire up + prune. `layout.html` links `bulma.min.css` before the site sheet; rename `style.css` → `disjoint.css`; delete all rules now provided by Bulma (reset/body/a/`.container`/`.btn*`/`.breadcrumb`/`.flash-msg`/`.badge*`/form controls/`.form-panel`/`.auth-*`); keep the site-specific rules listed in Decision 4; add primary re-tint + `color-scheme` + app-frame rules. Expect a "broken but building" intermediate state — template migration in Phases 2–3 fixes the visuals.
- [ ] Task 3: Layout chrome. Migrate `layout.html` (header, nav buttons, flash messages → `.notification`, footer) and `post_partial.html` in one pass so HTMX replies render correctly from the first commit.

### Checkpoint: Foundation
- [ ] `go build ./...` + `go test ./...` pass; app boots; index page visually acceptable in light, dark, and 375px width; HTMX reply round-trip renders styled partial.

### Phase 2: Page-by-page migration (one vertical slice each: template + any `disjoint.css` follow-up)
- [ ] Task 4: Board index (`index.html`): forum sections → cards, board rows (decide columns vs grid), unread/NEW tags, on-air strip, activity list, empty states.
- [ ] Task 5: Board view (`board.html`): header → box+level, thread rows, expiry/never tags, empty state.
- [ ] Task 6: Thread view (`thread.html`): breadcrumb, thread-header box, post cards, reply form (field/label/textarea/select/help, file input, checkbox control, HTMX attrs untouched).
- [ ] Task 7: Auth pages (`login.html`, `register.html`, `setup.html`, `register_closed.html`): centered box, fields, full-width primary buttons, dim footer links.
- [ ] Task 8: New board / new thread (`new_board.html`, `new_thread.html`): forms, hints, submit/cancel button row.
- [ ] Task 9: Admin + profile (`users.html`, `invites.html`, `user_card.html`): `.table.is-fullwidth` + tag roles (Admin tag → `is-warning`/`is-primary`), profile form (bio textarea, avatar file field, remove-avatar checkbox), drop the ad-hoc inline purple Admin-badge styles in favor of one tag variant.
- [ ] Task 10: Sweep. Grep every class used in templates against defined CSS (site + Bulma); delete orphan rules in `disjoint.css`; remove all remaining inline `style=""` referencing `var(--*)`; final pass on `post_partial.html` parity.

### Checkpoint: Migration complete
- [ ] All pages verified in light + dark + mobile (≤768px): index, board, thread, login, register, setup, new board, new thread, users, invites, user card, register-closed.
- [ ] HTMX reply flow verified (partial styles match the thread page; form resets on success).
- [ ] No template references a class that is defined in neither Bulma nor `disjoint.css` (grep check).

### Phase 3: Polish + docs
- [ ] Task 11: Visual polish. Compare against pre-migration screenshots (capture before Task 2!): row hover states, focus rings, tag density, card rounding/shadows; adjust `disjoint.css` (Bulma modifiers preferred over overrides).
- [ ] Task 12: Docs. Update `specs/disjoint.md` §2 Styling line ("Bulma v1 (vendored precompiled, MIT) + small site stylesheet"); README note on `make css` for Bulma upgrades.
- [ ] Task 13: Final. Full test suite, `go vet`, byte-size sanity check of the vendored CSS, commit history clean (suggested split: vendor/wire → per-page migrations → sweep/polish).

## Risks / Watch-outs
1. **Light/dark default flip** (dark-first → light-first) is the one user-visible behavior change — confirm with the user before Task 2 (Decision 2).
2. **Primary re-tint incompleteness** — a few `--bulma-primary-*` derived shades may stay teal (button hover, link focus). Mitigation: override the specific variables; last resort is the sass fallback in Decision 1.
3. **`.container` width change** — Bulma's containers are wider (up to 1344px) than the current 1080px cap; decide in Task 2 whether to keep Bulma's or cap it.
4. **Tests** assert avatar markup strings; Decision 5 avoids touching them. If any other test snapshots HTML classes (check `internal/handlers/handlers_test.go` for `btn`/`form-`/`badge` assertions), update expectations in the same commit as the template change.
5. **Vendoring drift** — `web/static/bulma.min.css` must not be edited by hand; all customization lives in `disjoint.css`. `make css` is the only writer.
6. **Intermediate broken state** — Phase 1 leaves templates using deleted classes. Keep Phase 1 within a short time-box and land Tasks 1–3 in one PR/commit series before pausing.
7. **Bulma's minireset** changes button/input defaults globally (e.g. removes native file-input styling, normalizes `a` colors) — check file inputs on the reply/new-thread forms and the `download` link cards.
