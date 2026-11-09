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
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("guest /gallery = %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// An empty wall has a dedicated empty state and no grid.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
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
	if !strings.Contains(body, `href="/" class="navbar-item">Gallery`) {
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

	gallery := get("/")
	if !strings.Contains(gallery, `class="tabs is-toggle is-toggle-rounded is-fullwidth"`) {
		t.Error("gallery home missing Bulma tabs switch")
	}
	if !strings.Contains(gallery, `aria-label="Front page view"`) {
		t.Error("gallery home switch missing accessible label")
	}
	if !strings.Contains(gallery, `<a href="/" aria-current="page">Gallery</a>`) {
		t.Error("index should mark Gallery as the active view by default")
	}
	if !strings.Contains(gallery, `<a href="/threads">Threads</a>`) {
		t.Error("gallery home switch missing Threads link")
	}
	if strings.Contains(gallery, `<a href="/threads" aria-current="page">`) {
		t.Error("gallery home must not mark Threads as active")
	}

	threads := get("/threads")
	if !strings.Contains(threads, `class="tabs is-toggle is-toggle-rounded is-fullwidth"`) {
		t.Error("thread list missing Bulma tabs switch")
	}
	if !strings.Contains(threads, `<a href="/threads" aria-current="page">Threads</a>`) {
		t.Error("thread list should mark Threads as the active view")
	}
	if !strings.Contains(threads, `<a href="/">Gallery</a>`) {
		t.Error("thread list switch missing Gallery link")
	}
	if strings.Contains(threads, `<a href="/" aria-current="page">`) {
		t.Error("thread list must not mark Gallery as active")
	}

	// The old gallery URL is a permanent redirect to the canonical home.
	req := httptest.NewRequest(http.MethodGet, "/gallery?page=3", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusMovedPermanently || rr.Header().Get("Location") != "/?page=3" {
		t.Fatalf("GET /gallery?page=3 = %d to %q, want 301 to /?page=3", rr.Code, rr.Header().Get("Location"))
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

	req := httptest.NewRequest(http.MethodGet, "/", nil)
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
		req := httptest.NewRequest(http.MethodGet, "/", nil)
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
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/?page=%d", page), nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /?page=%d = %d", page, rr.Code)
		}
		return rr.Body.String()
	}

	first := get(1)
	if got := strings.Count(first, `class="gallery-tile"`); got != 48 {
		t.Errorf("page 1 tiles = %d, want 48", got)
	}
	if !strings.Contains(first, `href="/?page=2"`) {
		t.Error("page 1 missing Next link")
	}

	second := get(2)
	if got := strings.Count(second, `class="gallery-tile"`); got != 1 {
		t.Errorf("page 2 tiles = %d, want 1", got)
	}
	if !strings.Contains(second, `href="/?page=1"`) {
		t.Error("page 2 missing Prev link")
	}
	if strings.Contains(second, `href="/?page=3"`) {
		t.Error("page 2 must not offer Next for the last page")
	}

	if empty := get(99); !strings.Contains(empty, "No photos yet") {
		t.Error("page 99 should render the empty state")
	}
}

