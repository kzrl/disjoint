package markdown

import (
	"reflect"
	"strings"
	"testing"
)

// withEmojiCache installs a small snapshot for the duration of a test: one
// unicode built-in and two custom uploads.
func withEmojiCache(t *testing.T) {
	t.Helper()
	SetEmojis([]Emoji{
		{Name: "joy", Glyph: "😂"},
		{Name: "shipit", StoragePath: "custom-shipit.png", Custom: true},
		{Name: "party_parrot", StoragePath: "custom-party.gif", Custom: true},
	})
	t.Cleanup(func() { SetEmojis(nil) })
}

func TestInlineEmojiRendering(t *testing.T) {
	withEmojiCache(t)

	customImg := func(name, path string) string {
		return `<img class="emoji-img emoji-inline" src="/uploads/` + path +
			`" alt=":` + name + `:" title=":` + name + `:" loading="lazy">`
	}

	// Built-in unicode emoji render as a span so hovering can reveal the
	// :name: token that produced the glyph.
	glyph := func(name, g string) string {
		return `<span class="emoji-inline" title=":` + name + `:">` + g + `</span>`
	}

	tests := []struct {
		name        string
		input       string
		contains    []string
		notContains []string
	}{
		{
			name:     "unicode glyph",
			input:    "feeling :joy: today",
			contains: []string{"feeling " + glyph("joy", "😂") + " today"},
		},
		{
			name:     "custom image",
			input:    "ship it :shipit:",
			contains: []string{customImg("shipit", "custom-shipit.png")},
		},
		{
			name:     "adjacent tokens",
			input:    ":shipit::party_parrot:",
			contains: []string{customImg("shipit", "custom-shipit.png"), customImg("party_parrot", "custom-party.gif")},
		},
		{
			name:     "at line start and after punctuation",
			input:    ":joy: then (:shipit:)",
			contains: []string{glyph("joy", "😂") + " then (", customImg("shipit", "custom-shipit.png")},
		},
		{
			name:     "inside emphasis",
			input:    "**before :joy: after**",
			contains: []string{"<strong>before " + glyph("joy", "😂") + " after</strong>"},
		},
		{
			name:     "blockquote and heading",
			input:    "> :joy:\n\n# :shipit:",
			contains: []string{"😂", customImg("shipit", "custom-shipit.png")},
		},
		{
			name:     "table cell",
			input:    "| col |\n| --- |\n| :joy: |",
			contains: []string{"😂"},
		},
		{
			name:        "unknown name stays literal",
			input:       "no :nope: here",
			contains:    []string{":nope:"},
			notContains: []string{"/uploads/"},
		},
		{
			name:        "one char under the minimum",
			input:       ":a:",
			contains:    []string{":a:"},
			notContains: []string{"/uploads/"},
		},
		{
			name:        "over max length",
			input:       ":" + strings.Repeat("a", 65) + ":",
			contains:    []string{":" + strings.Repeat("a", 65) + ":"},
			notContains: []string{"/uploads/"},
		},
		{
			name:        "empty token",
			input:       "::",
			contains:    []string{"::"},
			notContains: []string{"/uploads/"},
		},
		{
			name:        "uppercase is not a name",
			input:       ":Joy:",
			contains:    []string{":Joy:"},
			notContains: []string{"😂"},
		},
		{
			name:        "missing closing colon",
			input:       "trailing :joy",
			contains:    []string{":joy"},
			notContains: []string{"😂"},
		},
		{
			name:        "glued to a word",
			input:       "note:joy:here",
			contains:    []string{"note:joy:here"},
			notContains: []string{"😂"},
		},
		{
			name:        "time is not an emoji",
			input:       "meeting at 10:30:00",
			contains:    []string{"10:30:00"},
			notContains: []string{"/uploads/"},
		},
		{
			name:        "url host port is not an emoji",
			input:       "http://example.com:8080/path",
			contains:    []string{"http://example.com:8080/path"},
			notContains: []string{"/uploads/"},
		},
		{
			name:     "code span is literal",
			input:    "`:joy:`",
			contains: []string{"<code>:joy:</code>"},
		},
		{
			name:     "fence is literal",
			input:    "```\n:shipit:\n```",
			contains: []string{":shipit:"},
		},
		{
			name:        "link label is literal",
			input:       "[:joy:](/users/joy)",
			contains:    []string{">:joy:</a>"},
			notContains: []string{"😂"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := string(Render(tt.input))
			for _, want := range tt.contains {
				if !strings.Contains(out, want) {
					t.Errorf("Render(%q) = %q, want it to contain %q", tt.input, out, want)
				}
			}
			for _, unwanted := range tt.notContains {
				if strings.Contains(out, unwanted) {
					t.Errorf("Render(%q) = %q, must not contain %q", tt.input, out, unwanted)
				}
			}
		})
	}
}

// TestInlineEmojiEscaping pins the hand-rolled escaping in the renderer: the
// storage path comes from the database, so a crafted value must not break out
// of the src attribute.
func TestInlineEmojiEscaping(t *testing.T) {
	SetEmojis([]Emoji{{
		Name:        "evil",
		StoragePath: `x"><script>alert(1)</script>.png`,
		Custom:      true,
	}})
	t.Cleanup(func() { SetEmojis(nil) })

	out := string(Render(":evil:"))
	if strings.Contains(out, "<script>") {
		t.Fatalf("storage path must be escaped, got %q", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Errorf("expected escaped script tag in %q", out)
	}

	// The unicode glyph is database-sourced too and must be escaped rather
	// than written raw into the tooltip wrapper.
	SetEmojis([]Emoji{{Name: "evilglyph", Glyph: "<script>alert(2)</script>"}})
	glyphOut := string(Render(":evilglyph:"))
	if strings.Contains(glyphOut, "<script>") {
		t.Fatalf("glyph must be escaped, got %q", glyphOut)
	}
	if !strings.Contains(glyphOut, `title=":evilglyph:"`) {
		t.Errorf("glyph wrapper missing the token tooltip: %q", glyphOut)
	}
	if !strings.Contains(out, `src="/uploads/x&quot;&gt;&lt;script&gt;`) {
		t.Errorf("expected the quote to be escaped in the src attribute, got %q", out)
	}
}

func TestInlineEmojiLiteralWhenCacheEmpty(t *testing.T) {
	SetEmojis(nil)
	if out := string(Render(":joy:")); !strings.Contains(out, ":joy:") {
		t.Errorf("with no emoji loaded the token must stay literal, got %q", out)
	}
}

// TestMentionsUnaffectedByEmoji ensures the second inline extension does not
// disturb mention extraction or rendering.
func TestMentionsUnaffectedByEmoji(t *testing.T) {
	withEmojiCache(t)

	body := "**@alice** and :joy: plus `@bob`"
	if got, want := Mentions(body), []string{"alice"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Mentions(%q) = %#v, want %#v", body, got, want)
	}
	out := string(Render(body))
	if !strings.Contains(out, "😂") {
		t.Errorf("emoji should render alongside mentions, got %q", out)
	}
	if c := strings.Count(out, `class="mention"`); c != 1 {
		t.Errorf("expected exactly 1 mention pill, got %d in %q", c, out)
	}
}
