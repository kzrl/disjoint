# Implementation Plan: Inline Emoji in Post Bodies

## Overview

Today `:joy:` is just text. The emoji collection (12 seeded unicode + every
community upload) is only reachable through the reaction picker, so custom
emoji are write-once decoration. This plan adds Slack-style inline emoji:
`:name:` in any markdown body renders as the emoji — the unicode glyph for
built-ins, an `<img>` for custom uploads — with the literal `:name:` preserved
whenever the name is unknown, deleted, or inside code.

No migration, no new routes, no per-request queries: one goldmark extension
plus a process-wide emoji cache that is refreshed on emoji create/delete.

## Goals

- `:name:` renders in post bodies, replies, HTMX reply partials, and bios —
  every surface that already goes through `markdown.Render`.
- The community's uploaded emoji become usable in prose, not just reactions.
- Unknown names (typos, deleted emoji, code spans) degrade to literal text,
  never to a broken image or a swallowed token.
- Zero added latency on the render path (cache lookup, no SQL).

## Non-goals

- Emoji autocomplete while typing `:` (the mention-autocomplete plan owns the
  typing affordance; inline emoji is a rendering feature first).
- Skin tones, shortcode aliases, or `:name:` inside reaction counts.
- Emoji in titles, usernames, or thread names.
- Any change to the emoji upload rules (PNG/GIF, ≤256 KB, ≤256×256, `[a-z0-9_]{2,64}`).

## Current-state facts that shape the design

- **One renderer, four surfaces.** `markdown.Render` is called from the
  `renderMarkdown` template func (`internal/handlers/app.go:131`), which
  `post_card.html` (thread page *and* HTMX reply partial) and `user_card.html`
  (bio) use. One extension lights up all of them.
- **The inline-parser precedent is right here.** `internal/markdown/mention.go`
  is a working `parser.InlineParser` + `renderer.NodeRenderer` pair with the
  exact guards this feature needs: `pc.IsInLinkLabel()` (no nested `<a>`), and a
  `block.PrecendingCharacter()` word-boundary check (`isWordRune`, package
  scope) so tokens glued to words or inside e-mail-like runs are left alone.
- **`markdown.Render` is a pure function** — mentions link by name with no DB
  access. Inline emoji cannot be stateless: `:name:` needs a lookup. That is
  the one architectural decision below.
- **The emoji table already carries everything needed**
  (`ListAllEmojisRow`: `Name`, `Source` unicode|custom, `Glyph`,
  `StoragePath`), and the upload name regex is `^[a-z0-9_]{2,64}$`
  (`internal/handlers/emojis.go:40`) — i.e. the token grammar is already
  defined by the uploader contract.
- **Uploads are served with `Cache-Control: public, max-age=86400`**
  (`uploads.go:195`), so inline `<img>`s reused across posts cost one fetch.
- **The reaction chips already render both shapes**: glyph span for unicode,
  `<img class="emoji-img" src="/uploads/…" alt="">` for custom
  (`reaction_bar.html`). The inline renderer reuses `.emoji-img` (1.25em,
  `object-fit: contain`, `vertical-align: middle`).
- **`NewApp` runs once per process** and has `app.queries` in hand — the
  natural cache warm-up point. Emoji create/delete are single handlers
  (`emojis.go:70`, `:134`), so invalidation has exactly two call sites.

## Architecture Decisions

### Decision 1 — Resolve at parse time; unknown names are never consumed

The inline parser returns a node **only when the name exists in the cache**.
When it does not, it returns `nil`, which tells goldmark "not my token": the
`:` and the following text flow through the normal text path and render
verbatim.

Why: the literal fallback becomes free and exact. There is no "render the
original text" branch in the renderer, no stored source slice, and no way to
double-render. It also gives the deletion semantics we want for free — remove a
custom emoji and every old post that used it falls back to `:name:` on the next
render, with no data migration and no broken `<img>`.

