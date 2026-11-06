package handlers_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"disjoint/internal/database"
)

// The front page badges threads whose opening post carries a first-class link,
// so a reader can spot linked threads without opening them.
func TestFrontPageLinkBadge(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	u, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "linker", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, u.ID)

	newThread := func(title, url string) {
		t.Helper()
		th, err := queries.CreateThread(ctx, database.CreateThreadParams{UserID: u.ID, Title: title})
		if err != nil {
			t.Fatal(err)
		}
		params := database.CreatePostParams{ThreadID: th.ID, UserID: u.ID, Body: "opening for " + title}
		if url != "" {
			params.Url = sql.NullString{String: url, Valid: true}
		}
		if _, err := queries.CreatePost(ctx, params); err != nil {
			t.Fatal(err)
		}
	}

	newThread("Thread with a link", "https://example.com/interesting")
	newThread("Thread without a link", "")

	get := func(target string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", target, rr.Code)
		}
		return rr.Body.String()
	}

	// Both the plain front page and the search view use the same row builder.
	for _, target := range []string{"/", "/?q=Thread"} {
		page := get(target)
		if !strings.Contains(page, `class="board-link-icon"`) {
			t.Errorf("%s: missing the link badge", target)
		}
		if !strings.Contains(page, `title="Includes a link: https://example.com/interesting"`) {
			t.Errorf("%s: badge does not name the target URL", target)
		}
		if got := strings.Count(page, `class="board-link-icon"`); got != 1 {
			t.Errorf("%s: want exactly one badge for two threads, got %d", target, got)
		}
		// Each row is one .board-row; the badge must be in the row of the
		// linked thread and absent from the other row.
		rows := strings.Split(page, `class="board-row"`)
		foundRows := 0
		for _, row := range rows {
			switch {
			case strings.Contains(row, "Thread with a link"):
				foundRows++
				if !strings.Contains(row, `class="board-link-icon"`) {
					t.Errorf("%s: linked thread is missing its badge", target)
				}
			case strings.Contains(row, "Thread without a link"):
				foundRows++
				if strings.Contains(row, `class="board-link-icon"`) {
					t.Errorf("%s: unlinked thread must not carry a badge", target)
				}
			}
		}
		if foundRows != 2 {
			t.Fatalf("%s: found %d of 2 thread rows", target, foundRows)
		}
	}
}
