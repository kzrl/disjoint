package markdown

import (
	"bytes"
	"html/template"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

var md = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM, // Tables, Strikethrough, Autolinks, Task lists
		&mentionExt{}, // @username mentions (see mention.go)
	),
	goldmark.WithRendererOptions(
		html.WithHardWraps(), // Preserve line breaks
	),
)

// Render converts markdown text into safe HTML (template.HTML).
func Render(source string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(source), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(source))
	}
	return template.HTML(buf.String())
}
