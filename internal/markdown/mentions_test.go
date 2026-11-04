package markdown

import (
	"reflect"
	"regexp"
	"testing"
)

// TestMentionsMatchRenderedPills pins the invariant the inbox depends on:
// the usernames Mentions() reports are exactly the mention pills Render()
// produces. If these ever diverge, users get notified about tokens that are
// not links (or miss ones that are).
func TestMentionsMatchRenderedPills(t *testing.T) {
	bodies := []string{
		"",
		"plain text with no mentions",
		"hello @alice",
		"@alice and @bob, then @alice again",
		"**@alice** in emphasis",
		"(@alice) after a paren",
		"`@alice` in a code span",
		"```\n@alice in a fence\n```",
		"mail user@gmail.com but mention @bob",
		"[@alice](/users/alice) in a link label",
		`escaped \@alice`,
		"@ab is too short",
		"@" + repeatChar('a', 32),
		"@" + repeatChar('a', 33) + " is too long",
		"> quoted @alice\n\n- list @bob",
		"| table | @carol |\n| --- | --- |\n| x | y |",
	}

	for _, body := range bodies {
		got := Mentions(body)
		want := renderedMentionNames(body)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Mentions(%q) = %#v, rendered pills = %#v (html %q)",
				body, got, want, string(Render(body)))
		}
	}
}

func TestMentionsOrderAndDedupe(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "none", input: "no mentions here", want: nil},
		{name: "single", input: "hi @alice", want: []string{"alice"}},
		{name: "first appearance order", input: "@bob and @alice", want: []string{"bob", "alice"}},
		{name: "dedupe keeps first position", input: "@alice @bob @alice", want: []string{"alice", "bob"}},
		{name: "code span ignored", input: "`@alice`", want: nil},
		{name: "email ignored", input: "user@gmail.com", want: nil},
		{name: "link label ignored", input: "[@alice](/users/alice)", want: nil},
		{name: "emphasis linked", input: "**@alice**", want: []string{"alice"}},
		{name: "unknown user still extracted", input: "@nobodyhere", want: []string{"nobodyhere"}},
		{name: "short token ignored", input: "@ab", want: nil},
		{name: "max length accepted", input: "@" + repeatChar('a', 32), want: []string{repeatChar('a', 32)}},
		{name: "over max ignored", input: "@" + repeatChar('a', 33), want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Mentions(tt.input); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Mentions(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

var mentionAttrRe = regexp.MustCompile(`data-mention="@([^"]*)"`)

// renderedMentionNames extracts the deduped names from the pills in rendered
// HTML (Mentions reports each user once, so the contract is set equality in
// first-appearance order, not raw pill count).
func renderedMentionNames(body string) []string {
	matches := mentionAttrRe.FindAllStringSubmatch(string(Render(body)), -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		if _, dup := seen[m[1]]; dup {
			continue
		}
		seen[m[1]] = struct{}{}
		names = append(names, m[1])
	}
	return names
}

func repeatChar(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
