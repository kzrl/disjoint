package handlers_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"disjoint/internal/database"
)

func TestThreadPagination(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	user, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "pagination_user",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)

	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: user.ID,
		Title:  "pagination thread",
	})
	if err != nil {
		t.Fatal(err)
	}
	postIDs := make([]int64, 0, 45)
	for i := 0; i < 45; i++ {
		post, err := queries.CreatePost(ctx, database.CreatePostParams{
			ThreadID: thread.ID,
			UserID:   user.ID,
			Body:     fmt.Sprintf("post %02d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		postIDs = append(postIDs, post.ID)
	}

	getPage := func(path string) (int, string, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr.Code, rr.Header().Get("Location"), rr.Body.String()
	}

	threadPath := fmt.Sprintf("/threads/%d", thread.ID)

	// Default page: page 1, 20 posts, no composer, Next but no Prev.
	code, _, body := getPage(threadPath)
	if code != http.StatusOK {
		t.Fatalf("page 1 = %d", code)
	}
	if got := strings.Count(body, `class="post-card media"`); got != 20 {
		t.Fatalf("page 1 posts = %d, want 20", got)
	}
	if !strings.Contains(body, "Page 1 of 3") || !strings.Contains(body, `href="/threads/`+strconv.FormatInt(thread.ID, 10)+`?page=2"`) {
		t.Error("page 1 missing page indicator or Next link")
	}
	if strings.Contains(body, `href="/threads/`+strconv.FormatInt(thread.ID, 10)+`?page=0"`) {
		t.Error("page 1 must not render a Prev link")
	}
	if strings.Contains(body, `id="reply-form"`) || strings.Contains(body, `id="new-posts"`) {
		t.Error("page 1 must not render the composer or poller")
	}
	if !strings.Contains(body, "Go to the latest replies to reply") {
		t.Error("page 1 missing the go-to-latest link")
	}
	if strings.Contains(body, `action="/posts/`+strconv.FormatInt(postIDs[0], 10)+`/delete"`) {
		t.Error("opening post must not render a post-delete action")
	}

	// Page 2: 20 posts, Prev and Next, still no composer. The first post here
	// is a reply, so it must still offer its author a delete control.
	code, _, body = getPage(threadPath + "?page=2")
	if code != http.StatusOK {
		t.Fatalf("page 2 = %d", code)
	}
	if got := strings.Count(body, `class="post-card media"`); got != 20 {
		t.Fatalf("page 2 posts = %d, want 20", got)
	}
	if !strings.Contains(body, "Page 2 of 3") ||
		!strings.Contains(body, `href="/threads/`+strconv.FormatInt(thread.ID, 10)+`?page=1"`) ||
		!strings.Contains(body, `href="/threads/`+strconv.FormatInt(thread.ID, 10)+`?page=3"`) {
		t.Error("page 2 missing page indicator, Prev, or Next")
	}
	if strings.Contains(body, `id="reply-form"`) {
		t.Error("page 2 must not render the composer")
	}
	if !strings.Contains(body, `action="/posts/`+strconv.FormatInt(postIDs[20], 10)+`/delete"`) {
		t.Error("first reply on page 2 should still have a delete control")
	}

	// Last page: 5 posts, Prev only, composer and poller present.
	code, _, body = getPage(threadPath + "?page=3")
	if code != http.StatusOK {
		t.Fatalf("page 3 = %d", code)
	}
	if got := strings.Count(body, `class="post-card media"`); got != 5 {
		t.Fatalf("page 3 posts = %d, want 5", got)
	}
	if !strings.Contains(body, "Page 3 of 3") ||
		!strings.Contains(body, `href="/threads/`+strconv.FormatInt(thread.ID, 10)+`?page=2"`) {
		t.Error("page 3 missing page indicator or Prev link")
	}
	if strings.Contains(body, `href="/threads/`+strconv.FormatInt(thread.ID, 10)+`?page=4"`) {
		t.Error("last page must not render a Next link")
	}
	if !strings.Contains(body, `id="reply-form"`) || !strings.Contains(body, `id="new-posts"`) {
		t.Error("last page missing composer or poller")
	}

	// Out-of-range pages clamp instead of 404ing.
	code, _, body = getPage(threadPath + "?page=99")
	if code != http.StatusOK || !strings.Contains(body, "Page 3 of 3") {
		t.Fatalf("page 99 did not clamp to the last page: %d", code)
	}
	code, _, body = getPage(threadPath + "?page=0")
	if code != http.StatusOK || !strings.Contains(body, "Page 1 of 3") {
		t.Fatalf("page 0 did not clamp to page 1: %d", code)
	}

	// ?from=<post> redirects to the page containing that post.
	code, loc, _ := getPage(threadPath + "?from=" + strconv.FormatInt(postIDs[24], 10))
	if code != http.StatusSeeOther || loc != threadPath+"?page=2#post-"+strconv.FormatInt(postIDs[24], 10) {
		t.Fatalf("from post 25 = %d %q", code, loc)
	}
	code, loc, _ = getPage(threadPath + "?from=" + strconv.FormatInt(postIDs[0], 10))
	if code != http.StatusSeeOther || loc != threadPath+"?page=1#post-"+strconv.FormatInt(postIDs[0], 10) {
		t.Fatalf("from opening post = %d %q", code, loc)
	}
	code, loc, _ = getPage(threadPath + "?from=999999")
	if code != http.StatusSeeOther || loc != threadPath+"?page=1" {
		t.Fatalf("from unknown post = %d %q", code, loc)
	}

	// A post from another thread also falls back to page 1.
	otherThread, err := queries.CreateThread(ctx, database.CreateThreadParams{UserID: user.ID, Title: "other"})
	if err != nil {
		t.Fatal(err)
	}
	otherPost, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: otherThread.ID, UserID: user.ID, Body: "other"})
	if err != nil {
		t.Fatal(err)
	}
	code, loc, _ = getPage(threadPath + "?from=" + strconv.FormatInt(otherPost.ID, 10))
	if code != http.StatusSeeOther || loc != threadPath+"?page=1" {
		t.Fatalf("from foreign post = %d %q", code, loc)
	}
}
