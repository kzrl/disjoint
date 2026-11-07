package handlers_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"disjoint/internal/database"
	"disjoint/internal/worker"
)

func galleryUser(t *testing.T, queries *database.Queries, name string) *database.User {
	t.Helper()
	u, err := queries.CreateUser(context.Background(), database.CreateUserParams{
		Username:     name,
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &u
}

func galleryThread(t *testing.T, queries *database.Queries, userID int64, title string, expiresAt sql.NullTime) (database.Thread, database.Post) {
	t.Helper()
	ctx := context.Background()
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID:    userID,
		Title:     title,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID,
		UserID:   userID,
		Body:     "image post",
	})
	if err != nil {
		t.Fatal(err)
	}
	return thread, post
}

func galleryAttachment(t *testing.T, queries *database.Queries, uploadDir string, postID int64, name, mime string) database.Attachment {
	t.Helper()
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploadDir, name), []byte("image-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	att, err := queries.CreateAttachment(context.Background(), database.CreateAttachmentParams{
		PostID:      postID,
		Filename:    name,
		StoragePath: name,
		MimeType:    mime,
		FileSize:    11,
	})
	if err != nil {
		t.Fatal(err)
	}
	return att
}

func TestGalleryRequiresLoginAndEmptyState(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	user := galleryUser(t, queries, "gallery_guest_user")
	cookie := authCookieFor(t, queries, user.ID)

	// Guests are redirected to login (the first-run check is already satisfied).
	req := httptest.NewRequest(http.MethodGet, "/gallery", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("guest /gallery = %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// An empty wall has a dedicated empty state and no grid.
	req = httptest.NewRequest(http.MethodGet, "/gallery", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /gallery = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No photos yet") {
		t.Error("empty gallery missing empty state")
	}
	if strings.Contains(body, `class="gallery-grid"`) {
		t.Error("empty gallery must not render the grid")
	}
	if !strings.Contains(body, `href="/gallery" class="navbar-item">Gallery`) {
		t.Error("navbar missing Gallery link")
	}
	if !strings.Contains(body, `class="on-air-user"`) || !strings.Contains(body, user.Username) {
		t.Error("gallery missing on-air presence strip")
	}
}

func TestFrontPageViewToggle(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	user := galleryUser(t, queries, "view_switch_user")
	cookie := authCookieFor(t, queries, user.ID)

	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}

	threads := get("/")
	if !strings.Contains(threads, `class="tabs is-toggle is-toggle-rounded is-fullwidth"`) {
		t.Error("front page missing Bulma tabs switch")
	}
	if !strings.Contains(threads, `aria-label="Front page view"`) {
		t.Error("front page switch missing accessible label")
	}
	if !strings.Contains(threads, `<a href="/" aria-current="page">Threads</a>`) {
		t.Error("front page should mark Threads as the active view")
	}
	if !strings.Contains(threads, `<a href="/gallery">Gallery</a>`) {
		t.Error("front page switch missing Gallery link")
	}
	if strings.Contains(threads, `<a href="/gallery" aria-current="page">`) {
		t.Error("front page must not mark Gallery as active")
	}

	gallery := get("/gallery")
	if !strings.Contains(gallery, `class="tabs is-toggle is-toggle-rounded is-fullwidth"`) {
		t.Error("gallery missing Bulma tabs switch")
	}
	if !strings.Contains(gallery, `<a href="/gallery" aria-current="page">Gallery</a>`) {
		t.Error("gallery should mark Gallery as the active view")
	}
	if !strings.Contains(gallery, `<a href="/">Threads</a>`) {
		t.Error("gallery switch missing Threads link")
	}
	if strings.Contains(gallery, `<a href="/" aria-current="page">`) {
		t.Error("gallery must not mark Threads as active")
	}
}

func TestGalleryListsOpeningAndReplyImages(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	user := galleryUser(t, queries, "gallery_lister")
	cookie := authCookieFor(t, queries, user.ID)

	thread, opening := galleryThread(t, queries, user.ID, "gallery thread", sql.NullTime{})
	reply, err := queries.CreatePost(context.Background(), database.CreatePostParams{
		ThreadID: thread.ID,
		UserID:   user.ID,
		Body:     "reply with an image",
	})
	if err != nil {
		t.Fatal(err)
	}

	openingImg := galleryAttachment(t, queries, uploadDir, opening.ID, "opening.png", "image/png")
	replyImg := galleryAttachment(t, queries, uploadDir, reply.ID, "reply.jpg", "image/jpeg")
	galleryAttachment(t, queries, uploadDir, opening.ID, "document.pdf", "application/pdf")
	galleryAttachment(t, queries, uploadDir, opening.ID, "clip.mp4", "video/mp4")

	req := httptest.NewRequest(http.MethodGet, "/gallery", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /gallery = %d", rr.Code)
	}
	body := rr.Body.String()

	if got := strings.Count(body, `class="gallery-tile"`); got != 2 {
		t.Fatalf("expected 2 image tiles, got %d", got)
	}
	if !strings.Contains(body, `data-lightbox-group`) {
		t.Error("gallery grid missing data-lightbox-group")
	}
	for _, path := range []string{openingImg.StoragePath, replyImg.StoragePath} {
		if !strings.Contains(body, `class="lightbox-link" href="/uploads/`+path+`"`) {
			t.Errorf("missing lightbox link for %q", path)
		}
		if !strings.Contains(body, `/thumbs/`+path+`?w=320 320w`) {
			t.Errorf("missing 320w thumbnail candidate for %q", path)
		}
		if !strings.Contains(body, `alt="`+path+`"`) {
			t.Errorf("missing alt text for %q", path)
		}
	}
	if strings.Contains(body, "document.pdf") || strings.Contains(body, "clip.mp4") {
		t.Error("non-image attachments leaked into the wall")
	}
	if !strings.Contains(body, `href="/threads/`+fmt.Sprint(thread.ID)+`?from=`+fmt.Sprint(reply.ID)+`#post-`+fmt.Sprint(reply.ID)+`"`) {
		t.Error("reply image caption does not link to the reply")
	}
	if !strings.Contains(body, "gallery thread") {
		t.Error("caption missing thread title")
	}

	// Newest upload first: the reply image was inserted after the opening one.
	if strings.Index(body, replyImg.StoragePath) > strings.Index(body, openingImg.StoragePath) {
		t.Error("gallery is not newest-first by attachment id")
	}
}

func TestGalleryExcludesExpiredThreadsAndWorkerPurge(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	user := galleryUser(t, queries, "gallery_expiry")
	cookie := authCookieFor(t, queries, user.ID)

	expired, expiredPost := galleryThread(t, queries, user.ID, "expired gallery thread", sql.NullTime{
		Time:  time.Now().UTC().Add(-time.Hour),
		Valid: true,
	})
	expiredImg := galleryAttachment(t, queries, uploadDir, expiredPost.ID, "expired.png", "image/png")

	live, livePost := galleryThread(t, queries, user.ID, "live gallery thread", sql.NullTime{})
	liveImg := galleryAttachment(t, queries, uploadDir, livePost.ID, "live.png", "image/png")

	get := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/gallery", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /gallery = %d", rr.Code)
		}
		return rr.Body.String()
	}

	body := get()
	if strings.Contains(body, expiredImg.StoragePath) {
		t.Error("expired thread image appeared before the worker purge")
	}
	if !strings.Contains(body, liveImg.StoragePath) {
		t.Error("live thread image missing")
	}

	if err := worker.New(queries, uploadDir, time.Minute).CleanupOnce(context.Background()); err != nil {
		t.Fatalf("worker cleanup: %v", err)
	}
	if _, err := queries.GetThreadByID(context.Background(), expired.ID); err == nil {
		t.Fatal("expired thread survived the worker")
	}
	if _, err := queries.GetThreadByID(context.Background(), live.ID); err != nil {
		t.Fatal("worker removed the live thread")
	}
	if _, err := os.Stat(filepath.Join(uploadDir, expiredImg.StoragePath)); !os.IsNotExist(err) {
		t.Fatal("expired image file survived the worker")
	}
	if body := get(); strings.Contains(body, expiredImg.StoragePath) {
		t.Error("expired image still on the wall after purge")
	}
}

