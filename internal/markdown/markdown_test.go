package markdown

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	input := "**Bold** and *italic* and `code`"
	output := string(Render(input))

	if !strings.Contains(output, "<strong>Bold</strong>") {
		t.Errorf("expected <strong>Bold</strong>, got: %s", output)
	}
	if !strings.Contains(output, "<em>italic</em>") {
		t.Errorf("expected <em>italic</em>, got: %s", output)
	}
	if !strings.Contains(output, "<code>code</code>") {
		t.Errorf("expected <code>code</code>, got: %s", output)
	}

	// Test raw script tag security
	xssInput := "<script>alert('xss')</script>"
	xssOutput := string(Render(xssInput))
	if strings.Contains(xssOutput, "<script>") {
		t.Errorf("expected raw script tag to be neutralized, got: %s", xssOutput)
	}
}