Trade-off: the AST depends on the cache at parse time. Irrelevant here, because
`Render` parses on every template execution — no AST is ever cached between
requests.

### Decision 2 — Process-wide cache behind `atomic.Pointer`, not a per-render lookup

The alternative is threading a lookup through `Render` → `renderMarkdown` →
every call site. Rejected: `renderMarkdown` is a template function with no
request context, and `user_card.html` (bio) would need its own query just to
render one emoji. A package-level snapshot keeps `Render`'s signature and makes
every surface work with one code path.

Concurrency: `Render` is called from many handlers concurrently, so the map is
immutable once published and swapped with `atomic.Pointer[map[string]Emoji]`
(no `RWMutex` needed; readers never block).

### Decision 3 — Refresh at startup and on every emoji write

`app.refreshEmojiCache(ctx)` loads `ListAllEmojis` and calls
`markdown.SetEmojis`. Called from `NewApp` (with `context.Background()`) and at
the end of `CreateEmojiSubmit` / `DeleteEmojiSubmit`, best-effort (a failed
refresh must not fail the write; the next process start heals it).

Single-process assumption, matching the rest of the app (SQLite, embedded
assets, one binary). No TTL, no background poller.

### Decision 4 — Unicode renders as its glyph, custom renders as an image

A built-in `:joy:` writes `😂` directly — that is the emoji; wrapping it in a
span or an image would add nothing. A custom emoji writes

```html
<img class="emoji-img emoji-inline" src="/uploads/…" alt=":name:" loading="lazy">
```

`alt=":name:"` keeps the token meaningful to screen readers instead of
announcing "image". The storage path is **escaped by hand**: goldmark renderers
write raw bytes (the mention renderer only gets away with dropping escaping
because its name is `[A-Za-z0-9]`-by-construction; a DB-sourced path is not
parser-constrained).

### Decision 5 — Token grammar mirrors the upload contract exactly

Accepted: `:`, 2–64 chars of `[a-z0-9_]`, `:`, not preceded by a word
character. Rejected: everything else. Uppercase is rejected because the upload
regex is lowercase (`:Joy:` is not an emoji, matching what the emoji page
would reject).

The left-boundary rule is the mention rule (reuse `isWordRune`), which quietly
solves the colon-dense cases: `10:30:00`, `http://x:8080`, `note:joy:here`, and
`a:b:` all stay literal because the `:` is preceded by a word character. A
token at line start, after whitespace/punctuation, or after `(` / `**` works.

### Decision 6 — `:` is a cheap trigger

Goldmark calls the parser on every `:`. The fast path is: link label? previous
char a word rune? no closing colon within 64 chars? → `nil`. That is a bounded
scan over a short line, not a regex per character, and it only happens at
render time for text that is already being parsed.

### Decision 7 — No CSS unless the baseline is visibly off

`.emoji-img` already sets 1.25em + `vertical-align: middle`, which is what the
chips use. The plan adds no stylesheet change; if the inline baseline looks
wrong in practice, the single escape hatch is
`.emoji-img.emoji-inline { vertical-align: -0.2em; }` plus `make css`.

## Code sketch

### Cache (`internal/markdown/emoji.go`, new)

```go
// Emoji is the render-time view of one row of the emojis table. The markdown
// package deliberately does not import internal/database; handlers translate.
type Emoji struct {
	Name        string
	Glyph       string // unicode source only
	StoragePath string // custom source only
	Custom      bool
}

var emojiCache atomic.Pointer[map[string]Emoji]

// SetEmojis replaces the process-wide snapshot. nil clears it (tests, and the
// failure path where the startup load failed).
func SetEmojis(list []Emoji) {
	m := make(map[string]Emoji, len(list))
	for _, e := range list {
		m[e.Name] = e
	}
	emojiCache.Store(&m)
}

func lookupEmoji(name string) (Emoji, bool) {
	m := emojiCache.Load()
	if m == nil {
		return Emoji{}, false
	}
	e, ok := (*m)[name]
	return e, ok
}
```

