package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The homescreen icon set must be reachable without a session: iOS and
// Android fetch the manifest and icons before any login, and a redirect to
// /login would break "Add to Home Screen".
func TestAppIconsAndManifestArePublic(t *testing.T) {
	app, db, _, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr
	}

	// No users exist yet, so the first-run middleware would redirect anything
	// non-public to /setup; these paths must bypass it.
	for _, path := range []string{
		"/static/site.webmanifest",
		"/static/favicon.svg",
		"/static/favicon-32.png",
		"/static/apple-touch-icon.png",
		"/static/icon-192.png",
		"/static/icon-512.png",
		"/static/icon-maskable-512.png",
		"/favicon.ico",
	} {
		if rr := get(path); rr.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rr.Code)
		}
	}

	// The manifest must carry the JSON MIME type the browser requires.
	rr := get("/static/site.webmanifest")
	if ct := rr.Header().Get("Content-Type"); ct != "application/manifest+json" {
		t.Errorf("manifest Content-Type = %q", ct)
	}
	body := rr.Body.String()
	for _, want := range []string{`"start_url": "/"`, `"display": "standalone"`, `icon-512.png`, `icon-maskable-512.png`} {
		if !strings.Contains(body, want) {
			t.Errorf("manifest missing %q:\n%s", want, body)
		}
	}

	// Every page advertises the icon set in its <head>, including the public
	// setup/login pages (the first thing a would-be homescreen visitor sees).
	page := get("/setup").Body.String()
	for _, want := range []string{
		`<link rel="icon" type="image/svg+xml" href="data:image/svg+xml;base64,`,
		`<link rel="apple-touch-icon" sizes="180x180" href="/static/apple-touch-icon.png">`,
		`<link rel="manifest" href="/static/site.webmanifest">`,
		`<meta name="theme-color" content="#3c83f6">`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("setup page head missing %q", want)
		}
	}

	// The navbar wordmark uses the same mark inline (no extra request, and it
	// inherits the theme accent through currentColor).
	if !strings.Contains(page, `<svg class="logo-mark"`) || !strings.Contains(page, `fill="currentColor"`) {
		t.Errorf("navbar logo mark missing from the page")
	}

	// /favicon.ico keeps answering to legacy clients with a real image.
	if ct := get("/favicon.ico").Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("favicon.ico Content-Type = %q, want image/png", ct)
	}
	if ct := get("/static/favicon.svg").Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Errorf("favicon.svg Content-Type = %q", ct)
	}
}
