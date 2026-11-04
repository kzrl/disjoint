package markdown

import (
	"sync/atomic"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Emoji is the render-time view of one row of the emojis table. The markdown
// package deliberately does not import internal/database; the handlers
// translate rows into this shape.
type Emoji struct {
	Name        string
	Glyph       string // unicode source only
	StoragePath string // custom source only
	Custom      bool
}

// emojiCache holds the process-wide emoji snapshot. Render is called from many
// handlers concurrently, so the map is immutable once published and swapped
// with an atomic pointer (readers never block, no RWMutex needed).
var emojiCache atomic.Pointer[map[string]Emoji]

// SetEmojis replaces the process-wide emoji snapshot. nil (or an empty list)
// clears it, which makes every :name: render literally — used by tests and by
// the startup path when the initial load fails.
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

const (
	// The upload endpoint accepts ^[a-z0-9_]{2,64}$ names; the token grammar
	// mirrors it exactly so anything typed can only match a name that could
	// have been uploaded.
	minEmojiNameLen = 2
	maxEmojiNameLen = 64
)

// KindEmoji is a unique NodeKind for inline emoji nodes.
var KindEmoji = ast.NewNodeKind("Emoji")

// EmojiNode is an inline node representing a resolved :name: token.
type EmojiNode struct {
	ast.BaseInline
	Emoji Emoji
}

// Kind implements ast.Node.Kind.
func (n *EmojiNode) Kind() ast.NodeKind { return KindEmoji }

// Dump implements ast.Node.Dump.
func (n *EmojiNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

func isEmojiNameChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
}

// emojiParser parses :name: tokens into Emoji nodes.
type emojiParser struct{}

var _ parser.InlineParser = (*emojiParser)(nil)

// Trigger implements parser.InlineParser.Trigger.
func (p *emojiParser) Trigger() []byte { return []byte{':'} }

// Parse implements parser.InlineParser.Parse. It returns nil for anything that
// is not a known emoji, so the token is left untouched and flows through the
// normal text path verbatim. That single rule gives literal fallback for
// typos, deleted custom emoji, and over-long names without a renderer branch.
func (p *emojiParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	// Never emit an emoji inside a link label: the token is part of the label
	// text, not content of its own.
	if pc.IsInLinkLabel() {
		return nil
	}

	// Left boundary: reject a ':' immediately preceded by a word character.
	// This leaves times (10:30:00), hosts/ports (http://x:8080) and glued
	// text (note:joy:here) alone, while allowing mentions at line head, after
	// whitespace/punctuation, and inside emphasis (**[the token]**).
	if isWordRune(block.PrecendingCharacter()) {
		return nil
	}

	line, _ := block.PeekLine()
	end := 1
	for end < len(line) && isEmojiNameChar(line[end]) {
		end++
	}
	// A closing colon is required: ':joy' is not a token.
	if end >= len(line) || line[end] != ':' {
		return nil
	}

	name := string(line[1:end])
	if len(name) < minEmojiNameLen || len(name) > maxEmojiNameLen {
		return nil
	}

	emoji, ok := lookupEmoji(name)
	if !ok {
		return nil // unknown name → literal text, never a broken image
	}

	block.Advance(end + 1)
	return &EmojiNode{Emoji: emoji}
}

// emojiRenderer renders Emoji nodes. Unicode emoji are written as their glyph
// (that is the emoji); custom emoji become an image in the chip sizing.
type emojiRenderer struct{}

var _ renderer.NodeRenderer = (*emojiRenderer)(nil)

// RegisterFuncs implements renderer.NodeRenderer.RegisterFuncs.
func (r *emojiRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(KindEmoji, r.renderEmoji)
}

func (r *emojiRenderer) renderEmoji(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	e, _ := n.(*EmojiNode)
	if !e.Emoji.Custom {
		w.WriteString(e.Emoji.Glyph)
		return ast.WalkContinue, nil
	}

	// The storage path comes from the database, not from the (constrained)
	// token, so it must be escaped by hand: goldmark renderers write raw
	// bytes. alt=":name:" keeps the token meaningful to screen readers and
	// copy-pasteable.
	w.WriteString(`<img class="emoji-img emoji-inline" src="/uploads/`)
	w.Write(util.EscapeHTML([]byte(e.Emoji.StoragePath)))
	w.WriteString(`" alt="`)
	w.Write(util.EscapeHTML([]byte(":" + e.Emoji.Name + ":")))
	w.WriteString(`" loading="lazy">`)
	return ast.WalkContinue, nil
}

// emojiExt is a goldmark extension that renders :name: tokens as emoji.
type emojiExt struct{}

// Extend implements goldmark.Extender.
func (e *emojiExt) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithInlineParsers(util.Prioritized(&emojiParser{}, 500)))
	m.Renderer().AddOptions(renderer.WithNodeRenderers(util.Prioritized(&emojiRenderer{}, 500)))
}