### Parser + renderer (`internal/markdown/emoji.go`)

```go
const (
	minEmojiNameLen = 2
	maxEmojiNameLen = 64
)

var KindEmoji = ast.NewNodeKind("Emoji")

type EmojiNode struct {
	ast.BaseInline
	Emoji Emoji
}

func (n *EmojiNode) Kind() ast.NodeKind        { return KindEmoji }
func (n *EmojiNode) Dump(b []byte, level int) { ast.DumpHelper(n, b, level, nil, nil) }

func isEmojiNameChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
}

func (p *emojiParser) Trigger() []byte { return []byte{':'} }

func (p *emojiParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	if pc.IsInLinkLabel() {
		return nil
	}
	if isWordRune(block.PrecendingCharacter()) {
		return nil
	}
	line, _ := block.PeekLine()
	end := 1
	for end < len(line) && isEmojiNameChar(line[end]) {
		end++
	}
	if end >= len(line) || line[end] != ':' {
		return nil
	}
	name := string(line[1:end])
	if len(name) < minEmojiNameLen || len(name) > maxEmojiNameLen {
		return nil
	}
	emoji, ok := lookupEmoji(name)
	if !ok {
		return nil // unknown → literal text, no broken image
	}
	block.Advance(end + 1)
	return &EmojiNode{Emoji: emoji}
}

func (r *emojiRenderer) renderEmoji(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	e := n.(*EmojiNode).Emoji
	if !e.Custom {
		w.WriteString(e.Glyph)
		return ast.WalkContinue, nil
	}
	w.WriteString(`<img class="emoji-img emoji-inline" src="/uploads/`)
	w.Write(util.EscapeHTML([]byte(e.StoragePath)))
	w.WriteString(`" alt="`)
	w.Write(util.EscapeHTML([]byte(":" + e.Name + ":")))
	w.WriteString(`" loading="lazy">`)
	return ast.WalkContinue, nil
}
```

Register in `markdown.go` beside `&mentionExt{}` (priority 500, unique trigger
so no ordering conflict). `Mentions()` keeps working unchanged: it walks for
`KindMention` only, and emoji nodes simply fall through.

### Refresh (`internal/handlers/emojis.go` or `notify.go`'s neighbour)

```go
// refreshEmojiCache publishes the current emoji set to the markdown renderer.
// Best-effort: a failed load leaves the previous snapshot in place.
func (app *App) refreshEmojiCache(ctx context.Context) {
	rows, err := app.queries.ListAllEmojis(ctx)
	if err != nil {
		return
	}
	out := make([]markdown.Emoji, 0, len(rows))
	for _, r := range rows {
		out = append(out, markdown.Emoji{
			Name: r.Name, Glyph: r.Glyph, StoragePath: r.StoragePath,
			Custom: r.Source == "custom",
		})
	}
	markdown.SetEmojis(out)
}
```

Call sites: `NewApp` (before returning the app) and the success path of
`CreateEmojiSubmit` / `DeleteEmojiSubmit` (just before the redirect).

## Out of scope (explicit)

- Autocomplete / suggestions while typing `:`.
- Rendering emoji in thread titles, edit-form previews, or the quote JS (the
  quote inserts plain `:name:` text, which re-renders correctly).
- Shortcode aliases (`:smile_cat:` → `:cat:`), skin tones, or animated APNG.
- Notification/email copy using emoji.
- Retroactive rewriting of stored bodies (there is nothing to rewrite —
  rendering is the only layer that knows about `:name:`).

## Tasks (incremental, verifiable slices)

1. **Cache + grammar + renderer.** `internal/markdown/emoji.go` with
   `Emoji`, `SetEmojis`, `lookupEmoji`, `emojiParser`, `emojiRenderer`,
   `emojiExt`; register in `md`. Unit tests in `internal/markdown/emoji_test.go`
   with `defer SetEmojis(nil)`:
   - unicode → glyph; custom → `<img class="emoji-img emoji-inline" … alt=":name:">`
   - unknown name, `:a:` (1 char, under the 2-char minimum), 65-char name, `::`, `:Joy:`, `:joy` (no close),
     `:joy::fire:` adjacent, mid-word `note:joy:here`, `10:30:00`,
     `http://x:8080/y` → all literal
   - line start / after space / after `(` / inside `**bold**` → rendered
   - code span, fenced block, link label, autolink → literal
   - blockquote, heading, table cell → rendered
   - storage path containing `"><script>` → escaped in the attribute, no
     injected tag
   - `Mentions()` unchanged when emoji cache is populated (both extensions on)
