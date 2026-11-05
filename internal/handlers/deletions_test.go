package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"disjoint/internal/database"
)

type deletionFixture struct {
	thread  database.Thread
	opening database.Post
	reply   database.Post
	file    string
}

func seedDeletionFixture(t *testing.T, queries *database.Queries, uploadDir string, authorID int64) deletionFixture {
	t.Helper()
	ctx := context.Background()
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: authorID,
		Title:  "delete me",
	})
	if err != nil {
		t.Fatal(err)
	}
	opening, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID,
		UserID:   authorID,
		Body:     "opening",
	})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID,
		UserID:   authorID,
		Body:     "reply with file",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "delete-fixture.png"
	if err := os.WriteFile(filepath.Join(uploadDir, name), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateAttachment(ctx, database.CreateAttachmentParams{
		PostID:      reply.ID,
		Filename:    "delete-fixture.png",
		StoragePath: name,
		MimeType:    "image/png",
		FileSize:    3,
	}); err != nil {
		t.Fatal(err)
	}
	return deletionFixture{thread: thread, opening: opening, reply: reply, file: name}
}

func deleteRequest(router http.Handler, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestDeletePostAndThreadPermissions(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	alice, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "del_alice", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "del_bob", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "del_admin", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.SetUserAdmin(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	aliceCookie := authCookieFor(t, queries, alice.ID)
	bobCookie := authCookieFor(t, queries, bob.ID)
	adminCookie := authCookieFor(t, queries, admin.ID)

	fx := seedDeletionFixture(t, queries, uploadDir, alice.ID)

	// The page renders delete controls only for permitted viewers.
	getThread := func(cookie *http.Cookie) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/threads/"+strconv.FormatInt(fx.thread.ID, 10), nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET thread = %d", rr.Code)
		}
		return rr.Body.String()
	}
	alicePage := getThread(aliceCookie)
	if !strings.Contains(alicePage, "/threads/"+strconv.FormatInt(fx.thread.ID, 10)+"/delete") {
		t.Error("thread author should see the delete-thread control")
	}
	if !strings.Contains(alicePage, "/posts/"+strconv.FormatInt(fx.reply.ID, 10)+"/delete") {
		t.Error("reply author should see the delete-post control")
	}
	bobPage := getThread(bobCookie)
	if strings.Contains(bobPage, "/threads/"+strconv.FormatInt(fx.thread.ID, 10)+"/delete") {
		t.Error("non-author must not see the delete-thread control")
	}
	if strings.Contains(bobPage, "/posts/"+strconv.FormatInt(fx.reply.ID, 10)+"/delete") {
		t.Error("non-author must not see another user's delete-post control")
	}
	if !strings.Contains(getThread(adminCookie), "/threads/"+strconv.FormatInt(fx.thread.ID, 10)+"/delete") {
		t.Error("administrator should see the delete-thread control")
	}

	// Someone else's reply/thread cannot be deleted by a regular member.
	if rr := deleteRequest(router, "/posts/"+strconv.FormatInt(fx.reply.ID, 10)+"/delete", bobCookie); rr.Code != http.StatusNotFound {
		t.Fatalf("foreign post delete = %d, want 404", rr.Code)
	}
	if rr := deleteRequest(router, "/threads/"+strconv.FormatInt(fx.thread.ID, 10)+"/delete", bobCookie); rr.Code != http.StatusNotFound {
		t.Fatalf("foreign thread delete = %d, want 404", rr.Code)
	}
	if _, err := queries.GetPostByID(ctx, fx.reply.ID); err != nil {
		t.Fatal("denied delete removed the post")
	}

	// The reply author deletes their own reply; only that post and its file go.
	replyByBob, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: fx.thread.ID,
		UserID:   bob.ID,
		Body:     "bob reply",
	})
	if err != nil {
		t.Fatal(err)
	}
	rr := deleteRequest(router, "/posts/"+strconv.FormatInt(replyByBob.ID, 10)+"/delete", bobCookie)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/threads/"+strconv.FormatInt(fx.thread.ID, 10)+"?page=1" {
		t.Fatalf("own reply delete = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if _, err := queries.GetPostByID(ctx, replyByBob.ID); err == nil {
		t.Fatal("reply still exists")
	}
	if _, err := queries.GetThreadByID(ctx, fx.thread.ID); err != nil {
		t.Fatal("deleting a reply removed the thread")
	}

	// The fixture reply carries an attachment; deleting it removes the file.
	if rr := deleteRequest(router, "/posts/"+strconv.FormatInt(fx.reply.ID, 10)+"/delete", aliceCookie); rr.Code != http.StatusSeeOther {
		t.Fatalf("fixture reply delete = %d", rr.Code)
	}
	if _, err := os.Stat(filepath.Join(uploadDir, fx.file)); !os.IsNotExist(err) {
		t.Fatalf("attachment file survived post deletion")
	}

	// Deleting the opening post deletes the whole thread.
	rr = deleteRequest(router, "/posts/"+strconv.FormatInt(fx.opening.ID, 10)+"/delete", aliceCookie)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("opening post delete = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if _, err := queries.GetThreadByID(ctx, fx.thread.ID); err == nil {
		t.Fatal("opening-post delete left the thread behind")
	}

	// A thread author deletes their own thread.
	thread2, err := queries.CreateThread(ctx, database.CreateThreadParams{UserID: alice.ID, Title: "thread 2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: thread2.ID, UserID: alice.ID, Body: "opening"}); err != nil {
		t.Fatal(err)
	}
	rr = deleteRequest(router, "/threads/"+strconv.FormatInt(thread2.ID, 10)+"/delete", aliceCookie)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("own thread delete = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if _, err := queries.GetThreadByID(ctx, thread2.ID); err == nil {
		t.Fatal("thread still exists")
	}

	// Administrators can moderate anyone's thread.
	thread3, err := queries.CreateThread(ctx, database.CreateThreadParams{UserID: bob.ID, Title: "moderated"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: thread3.ID, UserID: bob.ID, Body: "opening"}); err != nil {
		t.Fatal(err)
	}
	rr = deleteRequest(router, "/threads/"+strconv.FormatInt(thread3.ID, 10)+"/delete", adminCookie)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("admin thread delete = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if _, err := queries.GetThreadByID(ctx, thread3.ID); err == nil {
		t.Fatal("admin delete left the thread behind")
	}

	// Guests are redirected to login and nothing is deleted.
	guestThread, err := queries.CreateThread(ctx, database.CreateThreadParams{UserID: alice.ID, Title: "guest-protected"})
	if err != nil {
		t.Fatal(err)
	}
	if rr := deleteRequest(router, "/threads/"+strconv.FormatInt(guestThread.ID, 10)+"/delete", nil); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("guest delete = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if _, err := queries.GetThreadByID(ctx, guestThread.ID); err != nil {
		t.Fatal("guest delete removed the thread")
	}

	// Sanity: the database still has exactly one thread (guest-protected).
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 remaining thread, got %d", count)
	}
}