// Gallery reactions are per image: two images in one post react independently,
// through /attachments/{id}/reactions, and do not touch the post's own bar.
func TestGalleryReactions(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	poster := galleryUser(t, queries, "gallery_poster")
	reactor := galleryUser(t, queries, "gallery_reactor")
	reactorCookie := authCookieFor(t, queries, reactor.ID)

	_, post := galleryThread(t, queries, poster.ID, "reaction gallery thread", sql.NullTime{})
	firstImg := galleryAttachment(t, queries, uploadDir, post.ID, "first.png", "image/png")
	secondImg := galleryAttachment(t, queries, uploadDir, post.ID, "second.png", "image/png")

	emoji, err := queries.GetEmojiByName(ctx, "joy")
	if err != nil {
		t.Fatalf("built-in emoji missing: %v", err)
	}

	// The poster reacts to the first image only.
	if _, err := queries.AddAttachmentReaction(ctx, database.AddAttachmentReactionParams{
		AttachmentID: firstImg.ID, UserID: poster.ID, EmojiID: emoji.ID,
	}); err != nil {
		t.Fatal(err)
	}
	// A reaction on the post itself must not leak into the image bars.
	if _, err := queries.AddReaction(ctx, database.AddReactionParams{
		PostID: post.ID, UserID: poster.ID, EmojiID: emoji.ID,
	}); err != nil {
		t.Fatal(err)
	}

	get := func(cookie *http.Cookie) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /gallery = %d", rr.Code)
		}
		return rr.Body.String()
	}

	body := get(reactorCookie)
	if !strings.Contains(body, `hx-post="/attachments/`+fmt.Sprint(firstImg.ID)+`/reactions"`) {
		t.Errorf("first image has no per-image reaction endpoint")
	}
	if !strings.Contains(body, `hx-post="/attachments/`+fmt.Sprint(secondImg.ID)+`/reactions"`) {
		t.Errorf("second image has no per-image reaction endpoint")
	}
	if strings.Contains(body, `hx-post="/posts/`+fmt.Sprint(post.ID)+`/reactions"`) {
		t.Errorf("gallery fell back to post reactions")
	}

	// Exactly one tile shows the poster's reaction, on the first image.
	tileFor := func(page, storage string) string {
		tiles := strings.Split(page, `class="gallery-tile"`)
		if len(tiles) != 3 {
			t.Fatalf("expected 2 tiles, got %d", len(tiles)-1)
		}
		for _, tile := range tiles[1:] {
			if strings.Contains(tile, storage) {
				return tile
			}
		}
		t.Fatalf("no tile for %s", storage)
		return ""
	}
	firstTile := tileFor(body, firstImg.StoragePath)
	secondTile := tileFor(body, secondImg.StoragePath)
	if !strings.Contains(firstTile, `class="reaction-count">1<`) {
		t.Errorf("first image missing its reaction count")
	}
	if strings.Contains(secondTile, `class="reaction-count">`) {
		t.Errorf("second image inherited the first image's reaction")
	}

	// Toggling the first image updates only that bar.
	req := httptest.NewRequest(http.MethodPost, "/attachments/"+fmt.Sprint(firstImg.ID)+"/reactions",
		strings.NewReader("emoji_id="+fmt.Sprint(emoji.ID)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.AddCookie(reactorCookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("attachment reaction toggle = %d: %s", rr.Code, rr.Body.String())
	}
	bar := rr.Body.String()
	if !strings.Contains(bar, `hx-post="/attachments/`+fmt.Sprint(firstImg.ID)+`/reactions"`) {
		t.Errorf("toggle response targets the wrong bar")
	}
	if !strings.Contains(bar, `class="reaction-count">2<`) || !strings.Contains(bar, `reaction-chip is-active`) {
		t.Errorf("toggle response not updated: %s", bar)
	}

	// Reacting to the second image leaves the first untouched.
	req = httptest.NewRequest(http.MethodPost, "/attachments/"+fmt.Sprint(secondImg.ID)+"/reactions",
		strings.NewReader("emoji_id="+fmt.Sprint(emoji.ID)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.AddCookie(reactorCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("second toggle = %d", rr.Code)
	}

	body = get(reactorCookie)
	firstTile = tileFor(body, firstImg.StoragePath)
	secondTile = tileFor(body, secondImg.StoragePath)
	if !strings.Contains(firstTile, `class="reaction-count">2<`) {
		t.Errorf("first image count changed unexpectedly")
	}
	if !strings.Contains(secondTile, `class="reaction-count">1<`) {
		t.Errorf("second image reaction not shown")
	}

	// Non-image attachments have no gallery reaction surface.
	doc := galleryAttachment(t, queries, uploadDir, post.ID, "notes.pdf", "application/pdf")
	req = httptest.NewRequest(http.MethodPost, "/attachments/"+fmt.Sprint(doc.ID)+"/reactions",
		strings.NewReader("emoji_id="+fmt.Sprint(emoji.ID)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(reactorCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("non-image reaction = %d, want 404", rr.Code)
	}
}

// Inline thread images use the same per-image attachment reactions as the
// gallery, so a reaction cast in either place shows in both.
func TestThreadPageImageReactions(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	poster := galleryUser(t, queries, "thread_poster")
	reactor := galleryUser(t, queries, "thread_reactor")
	reactorCookie := authCookieFor(t, queries, reactor.ID)

	thread, post := galleryThread(t, queries, poster.ID, "inline image thread", sql.NullTime{})
	firstImg := galleryAttachment(t, queries, uploadDir, post.ID, "inline-first.png", "image/png")
	secondImg := galleryAttachment(t, queries, uploadDir, post.ID, "inline-second.png", "image/png")

	emoji, err := queries.GetEmojiByName(ctx, "joy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AddAttachmentReaction(ctx, database.AddAttachmentReactionParams{
		AttachmentID: firstImg.ID, UserID: poster.ID, EmojiID: emoji.ID,
	}); err != nil {
		t.Fatal(err)
	}
	// A post reaction must still render on the post's own bar, separately.
	if _, err := queries.AddReaction(ctx, database.AddReactionParams{
		PostID: post.ID, UserID: poster.ID, EmojiID: emoji.ID,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/threads/"+fmt.Sprint(thread.ID), nil)
	req.AddCookie(reactorCookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET thread = %d", rr.Code)
	}
	body := rr.Body.String()

	for _, att := range []database.Attachment{firstImg, secondImg} {
		if !strings.Contains(body, `hx-post="/attachments/`+fmt.Sprint(att.ID)+`/reactions"`) {
			t.Errorf("image %d has no per-image reaction endpoint on the thread page", att.ID)
		}
	}
	if !strings.Contains(body, `hx-post="/posts/`+fmt.Sprint(post.ID)+`/reactions"`) {
		t.Errorf("post reaction bar missing")
	}

	// The poster's image reaction is on the first image only; the post bar's
	// chip is independent.
	cards := strings.Split(body, `class="attachment-image-card"`)
	if len(cards) != 3 {
		t.Fatalf("expected 2 image cards, got %d", len(cards)-1)
	}
	cardFor := func(storage string) string {
		for _, card := range cards[1:] {
			if strings.Contains(card, storage) {
				return card
			}
		}
		t.Fatalf("no card for %s", storage)
		return ""
	}
	firstCard := cardFor(firstImg.StoragePath)
	secondCard := cardFor(secondImg.StoragePath)
	if !strings.Contains(firstCard, `hx-post="/attachments/`+fmt.Sprint(firstImg.ID)+`/reactions"`) {
		t.Errorf("first card targets the wrong endpoint")
	}
	if !strings.Contains(firstCard, `class="reaction-count">1<`) {
		t.Errorf("first image missing its reaction count")
	}
	if strings.Contains(secondCard, `class="reaction-count">`) {
		t.Errorf("second image inherited the first image's reaction")
	}

	// Toggling from the thread page updates the same attachment rows the wall
	// reads.
	req = httptest.NewRequest(http.MethodPost, "/attachments/"+fmt.Sprint(firstImg.ID)+"/reactions",
		strings.NewReader("emoji_id="+fmt.Sprint(emoji.ID)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.AddCookie(reactorCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("toggle = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `class="reaction-count">2<`) {
		t.Errorf("toggle response not updated")
	}

	// The gallery shows the same reaction.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(reactorCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	tiles := strings.Split(rr.Body.String(), `class="gallery-tile"`)
	for _, tile := range tiles[1:] {
		if strings.Contains(tile, firstImg.StoragePath) && !strings.Contains(tile, `class="reaction-count">2<`) {
			t.Errorf("gallery does not show the thread-page reaction")
		}
	}
}