2. **Refresh wiring.** `refreshEmojiCache`, call from `NewApp` and both emoji
   write handlers. Verify `go run ./cmd/disjoint` on the dev DB renders the
   seeded `:joy:` in an existing post body; creating an emoji makes it usable
   immediately without a restart.
3. **Handler + template integration tests** (`handlers_test.go`):
   custom emoji uploaded via `POST /emojis` then used in a thread body renders
   the `<img>` on the thread page *and* in the HTMX reply partial; a custom
   emoji in a bio renders on the user card; a seeded unicode `:joy:` renders
   the glyph; unknown `:nope:` stays literal; code span stays literal; deleting
   the custom emoji (`POST /emojis/{id}/delete`) makes the same thread page fall
   back to literal `:name:` (proves cache invalidation, not just the parser).
4. **Docs + smoke.** `specs/disjoint.md` §D: new **Inline Emoji** bullet (token
   grammar, parse-time resolution/literal fallback, cache + invalidation,
   unicode-vs-image rendering, escaping); `README.md` feature bullet. Live
   smoke on a fresh instance: upload an emoji, post with `:name:` and `:joy:`,
   confirm both, delete the custom one, reload and confirm the literal token.
5. **Checkpoint.** `go test -race -count=1 ./...` green; `gofmt`/`go vet`
   clean; no new CSS (or exactly one alignment rule + `make css`).

## Risks / notes

- **Global mutable state in a pure-looking package.** `markdown` now has a
  cache; tests must reset it (`defer SetEmojis(nil)`) and the suite must keep
  not using `t.Parallel` in these packages. Document it on `SetEmojis`.
- **Parse-time resolution means the cache is part of the render contract.**
  If a handler ever caches rendered HTML, emoji edits stop being visible. No
  such cache exists today and none is planned; noted so nobody adds one.
- **No escaping for unicode glyphs.** They come from the seeded table (trusted
  literals). If built-ins ever become user-editable, the glyph needs escaping
  too — one-line change, called out because it is the same class of bug as the
  name field.
- **`alt=":name:"` is a deliberate accessibility choice**, not laziness: to a
  screen reader the token is the emoji's identity, and it also makes the
  fallback copy-pasteable.
- **Every `:` is now a parser call.** Bounded by the max name length and by the
  closing-colon requirement; text without colons is untouched. Watch it only if
  a pathological post ever shows up.
- **Refresh races are benign.** Two concurrent emoji writes publish two
  snapshots built from `ListAllEmojis`; the later store wins and both are
  complete sets.
- **Deletion is graceful by design** (Decision 1): old posts show `:name:`,
  not a 404 image. Do not "fix" that by storing images in post bodies.

## Acceptance criteria

- [ ] `:joy:` in a post renders the unicode glyph; `:custom_name:` renders the
      uploaded image at chip size with `alt=":custom_name:"`.
- [ ] The same body in a bio, a reply, and an HTMX-appended reply renders
      identically (one renderer, no per-surface special cases).
- [ ] `:nope:`, `:Joy:`, `:a:`, code spans/fences, and link labels stay literal.
- [ ] Deleting a custom emoji downgrades every old post using it to literal
      `:name:` text on the next render.
- [ ] No new query on any page render; emoji create/delete take effect
      immediately without a restart.
- [ ] `go test ./... -race` green; no migration; no new route.