func TestGalleryPagination(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	user := galleryUser(t, queries, "gallery_pager")
	cookie := authCookieFor(t, queries, user.ID)
	_, post := galleryThread(t, queries, user.ID, "paged gallery", sql.NullTime{})

	for i := 0; i < 49; i++ {
		name := fmt.Sprintf("gallery-%02d.png", i)
		if _, err := queries.CreateAttachment(context.Background(), database.CreateAttachmentParams{
			PostID:      post.ID,
			Filename:    name,
			StoragePath: name,
			MimeType:    "image/png",
			FileSize:    1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	get := func(page int) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/gallery?page=%d", page), nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /gallery?page=%d = %d", page, rr.Code)
		}
		return rr.Body.String()
	}

	first := get(1)
	if got := strings.Count(first, `class="gallery-tile"`); got != 48 {
		t.Errorf("page 1 tiles = %d, want 48", got)
	}
	if !strings.Contains(first, `href="/gallery?page=2"`) {
		t.Error("page 1 missing Next link")
	}

	second := get(2)
	if got := strings.Count(second, `class="gallery-tile"`); got != 1 {
		t.Errorf("page 2 tiles = %d, want 1", got)
	}
	if !strings.Contains(second, `href="/gallery?page=1"`) {
		t.Error("page 2 missing Prev link")
	}
	if strings.Contains(second, `href="/gallery?page=3"`) {
		t.Error("page 2 must not offer Next for the last page")
	}

	if empty := get(99); !strings.Contains(empty, "No photos yet") {
		t.Error("page 99 should render the empty state")
	}
}

