package markdown

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// KindMention is a unique NodeKind for Mention nodes.
var KindMention = ast.NewNodeKind("Mention")

// Mention is an inline node representing an @username mention.
type Mention struct {
	ast.BaseInline
	// Name is the mentioned username without the leading '@'. It is
	// guaranteed to match [A-Za-z0-9]{3,32}.
	Name string
}

// Kind implements ast.Node.Kind.
func (n *Mention) Kind() ast.NodeKind { return KindMention }

// Dump implements ast.Node.Dump.
func (n *Mention) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

const (
	minMentionLen = 3
	maxMentionLen = 32
)

func isMentionChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// isWordRune reports whether r is a "word" character (letter, digit,
// underscore) — i.e. something that can appear in an e-mail local part or a
// username. A '@' immediately preceded by one of these is not a mention.
func isWordRune(r rune) bool {
	return r == '_' ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

// mentionParser parses @username tokens into Mention nodes.
type mentionParser struct{}

var _ parser.InlineParser = (*mentionParser)(nil)

// Trigger implements parser.InlineParser.Trigger.
func (p *mentionParser) Trigger() []byte { return []byte{'@'} }

// Parse implements parser.InlineParser.Parse.
func (p *mentionParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	// Never emit a mention inside a link label: a mention renders its own
	// <a>, and nesting it inside another link would produce invalid HTML.
	if pc.IsInLinkLabel() {
		return nil
	}

	// Left boundary: reject '@' immediately preceded by a word character
	// (letter/digit/underscore). This excludes e-mail local parts
	// (user@host, my_bot@host) and '@' glued to a word, while still allowing
	// mentions at line head, after whitespace/punctuation, and inside
	// emphasis (**@user**). It also covers malformed e-mails that GFM's
	// linkify did not consume as autolinks.
	if isWordRune(block.PrecendingCharacter()) {
		return nil
	}

	line, _ := block.PeekLine()
	end := 1
	for end < len(line) && isMentionChar(line[end]) {
		end++
	}
	name := line[1:end]
	if len(name) < minMentionLen || len(name) > maxMentionLen {
		return nil
	}

	block.Advance(end)
	return &Mention{Name: string(name)}
}

// mentionRenderer renders Mention nodes to HTML.
type mentionRenderer struct{}

var _ renderer.NodeRenderer = (*mentionRenderer)(nil)

// RegisterFuncs implements renderer.NodeRenderer.RegisterFuncs.
func (r *mentionRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(KindMention, r.renderMention)
}

// renderMention writes the mention anchor. The name is constrained to
// [A-Za-z0-9]{3,32} by the parser, so it cannot carry a scheme, path, or
// quote in the href.
func (r *mentionRenderer) renderMention(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	m, _ := n.(*Mention)
	w.WriteString(`<a class="mention" href="/users/`)
	w.WriteString(m.Name)
	w.WriteString(`" data-mention="@`)
	w.WriteString(m.Name)
	w.WriteString(`">@`)
	w.WriteString(m.Name)
	w.WriteString(`</a>`)
	return ast.WalkContinue, nil
}

// mentionExt is a goldmark extension that renders @username tokens as links
// to user cards.
type mentionExt struct{}

// Extend implements goldmark.Extender.
func (e *mentionExt) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithInlineParsers(util.Prioritized(&mentionParser{}, 500)))
	m.Renderer().AddOptions(renderer.WithNodeRenderers(util.Prioritized(&mentionRenderer{}, 500)))
}
