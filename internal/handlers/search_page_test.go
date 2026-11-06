package handlers_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"disjoint/internal/database"
)

// TestSearchPage covers the dedicated /search view: anonymous access is
// redirected, the form and hint render without a query, matches are listed
// with the shared front-page rows, and pagination keeps the term.
func TestSearchPage(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	u, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "searcher", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, u.ID)

	newThread := func(title, body string) {
		t.Helper()
		th, err := queries.CreateThread(ctx, database.CreateThreadParams{UserID: u.ID, Title: title})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: th.ID, UserID: u.ID, Body: body}); err != nil {
			t.Fatal(err)
		}
	}

	newThread("Go concurrency patterns", "channels and goroutines")
	newThread("Cooking thread", "pasta recipes")

	get := func(target string, withCookie bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if withCookie {
			req.AddCookie(cookie)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	// 1. Anonymous visitors are sent to the login page.
	if rr := get("/search?q=Go", false); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous /search = %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// 2. No query: the page renders the form and the hint, and lists nothing.
	rr := get("/search", true)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /search = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `<form method="GET" action="/search"`) || !strings.Contains(body, `name="q"`) {
		t.Errorf("search form missing")
	}
	if !strings.Contains(body, "Search thread titles and post bodies") {
		t.Errorf("search hint missing")
	}
	if strings.Contains(body, `class="board-row"`) {
		t.Errorf("empty search should not list threads")
	}

	// 3. A query lists matching threads only, using the front-page row markup.
	rr = get("/search?q=goroutine", true)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /search?q = %d", rr.Code)
	}
	body = rr.Body.String()
	if !strings.Contains(body, "Go concurrency patterns") {
		t.Errorf("matching thread missing from results")
	}
	if strings.Contains(body, "Cooking thread") {
		t.Errorf("non-matching thread leaked into results")
	}
	if !strings.Contains(body, `class="board-row"`) {
		t.Errorf("results should use the shared front-page rows")
	}
	if !strings.Contains(body, `value="goroutine"`) {
		t.Errorf("query not preserved in the input")
	}

	// 4. No matches: empty state with a way back to a blank search.
	rr = get("/search?q=zzzz-nope", true)
	body = rr.Body.String()
	if !strings.Contains(body, "No threads match") || !strings.Contains(body, `href="/search"`) {
		t.Errorf("missing no-results state")
	}

	// 5. A full page of matches advertises page 2, keeping the term.
	for i := 0; i < 31; i++ {
		newThread(fmt.Sprintf("Bulk search target %02d", i), "filled")
	}
	rr = get("/search?q="+url.QueryEscape("Bulk search target"), true)
	body = rr.Body.String()
	// html/template escapes "+" as &#43; inside the href, so match the parts.
	if !strings.Contains(body, "/search?page=2") || !strings.Contains(body, "q=Bulk") {
		t.Errorf("pagination link missing or dropped the query")
	}
	if got := strings.Count(body, `class="board-row"`); got != 30 {
		t.Errorf("expected 30 rows on page 1, got %d", got)
	}

	// 6. The navbar links to it.
	rr = get("/", true)
	if !strings.Contains(rr.Body.String(), `href="/search" class="navbar-item">Search<`) {
		t.Errorf("navbar is missing the Search link")
	}

	// 7. The front page keeps its own ?q= behaviour (results heading) so old
	// links and the shared row builder stay covered.
	rr = get("/?q=goroutine", true)
	body = rr.Body.String()
	if !strings.Contains(body, "Results for") || !strings.Contains(body, "Go concurrency patterns") {
		t.Errorf("front-page ?q= search regressed")
	}
}

// A disabled user's account must not show up as an author in search results
// (the rows are shared with the front page, so this is a cheap regression
// guard on the join).
func TestSearchPageOmitsExpiredThreads(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	u, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "expiry_user", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, u.ID)

	expired, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID:    u.ID,
		Title:     "Expired target thread",
		ExpiresAt: sql.NullTime{Time: time.Now().UTC().Add(-time.Hour).Round(0), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: expired.ID, UserID: u.ID, Body: "target"}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/search?q=target", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("search = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "Expired target thread") {
		t.Errorf("expired thread appeared in search results")
	}
}
