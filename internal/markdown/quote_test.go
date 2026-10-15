package markdown

import (
	"strings"
	"testing"
)

// TestQuoteBlockquoteFormat pins the rendering contract for client-side quote
// insertion. The Quote button JS in web/templates/thread.html builds exactly
// this shape (attribution line, every physical line > -prefixed, blank lines
// as bare >, permalink line); the renderer must keep it as ONE <blockquote>
// with the permalink anchor intact. If the JS format changes, update both.
func TestQuoteBlockquoteFormat(t *testing.T) {
	tests := []struct {
		name       string
		quote      string
		wantBlocks int // expected <blockquote> openers
		want       []string
	}{
		{
			name: "two-line quote (plan example)",
			quote: "> **kzrl wrote:**\n" +
				"> first line\n" +
				"> second line\n" +
				">\n" +
				"> — [view original](/threads/42#post-1337)",
			wantBlocks: 1,
			want: []string{
				"<strong>kzrl wrote:</strong>",
				"first line",
				"second line",
				`<a href="/threads/42#post-1337">view original</a>`,
			},
		},
		{
			name: "multi-paragraph quote stays one blockquote",
			quote: "> **kzrl wrote:**\n" +
				"> para one\n" +
				">\n" +
				"> para two\n" +
				">\n" +
				"> — [view original](/threads/7#post-5)",
			wantBlocks: 1,
			want: []string{
				// The first quoted line glues to the attribution via a hard-wrap <br>;
				// the second paragraph gets its own <p>.
				`<strong>kzrl wrote:</strong><br>
para one</p>`,
				"<p>para two</p>",
				`<a href="/threads/7#post-5">view original</a>`,
			},
		},
		{
			name: "empty quote (attribution only)",
			quote: "> **bob wrote:**\n" +
				">\n" +
				"> — [view original](/threads/1#post-2)",
			wantBlocks: 1,
			want: []string{
				"<strong>bob wrote:</strong>",
				`<a href="/threads/1#post-2">view original</a>`,
			},
		},
		{
			name: "quoted line starting with > nests but stays valid",
			quote: "> **kzrl wrote:**\n" +
				"> > inner line\n" +
				"> tail line\n" +
				">\n" +
				"> — [view original](/threads/9#post-9)",
			wantBlocks: 2, // outer + nested
			want: []string{
				"inner line",
				"tail line",
				`<a href="/threads/9#post-9">view original</a>`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := string(Render(tt.quote))
			if c := strings.Count(out, "<blockquote>"); c != tt.wantBlocks {
				t.Errorf("want %d <blockquote> openers, got %d:\n%s", tt.wantBlocks, c, out)
			}
			if c := strings.Count(out, "</blockquote>"); c != tt.wantBlocks {
				t.Errorf("want %d </blockquote> closers, got %d:\n%s", tt.wantBlocks, c, out)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("want %q in render output:\n%s", w, out)
				}
			}
		})
	}
}
