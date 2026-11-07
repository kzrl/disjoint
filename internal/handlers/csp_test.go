package handlers

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"disjoint/web"
)

// TestTemplatesAreCSPCompatible pins the contract behind the script-src 'self'
// CSP: page markup must not carry inline scripts, inline event handlers, or
// htmx attributes that require eval. A regression here would silently break
// behaviour in every browser once the CSP is enforced.
func TestTemplatesAreCSPCompatible(t *testing.T) {
	files, err := fs.Glob(web.Files, "templates/*.html")
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no templates found")
	}

	scriptTag := regexp.MustCompile(`(?i)<script(?:\s[^>]*)?>`)
	inlineHandler := regexp.MustCompile(`(?i)\son(?:click|submit|change|input|load|error|focus|blur|keydown|keyup|mouseover|mouseout|reset)\s*=`)

	for _, name := range files {
		data, err := fs.ReadFile(web.Files, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(data)

		for _, tag := range scriptTag.FindAllString(body, -1) {
			if !strings.Contains(strings.ToLower(tag), " src=") {
				t.Errorf("%s: inline <script> is blocked by script-src 'self': %s", name, tag)
			}
		}
		if match := inlineHandler.FindString(body); match != "" {
			t.Errorf("%s: inline event handler is blocked by script-src 'self': %q", name, match)
		}
		for _, forbidden := range []string{"hx-on", "[!document"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s: %q requires htmx eval, which the CSP and htmx-config disable", name, forbidden)
			}
		}
	}
}
