package markdown

import (
	"strings"
	"testing"
)

func TestMentionBasics(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		contains    string
		notContains []string
	}{
		{
			name:     "whole line",
			input:    "@kzrl",
			contains: fmtMention("kzrl"),
		},
		{
			name:     "after space mid-line",
			input:    "hello @kzrl world",
			contains: `hello ` + fmtMention("kzrl") + ` world`,
		},
		{
			name:     "at end of line",
			input:    "hi @kzrl",
			contains: fmtMention("kzrl"),
		},
		{
			name:     "two mentions on one line",
			input:    "@abc and @def",
			contains: fmtMention("abc") + ` and ` + fmtMention("def"),
		},
		{
			name:     "inside emphasis",
			input:    "**@kzrl**",
			contains: `<strong>` + fmtMention("kzrl") + `</strong>`,
		},
		{
			name:     "after opening parenthesis",
			input:    "(reply to @kzrl)",
			contains: fmtMention("kzrl"),
		},
		{
			name:        "stops at punctuation",
			input:       "@abc.def",
			contains:    fmtMention("abc") + `.def`,
			notContains: []string{"/users/abc.def"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := string(Render(tt.input))
			if tt.contains != "" && !strings.Contains(out, tt.contains) {
				t.Errorf("input %q:\n  want substring %q\n  got %q", tt.input, tt.contains, out)
			}
			for _, nc := range tt.notContains {
				if strings.Contains(out, nc) {
					t.Errorf("input %q:\n  want NOT to contain %q\n  got %q", tt.input, nc, out)
				}
			}
		})
	}
}

func TestMentionRejected(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "email address", input: "user@gmail.com"},
		{name: "at glued to word", input: "a@kzrl"},
		{name: "email with underscore local part", input: "my_bot@host.com"},
		{name: "inline code span", input: "use `@kzrl` here"},
		{name: "fenced code block", input: "before\n```\n@kzrl\n```\nafter"},
		{name: "escaped at", input: `reply with \@kzrl`},
		{name: "too short (2 chars)", input: "hey @ab"},
		{name: "too long (33 chars)", input: "hey @" + strings.Repeat("a", 33)},
		{name: "no alphanumeric name", input: "@---"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := string(Render(tt.input))
			if strings.Contains(out, `class="mention"`) {
				t.Errorf("input %q:\n  want no mention link\n  got %q", tt.input, out)
			}
			if strings.Contains(out, `href="/users/`) {
				t.Errorf("input %q:\n  want no user-card href\n  got %q", tt.input, out)
			}
		})
	}
}

func TestMentionLengthBoundaries(t *testing.T) {
	// Exactly 3 and exactly 32 characters are valid mentions.
	for _, n := range []int{3, 32} {
		name := strings.Repeat("a", n)
		out := string(Render("@" + name))
		if !strings.Contains(out, fmtMention(name)) {
			t.Errorf("len %d: want mention, got %q", n, out)
		}
	}
}

// TestMentionXSS verifies the parser only ever accepts [A-Za-z0-9] names, so
// the rendered href can never carry a scheme or path, and that hostile
// trailing bytes cannot break out of the anchor's attributes.
func TestMentionXSS(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		contains    string
		notContains []string
	}{
		{
			name:        "script tag after name",
			input:       "@abc<script>",
			contains:    fmtMention("abc"),
			notContains: []string{"<script>"},
		},
		{
			name:        "slash after name",
			input:       "@abc/def",
			contains:    fmtMention("abc"),
			notContains: []string{"/users/abc/def"},
		},
		{
			name:     "double quote after name",
			input:    `@abc"onmouseover=x`,
			contains: fmtMention("abc"), // exact well-formed anchor => no attribute breakout
		},
		{
			name:        "colon after name (mentions user 'javascript')",
			input:       "@javascript:alert(1)",
			contains:    fmtMention("javascript") + `:alert(1)`,
			notContains: []string{`href="javascript:`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := string(Render(tt.input))
			if !strings.Contains(out, tt.contains) {
				t.Errorf("input %q:\n  want substring %q\n  got %q", tt.input, tt.contains, out)
			}
			for _, nc := range tt.notContains {
				if strings.Contains(out, nc) {
					t.Errorf("input %q:\n  want NOT to contain %q\n  got %q", tt.input, nc, out)
				}
			}
		})
	}
}

func TestMentionInBlockContexts(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains string
	}{
		{
			name:  "blockquote",
			input: "> @kzrl said it best",
			contains: "<p>" + fmtMention("kzrl") + ` said it best</p>
</blockquote>`,
		},
		{
			name:     "heading",
			input:    "# Heading @kzrl here",
			contains: `<h1>Heading ` + fmtMention("kzrl") + ` here</h1>`,
		},
		{
			name:     "table cell",
			input:    "| note |\n|------|\n| hi @kzrl |",
			contains: `<td>hi ` + fmtMention("kzrl") + `</td>`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := string(Render(tt.input))
			if !strings.Contains(out, tt.contains) {
				t.Errorf("input %q:\n  want substring %q\n  got %q", tt.input, tt.contains, out)
			}
		})
	}
}

// TestMentionInLinkLabel ensures a '@' inside a link label does not become a
// mention: a mention renders its own <a>, and nesting it inside the link's
// anchor would produce invalid HTML.
func TestMentionInLinkLabel(t *testing.T) {
	out := string(Render("[@kzrl](/users/kzrl)"))
	if strings.Contains(out, `class="mention"`) {
		t.Errorf("want no mention inside link label, got %q", out)
	}
	if !strings.Contains(out, `<a href="/users/kzrl">@kzrl</a>`) {
		t.Errorf("want plain link, got %q", out)
	}
}

func TestMentionCodeAwareness(t *testing.T) {
	// A valid mention outside code and an identical token inside a code span
	// in the same document: only the plain-text one becomes a link.
	input := "mention @kzrl but not in `@kzrl`"
	out := string(Render(input))
	if c := strings.Count(out, `class="mention"`); c != 1 {
		t.Errorf("want exactly 1 mention, got %d in %q", c, out)
	}
	if !strings.Contains(out, `<code>@kzrl</code>`) {
		t.Errorf("want code span preserved, got %q", out)
	}
}

func fmtMention(name string) string {
	return `<a class="mention" href="/users/` + name + `" data-mention="@` + name + `">@` + name + `</a>`
}