// Gallery tiles react on the post that carries the image, with the same bar and
// toggle endpoint as the thread page.
func TestGalleryReactions(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	poster := galleryUser(t, queries, "gallery_poster")
	reactor := galleryUser(t, queries, "gallery_reactor")
	reactorCookie := authCookieFor(t, queries, reactor.ID)

	_, post := galleryThread(t, queries, poster.ID, "reaction gallery thread", sql.NullTime{})
	galleryAttachment(t, queries, uploadDir, post.ID, "reactable.png", "image/png")

	emoji, err := queries.GetEmojiByName(ctx, "joy")
	if err != nil {
		t.Fatalf("built-in emoji missing: %v", err)
	}

	// The poster reacts before anyone opens the gallery.
	if _, err := queries.AddReaction(ctx, database.AddReactionParams{
		PostID: post.ID, UserID: poster.ID, EmojiID: emoji.ID,
	}); err != nil {
		t.Fatal(err)
	}

	get := func(cookie *http.Cookie) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/gallery", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /gallery = %d", rr.Code)
		}
		return rr.Body.String()
	}

	body := get(reactorCookie)
	if !strings.Contains(body, `class="gallery-tile"`) {
		t.Fatalf("gallery did not render the tile")
	}
	if !strings.Contains(body, `hx-post="/posts/`+fmt.Sprint(post.ID)+`/reactions"`) {
		t.Errorf("gallery tile has no reaction toggle for post %d", post.ID)
	}
	// The poster's reaction is visible, with its count, and is not "you".
	if !strings.Contains(body, `class="reaction-chip"`) || !strings.Contains(body, `class="reaction-count">1<`) {
		t.Errorf("existing reaction not rendered in the gallery")
	}
	if strings.Contains(body, `reaction-chip is-active`) {
		t.Errorf("another user's reaction was marked as the viewer's")
	}

	// Toggling from the gallery goes through the normal endpoint and returns the
	// updated bar (HTMX swaps it in place).
	req := httptest.NewRequest(http.MethodPost, "/posts/"+fmt.Sprint(post.ID)+"/reactions",
		strings.NewReader("emoji_id="+fmt.Sprint(emoji.ID)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.AddCookie(reactorCookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("gallery reaction toggle = %d: %s", rr.Code, rr.Body.String())
	}
	bar := rr.Body.String()
	if !strings.Contains(bar, `class="reaction-count">2<`) {
		t.Errorf("toggle response does not show the updated count: %s", bar)
	}
	if !strings.Contains(bar, `reaction-chip is-active`) {
		t.Errorf("toggle response does not mark the viewer's reaction active")
	}

	// The reaction is the post's reaction everywhere: the poster sees the same
	// count on the gallery after a reload.
	if body = get(reactorCookie); !strings.Contains(body, `class="reaction-count">2<`) {
		t.Errorf("gallery did not reflect the new reaction")
	}
}
