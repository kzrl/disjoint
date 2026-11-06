package handlers_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"disjoint/internal/database"
)

// newThreadForm posts a multipart thread creation form (the handler parses
// multipart because of the attachment field).
func newThreadForm(t *testing.T, router http.Handler, cookie *http.Cookie, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf strings.Builder
	boundary := "testboundary"
	for k, v := range fields {
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString(fmt.Sprintf("Content-Disposition: form-data; name=%q\r\n\r\n", k))
		buf.WriteString(v + "\r\n")
	}
	buf.WriteString("--" + boundary + "--\r\n")

	req := httptest.NewRequest(http.MethodPost, "/threads/new", strings.NewReader(buf.String()))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestCreateThreadFillsTitleFromLink(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	user, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "linker", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)

	var fetched []string
	app.FetchLinkTitle = func(_ context.Context, rawURL string) (string, error) {
		fetched = append(fetched, rawURL)
		switch rawURL {
		case "https://example.com/good":
			return "  Example Domain  ", nil
		case "https://example.com/long":
			return strings.Repeat("x", 260), nil
		default:
			return "", errors.New("no title")
		}
	}

	// 1. Blank title + link: the page title is used.
	rr := newThreadForm(t, router, cookie, map[string]string{
		"title":      "   ",
		"body":       "worth a read",
		"url":        "https://example.com/good",
		"expiration": "never",
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create = %d", rr.Code)
	}
	threads, err := queries.ListFrontPageThreads(ctx, database.ListFrontPageThreadsParams{Limit: 10, Offset: 0})
	if err != nil || len(threads) != 1 {
		t.Fatalf("expected one thread, got %d (err=%v)", len(threads), err)
	}
	if threads[0].Title != "Example Domain" {
		t.Fatalf("title = %q, want the linked page's title", threads[0].Title)
	}

	// 2. Over-long page titles are truncated to the stored limit.
	rr = newThreadForm(t, router, cookie, map[string]string{
		"body":       "long one",
		"url":        "https://example.com/long",
		"expiration": "never",
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create (long) = %d", rr.Code)
	}
	threads, err = queries.ListFrontPageThreads(ctx, database.ListFrontPageThreadsParams{Limit: 10, Offset: 0})
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range threads {
		if strings.HasPrefix(th.Title, "xxxx") {
			if len([]rune(th.Title)) > 200 {
				t.Fatalf("fetched title not truncated: %d runes", len([]rune(th.Title)))
			}
		}
	}

	// 3. An explicit title is never overwritten, and the fetcher is not used.
	before := len(fetched)
	rr = newThreadForm(t, router, cookie, map[string]string{
		"title":      "My own title",
		"body":       "x",
		"url":        "https://example.com/good",
		"expiration": "never",
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create (own title) = %d", rr.Code)
	}
	if len(fetched) != before {
		t.Errorf("fetcher ran even though the title was provided")
	}
	threads, _ = queries.ListFrontPageThreads(ctx, database.ListFrontPageThreadsParams{Limit: 10, Offset: 0})
	found := false
	for _, th := range threads {
		if th.Title == "My own title" {
			found = true
		}
	}
	if !found {
		t.Errorf("explicit title was not stored")
	}

	// 4. A link whose title cannot be read leaves the title requirement in
	// place: the thread is refused, not created with an empty title.
	rr = newThreadForm(t, router, cookie, map[string]string{
		"body":       "no title available",
		"url":        "https://example.com/none",
		"expiration": "never",
	})
	if !flashCookieSet(rr, "flash_error", "Title and post content (text, a link, or an attachment) are required.") {
		t.Errorf("expected title-required flash when the page has no title")
	}
	threads, _ = queries.ListFrontPageThreads(ctx, database.ListFrontPageThreadsParams{Limit: 10, Offset: 0})
	for _, th := range threads {
		if strings.Contains(th.Title, "no title") || th.Title == "" {
			t.Errorf("thread with an unresolvable title should not exist: %q", th.Title)
		}
	}
	if len(threads) != 3 {
		t.Errorf("expected 3 threads after the refused creation, got %d", len(threads))
	}
}
