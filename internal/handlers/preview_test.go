package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"disjoint/internal/database"
)

// postPreview sends an authenticated preview request and returns the response.
func postPreview(t *testing.T, router http.Handler, cookie *http.Cookie, body, link string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	form.Set("body", body)
	form.Set("url", link)
	req := httptest.NewRequest(http.MethodPost, "/preview", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestPreviewRendersMarkdownAndLinkCard(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()

	ctx := context.Background()
	user, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "previewer",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	cookie := authCookieFor(t, queries, user.ID)
	router := app.Routes()

	body := "**bold** @alice :joy:\n\n| a | b |\n| - | - |\n| 1 | 2 |"
	rr := postPreview(t, router, cookie, body, "https://example.com/a?b=1&c=2")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("expected an HTML fragment, got Content-Type %q", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("expected Cache-Control no-store, got %q", got)
	}

	html := rr.Body.String()
	for _, want := range []string{
		"<strong>bold</strong>",
		`class="mention"`,
		"🙂",
		"<table>",
		`class="post-url"`,
		`href="https://example.com/a?b=1&amp;c=2"`,
		`title="https://example.com/a?b=1&amp;c=2"`,
		`rel="noopener noreferrer"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("preview missing %q\n%s", want, html)
		}
	}
	if strings.Contains(strings.ToLower(html), "<!doctype") || strings.Contains(html, "<html") {
		t.Errorf("preview must be a fragment, not a full page:\n%s", html)
	}

	// The endpoint is read-only: no thread or post rows may be created.
	var threads, posts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&threads); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM posts`).Scan(&posts); err != nil {
		t.Fatalf("count posts: %v", err)
	}
	if threads != 0 || posts != 0 {
		t.Fatalf("preview wrote data: threads=%d posts=%d", threads, posts)
	}
}

func TestPreviewValidationAndAuth(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()

	ctx := context.Background()
	user, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "previewer",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	cookie := authCookieFor(t, queries, user.ID)
	router := app.Routes()

	// Anonymous, non-HTMX: the normal login redirect.
	form := url.Values{}
	form.Set("body", "hello")
	req := httptest.NewRequest(http.MethodPost, "/preview", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous preview: expected 303 /login, got %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// Anonymous, HTMX: 401 with HX-Redirect so the client navigates to login.
	req = httptest.NewRequest(http.MethodPost, "/preview", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized || rr.Header().Get("HX-Redirect") != "/login" {
		t.Fatalf("anonymous HTMX preview: expected 401 + HX-Redirect /login, got %d + %q", rr.Code, rr.Header().Get("HX-Redirect"))
	}

	// Invalid link: inline 200 error, no card, valid body still rendered.
	rr = postPreview(t, router, cookie, "hello", "javascript:alert(1)")
	if rr.Code != http.StatusOK {
		t.Fatalf("invalid link preview: expected 200, got %d", rr.Code)
	}
	html := rr.Body.String()
	if !strings.Contains(html, "link must be a valid http(s) URL") {
		t.Fatalf("invalid link preview missing inline error:\n%s", html)
	}
	if strings.Contains(html, `href="javascript:`) {
		t.Fatalf("invalid link was rendered as a card:\n%s", html)
	}
	if !strings.Contains(html, "<p>hello</p>") {
		t.Fatalf("valid body was dropped when the link was invalid:\n%s", html)
	}

	// Oversized body: inline 200 error, and the payload is not echoed back.
	oversized := strings.Repeat("a", 256*1024+1)
	rr = postPreview(t, router, cookie, oversized, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("oversized preview: expected 200, got %d", rr.Code)
	}
	html = rr.Body.String()
	if !strings.Contains(html, "Post body is too long") {
		t.Fatalf("oversized preview missing inline error:\n%s", html)
	}
	if len(html) > 2000 {
		t.Fatalf("oversized preview echoed the payload (%d bytes)", len(html))
	}

	// Empty body and link: friendly empty state.
	rr = postPreview(t, router, cookie, "   ", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Nothing to preview yet.") {
		t.Fatalf("empty preview: got %d:\n%s", rr.Code, rr.Body.String())
	}
}

func TestComposerPreviewMarkup(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()

	ctx := context.Background()
	user, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "previewer",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	cookie := authCookieFor(t, queries, user.ID)
	router := app.Routes()

	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: user.ID,
		Title:  "preview markup",
	})
	if err != nil {
		t.Fatalf("failed to create thread: %v", err)
	}
	post, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID,
		UserID:   user.ID,
		Body:     "opening post",
	})
	if err != nil {
		t.Fatalf("failed to create post: %v", err)
	}

	cases := []struct {
		name    string
		path    string
		panelID string
	}{
		{"new thread", "/threads/new", "preview-new-thread"},
		{"reply", "/threads/" + strconv.FormatInt(thread.ID, 10), "preview-reply"},
		{"edit post", "/posts/" + strconv.FormatInt(post.ID, 10) + "/edit", "preview-edit"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.AddCookie(cookie)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET %s: expected 200, got %d", tc.path, rr.Code)
			}
			html := rr.Body.String()
			for _, want := range []string{
				`data-preview-toggle="#` + tc.panelID + `"`,
				`id="` + tc.panelID + `"`,
				`hx-post="/preview"`,
				`hx-trigger="preview"`,
				`hx-params="body,url"`,
				`aria-controls="` + tc.panelID + `"`,
			} {
				if !strings.Contains(html, want) {
					t.Errorf("GET %s missing %q", tc.path, want)
				}
			}
		})
	}
}
