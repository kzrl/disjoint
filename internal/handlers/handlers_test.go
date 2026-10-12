package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"disjoint/internal/auth"
	"disjoint/internal/database"
	"disjoint/internal/handlers"
	"disjoint/internal/worker"
)

func setupTestApp(t *testing.T) (*handlers.App, *sql.DB, *database.Queries, string) {
	t.Helper()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_disjoint.db")
	uploadDir := filepath.Join(tempDir, "uploads")
	sessionSecret := "test-secret-key-32-bytes-long!!"

	db, queries, err := database.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}

	app, err := handlers.NewApp(db, queries, uploadDir, sessionSecret)
	if err != nil {
		t.Fatalf("failed to create app: %v", err)
	}

	return app, db, queries, uploadDir
}

func TestFirstRunSetupAndAuthFlow(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()

	router := app.Routes()

	// 1. Initial request to / should redirect to /setup because no users exist
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect (303), got %d", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/setup" {
		t.Fatalf("expected redirect to /setup, got %s", loc)
	}

	// 2. GET /setup should return 200
	req = httptest.NewRequest(http.MethodGet, "/setup", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected GET /setup to return 200, got %d", rr.Code)
	}

	// 3. POST /setup to create initial admin
	form := url.Values{}
	form.Set("username", "admin")
	form.Set("password", "password123")
	req = httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect after setup, got %d", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/" {
		t.Fatalf("expected redirect to /, got %s", loc)
	}

	// Extract session cookie
	var sessionCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatalf("expected session cookie after setup")
	}

	// 4. Access / with session cookie -> should return 200 OK
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(sessionCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected GET / with session to return 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "admin") {
		t.Fatalf("expected rendered index page to contain username 'admin'")
	}

	// 5. Subsequent request to /setup should redirect to /login
	req = httptest.NewRequest(http.MethodGet, "/setup", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("expected redirect to /login for /setup when users exist, got %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// 6. Admin generates an invite code
	req = httptest.NewRequest(http.MethodPost, "/invites", nil)
	req.AddCookie(sessionCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/invites" {
		t.Fatalf("expected redirect to /invites after creating invite, got %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// Retrieve created invite from database
	invites, err := queries.ListInvites(context.Background())
	if err != nil || len(invites) == 0 {
		t.Fatalf("expected created invite in database, got err: %v", err)
	}
	inviteCode := invites[0].Code

	// Test Registering a second user with invite code
	form = url.Values{}
	form.Set("username", "alice")
	form.Set("password", "wonderland123")
	form.Set("code", inviteCode)
	req = httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected register redirect, got %d", rr.Code)
	}

	// Verify user exists in database
	u, err := queries.GetUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatalf("failed to find registered user in DB: %v", err)
	}
	if !auth.CheckPassword(u.PasswordHash, "wonderland123") {
		t.Fatalf("password hash verification failed")
	}

	// 7. Test Login with incorrect password
	form = url.Values{}
	form.Set("username", "alice")
	form.Set("password", "wrongpass")
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("expected redirect back to /login on wrong password, got %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// 8. Test Login with correct password
	form.Set("password", "wonderland123")
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("expected redirect to / on valid login, got %d to %s", rr.Code, rr.Header().Get("Location"))
	}
}

func TestInviteSystem(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()

	ctx := context.Background()

	// 1. Create Admin user
	admin, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "admin_user", PasswordHash: "hash"})
	if err != nil {
		t.Fatalf("failed to create admin user: %v", err)
	}
	_ = queries.SetUserAdmin(ctx, admin.ID)

	// Create Non-Admin user
	regularUser, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "regular_user", PasswordHash: "hash"})
	if err != nil {
		t.Fatalf("failed to create regular user: %v", err)
	}

	adminCookie := &http.Cookie{Name: auth.SessionCookieName, Value: auth.CreateSessionToken(admin.ID, "test-secret-key-32-bytes-long!!"), Path: "/"}
	userCookie := &http.Cookie{Name: auth.SessionCookieName, Value: auth.CreateSessionToken(regularUser.ID, "test-secret-key-32-bytes-long!!"), Path: "/"}

	// 2. Non-admin accessing /invites should be redirected to /
	req := httptest.NewRequest(http.MethodGet, "/invites", nil)
	req.AddCookie(userCookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("expected non-admin GET /invites to redirect to /, got %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// 3. Admin accessing /invites should get 200 OK
	req = httptest.NewRequest(http.MethodGet, "/invites", nil)
	req.AddCookie(adminCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected admin GET /invites to return 200 OK, got %d", rr.Code)
	}

	// 4. Admin creates an invite
	req = httptest.NewRequest(http.MethodPost, "/invites", nil)
	req.AddCookie(adminCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/invites" {
		t.Fatalf("expected admin POST /invites to redirect to /invites, got %d", rr.Code)
	}

	invites, err := queries.ListInvites(ctx)
	if err != nil || len(invites) != 1 {
		t.Fatalf("expected 1 invite in database, got %d", len(invites))
	}
	inv := invites[0]

	// 5. GET /register without code shows invite-closed page
	req = httptest.NewRequest(http.MethodGet, "/register", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "invite-only") {
		t.Fatalf("expected GET /register without code to display closed page")
	}

	// 6. GET /register with valid code shows registration page
	req = httptest.NewRequest(http.MethodGet, "/register?code="+inv.Code, nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), inv.Code) {
		t.Fatalf("expected GET /register?code to render register page with code")
	}

	// 7. Registering user with valid code consumes code
	form := url.Values{}
	form.Set("code", inv.Code)
	form.Set("username", "newmember")
	form.Set("password", "password123")
	req = httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("expected successful registration redirect to /, got %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// 8. Re-using the same invite code should fail
	form.Set("username", "anotheruser")
	req = httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/register" {
		t.Fatalf("expected failed registration with used code to redirect to /register, got %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// 9. Admin deleting an unused invite
	inv2, err := queries.CreateInvite(ctx, database.CreateInviteParams{Code: "tempcode999", CreatedBy: admin.ID})
	if err != nil {
		t.Fatalf("failed to create invite 2: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/invites/%d/delete", inv2.ID), nil)
	req.AddCookie(adminCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/invites" {
		t.Fatalf("expected delete invite redirect to /invites, got %d", rr.Code)
	}

	_, err = queries.GetInviteByCode(ctx, "tempcode999")
	if err == nil {
		t.Fatalf("expected deleted invite to no longer exist in DB")
	}
}

func TestThreadCreationAndHTMXReply(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()

	// 1. Setup admin user
	ctx := context.Background()
	hash, _ := auth.HashPassword("password123")
	admin, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "bob",
		PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	sessionSecret := "test-secret-key-32-bytes-long!!"
	cookieToken := auth.CreateSessionToken(admin.ID, sessionSecret)
	authCookie := &http.Cookie{Name: auth.SessionCookieName, Value: cookieToken, Path: "/"}

	// 2. Fetch seeded board
	boards, err := queries.ListBoards(ctx)
	if err != nil || len(boards) == 0 {
		t.Fatalf("expected seeded boards, got err: %v", err)
	}
	boardID := boards[0].ID

	// 3. Create a thread with an attachment
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("title", "My First Discussion")
	_ = w.WriteField("body", "Hello **world**, this is a test post with `code` formatting.")
	_ = w.WriteField("expiration", "7d")

	part, err := w.CreateFormFile("files", "test-image.jpg")
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	_, _ = part.Write([]byte("\xFF\xD8\xFF\xE0\x00\x10JFIFfakeimagedata"))
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/boards/%d/threads/new", boardID), &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(authCookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected thread create redirect, got %d, body: %s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "/threads/") {
		t.Fatalf("expected redirect to /threads/{id}, got %s", loc)
	}

	threadIDStr := strings.TrimPrefix(loc, "/threads/")

	// 4. View thread
	req = httptest.NewRequest(http.MethodGet, loc, nil)
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected GET thread to return 200, got %d", rr.Code)
	}
	bodyContent := rr.Body.String()
	if !strings.Contains(bodyContent, "<strong>world</strong>") {
		t.Fatalf("expected markdown bold tag in post body")
	}
	if !strings.Contains(bodyContent, "test-image.jpg") {
		t.Fatalf("expected attachment filename in thread page")
	}

	// 5. Submit reply via HTMX
	var replyBuf bytes.Buffer
	rw := multipart.NewWriter(&replyBuf)
	_ = rw.WriteField("body", "This is an **HTMX** live reply!")
	_ = rw.Close()

	req = httptest.NewRequest(http.MethodPost, "/threads/"+threadIDStr+"/posts", &replyBuf)
	req.Header.Set("Content-Type", rw.FormDataContentType())
	req.Header.Set("HX-Request", "true")
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected HTMX reply to return 200, got %d, body: %s", rr.Code, rr.Body.String())
	}
	replyResp := rr.Body.String()
	if !strings.Contains(replyResp, "<strong>HTMX</strong> live reply!") {
		t.Fatalf("expected rendered reply HTML snippet in HTMX response, got: %s", replyResp)
	}
	if !strings.Contains(replyResp, "bob") {
		t.Fatalf("expected author username in HTMX post component")
	}

	// Verify uploads directory has the test-image file
	entries, err := os.ReadDir(uploadDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected uploaded attachment file on disk, got %v", err)
	}
	storedFile := entries[0].Name()

	// 6. Test media serving
	req = httptest.NewRequest(http.MethodGet, "/uploads/"+storedFile, nil)
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected media serving to return 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Header().Get("Cache-Control"), "public") {
		t.Fatalf("expected Cache-Control header on served upload")
	}
}

func TestExpiringThreadsAndWorkerCleanup(t *testing.T) {
	_, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()

	// Create user
	u, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "charlie",
		PasswordHash: "fakehash",
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	boards, _ := queries.ListBoards(ctx)
	boardID := boards[0].ID

	// Create a thread that is already expired (expires_at in past)
	pastTime := time.Now().UTC().Add(-1 * time.Hour).Round(0)
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		BoardID:   boardID,
		UserID:    u.ID,
		Title:     "Expired Thread",
		ExpiresAt: sql.NullTime{Time: pastTime, Valid: true},
	})
	if err != nil {
		t.Fatalf("failed to create expired thread: %v", err)
	}

	post, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID,
		UserID:   u.ID,
		Body:     "This post should be purged.",
	})
	if err != nil {
		t.Fatalf("failed to create post: %v", err)
	}

	// Create dummy attachment file on disk
	_ = os.MkdirAll(uploadDir, 0o755)
	dummyFilename := "test-purged-file.png"
	err = os.WriteFile(filepath.Join(uploadDir, dummyFilename), []byte("dummydata"), 0o644)
	if err != nil {
		t.Fatalf("failed to create dummy file: %v", err)
	}

	_, err = queries.CreateAttachment(ctx, database.CreateAttachmentParams{
		PostID:      post.ID,
		Filename:    "userfile.png",
		StoragePath: dummyFilename,
		MimeType:    "image/png",
		FileSize:    9,
	})
	if err != nil {
		t.Fatalf("failed to create attachment: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(filepath.Join(uploadDir, dummyFilename)); os.IsNotExist(err) {
		t.Fatalf("expected dummy file to exist before cleanup")
	}

	// Run cleanup worker
	cleaner := worker.New(queries, uploadDir, 1*time.Minute)
	if err := cleaner.CleanupOnce(ctx); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}

	// Verify thread is deleted from DB
	_, err = queries.GetThreadByID(ctx, thread.ID)
	if err == nil {
		t.Fatalf("expected thread to be deleted from DB")
	}

	// Verify file is removed from disk
	if _, err := os.Stat(filepath.Join(uploadDir, dummyFilename)); !os.IsNotExist(err) {
		t.Fatalf("expected dummy file to be deleted from disk by worker")
	}
}

func TestUnreadIndicators(t *testing.T) {
	_, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()

	// Create two users
	u1, _ := queries.CreateUser(ctx, database.CreateUserParams{Username: "user1", PasswordHash: "h1"})
	u2, _ := queries.CreateUser(ctx, database.CreateUserParams{Username: "user2", PasswordHash: "h2"})

	boards, _ := queries.ListBoards(ctx)
	board := boards[0]

	// User2 marks board as read in the past
	_, _ = db.Exec("INSERT INTO user_board_reads (user_id, board_id, last_read_at) VALUES (?, ?, datetime('now', '-1 minute'))", u2.ID, board.ID)

	// User1 creates a new thread
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		BoardID:   board.ID,
		UserID:    u1.ID,
		Title:     "New unread discussion",
		ExpiresAt: sql.NullTime{Valid: false},
	})
	if err != nil {
		t.Fatalf("failed to create thread: %v", err)
	}
	_, _ = queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID,
		UserID:   u1.ID,
		Body:     "Unread post content",
	})

	// Verify that user2 sees board as unread
	boardDetails, err := queries.ListBoardsWithDetails(ctx)
	if err != nil {
		t.Fatalf("failed to query board details: %v", err)
	}
	var targetBoard *database.ListBoardsWithDetailsRow
	for _, b := range boardDetails {
		if b.ID == board.ID {
			targetBoard = &b
			break
		}
	}
	if targetBoard == nil {
		t.Fatalf("expected board in details")
	}

	read, err := queries.GetUserBoardRead(ctx, database.GetUserBoardReadParams{
		UserID:  u2.ID,
		BoardID: board.ID,
	})
	if err != nil || !read.Valid {
		t.Fatalf("expected valid last read")
	}

	latestTime, ok := targetBoard.LatestUpdatedTime()
	if !ok {
		t.Fatalf("failed to parse latest updated timestamp: %#v", targetBoard.LatestUpdatedAt)
	}
	if !latestTime.After(read.Time) {
		t.Fatalf("expected latest updated time %v to be after last read time %v", latestTime, read.Time)
	}
}

func TestCategoryAndBoardCreation(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()

	// Create user & auth cookie
	ctx := context.Background()
	user, _ := queries.CreateUser(ctx, database.CreateUserParams{Username: "moderator", PasswordHash: "hash"})
	token := auth.CreateSessionToken(user.ID, "test-secret-key-32-bytes-long!!")
	authCookie := &http.Cookie{Name: auth.SessionCookieName, Value: token, Path: "/"}

	// 1. Create Category via POST /categories/new
	form := url.Values{}
	form.Set("name", "Gaming & Hobbies")
	form.Set("position", "5")
	req := httptest.NewRequest(http.MethodPost, "/categories/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(authCookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("expected redirect to / after category creation, got %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	cats, err := queries.ListCategories(ctx)
	if err != nil || len(cats) < 3 {
		t.Fatalf("expected at least 3 categories, got %d", len(cats))
	}
	var newCat database.Category
	for _, c := range cats {
		if c.Name == "Gaming & Hobbies" {
			newCat = c
			break
		}
	}
	if newCat.ID == 0 {
		t.Fatalf("category Gaming & Hobbies was not found in DB")
	}

	// 2. Create Board via POST /boards/new
	bForm := url.Values{}
	bForm.Set("category_id", strconv.FormatInt(newCat.ID, 10))
	bForm.Set("name", "Retro Games")
	bForm.Set("description", "Talk about NES, SNES, and retro arcade titles.")
	req = httptest.NewRequest(http.MethodPost, "/boards/new", strings.NewReader(bForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect after board creation, got %d", rr.Code)
	}
	newBoardLoc := rr.Header().Get("Location")
	if !strings.HasPrefix(newBoardLoc, "/boards/") {
		t.Fatalf("expected redirect to /boards/{id}, got %s", newBoardLoc)
	}
}

func TestUnauthorizedHTMXRequest(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()

	// Ensure users exist so first-run doesn't catch it
	_, _ = queries.CreateUser(context.Background(), database.CreateUserParams{Username: "existing", PasswordHash: "h"})

	// Make an unauthenticated HTMX request
	req := httptest.NewRequest(http.MethodPost, "/threads/1/posts", nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for unauthenticated HTMX request, got %d", rr.Code)
	}
	if rr.Header().Get("HX-Redirect") != "/login" {
		t.Fatalf("expected HX-Redirect header set to /login, got %s", rr.Header().Get("HX-Redirect"))
	}
}

func TestStandardFormReplyFallback(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()

	ctx := context.Background()
	user, _ := queries.CreateUser(ctx, database.CreateUserParams{Username: "dan", PasswordHash: "h"})
	token := auth.CreateSessionToken(user.ID, "test-secret-key-32-bytes-long!!")
	authCookie := &http.Cookie{Name: auth.SessionCookieName, Value: token, Path: "/"}

	boards, _ := queries.ListBoards(ctx)
	thread, _ := queries.CreateThread(ctx, database.CreateThreadParams{
		BoardID:   boards[0].ID,
		UserID:    user.ID,
		Title:     "Fallback Test",
		ExpiresAt: sql.NullTime{Valid: false},
	})

	// Standard non-HTMX multipart form submit
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("body", "Standard reply without JS")
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/threads/%d/posts", thread.ID), &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(authCookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect for standard form reply, got %d", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, fmt.Sprintf("/threads/%d#post-", thread.ID)) {
		t.Fatalf("expected redirect to thread anchoring to post, got %s", loc)
	}
}

func TestUserAdministration(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()
	sessionSecret := "test-secret-key-32-bytes-long!!"

	admin, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "root_admin", PasswordHash: "hash"})
	if err != nil {
		t.Fatalf("failed to create admin user: %v", err)
	}
	if err := queries.SetUserAdmin(ctx, admin.ID); err != nil {
		t.Fatalf("failed to set admin: %v", err)
	}

	regular, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "member_one", PasswordHash: "hash"})
	if err != nil {
		t.Fatalf("failed to create regular user: %v", err)
	}
	secondAdmin, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "second_admin", PasswordHash: "hash"})
	if err != nil {
		t.Fatalf("failed to create second admin: %v", err)
	}
	if err := queries.SetUserAdmin(ctx, secondAdmin.ID); err != nil {
		t.Fatalf("failed to set second admin: %v", err)
	}

	adminCookie := &http.Cookie{Name: auth.SessionCookieName, Value: auth.CreateSessionToken(admin.ID, sessionSecret), Path: "/"}
	userCookie := &http.Cookie{Name: auth.SessionCookieName, Value: auth.CreateSessionToken(regular.ID, sessionSecret), Path: "/"}

	// 1. Non-admin accessing GET /admin/users is redirected to / with unauthorized error
	req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	req.AddCookie(userCookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("expected non-admin GET /admin/users to redirect to /, got %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if !flashCookieSet(rr, "flash_error", "Unauthorized") {
		t.Fatalf("expected flash_error 'Unauthorized' cookie for non-admin access")
	}

	// 2. Non-admin POST /admin/users/{id}/promote is also denied
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/users/%d/promote", regular.ID), nil)
	req.AddCookie(userCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("expected non-admin promote to redirect to /, got %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// 3. Admin listing users sees all registered users
	req = httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	req.AddCookie(adminCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected admin GET /admin/users to return 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	for _, name := range []string{"root_admin", "member_one", "second_admin"} {
		if !strings.Contains(body, name) {
			t.Fatalf("expected user list to contain %q", name)
		}
	}

	// 4. Admin promotes a regular user
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/users/%d/promote", regular.ID), nil)
	req.AddCookie(adminCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin/users" {
		t.Fatalf("expected promote redirect to /admin/users, got %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	promoted, err := queries.GetUserByID(ctx, regular.ID)
	if err != nil || promoted.IsAdmin != 1 {
		t.Fatalf("expected promoted user to have is_admin=1, got %v (err: %v)", promoted, err)
	}

	// 5. Admin demoting themselves is prevented with an error flash
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/users/%d/demote", admin.ID), nil)
	req.AddCookie(adminCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin/users" {
		t.Fatalf("expected self-demote redirect to /admin/users, got %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if !flashCookieSet(rr, "flash_error", "You cannot revoke your own admin privileges") {
		t.Fatalf("expected flash_error cookie preventing self-demote")
	}
	stillAdmin, err := queries.GetUserByID(ctx, admin.ID)
	if err != nil || stillAdmin.IsAdmin != 1 {
		t.Fatalf("expected admin to retain admin status after blocked self-demote, got %v (err: %v)", stillAdmin, err)
	}

	// 6. Admin demoting another admin works
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/users/%d/demote", secondAdmin.ID), nil)
	req.AddCookie(adminCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin/users" {
		t.Fatalf("expected demote redirect to /admin/users, got %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	demoted, err := queries.GetUserByID(ctx, secondAdmin.ID)
	if err != nil || demoted.IsAdmin != 0 {
		t.Fatalf("expected demoted user to have is_admin=0, got %v (err: %v)", demoted, err)
	}
}

func flashCookieSet(rr *httptest.ResponseRecorder, name, value string) bool {
	for _, c := range rr.Result().Cookies() {
		if c.Name == name && c.Value == value {
			return true
		}
	}
	return false
}

func ensureAllImportsUsed() {
	_ = io.EOF
}

func TestOnTheAirPresence(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	user, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "onair_user", PasswordHash: "hash"})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	cookie := &http.Cookie{Name: auth.SessionCookieName, Value: auth.CreateSessionToken(user.ID, "test-secret-key-32-bytes-long!!"), Path: "/"}

	getHome := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET / returned %d, body: %s", rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}

	// 1. An authenticated request writes presence and the homepage lists the user
	body := getHome()
	if !strings.Contains(body, "On the air:") || !strings.Contains(body, "onair_user") {
		t.Fatalf("homepage should list onair_user in the on-air strip, got:\n%s", body)
	}
	ts1, err := queries.GetUserPresence(ctx, user.ID)
	if err != nil {
		t.Fatalf("expected a presence row after request: %v", err)
	}

	// 2. Throttle: a second request within 30s must NOT update last_seen_at
	getHome()
	ts2, err := queries.GetUserPresence(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ts1.Equal(ts2) {
		t.Errorf("throttle violated: presence updated within 30s (%v -> %v)", ts1, ts2)
	}

	// 3. A stale row (40s old) IS refreshed on the next request
	if _, err := db.Exec("UPDATE user_presence SET last_seen_at = datetime('now','-40 seconds') WHERE user_id = ?", user.ID); err != nil {
		t.Fatal(err)
	}
	var tsBack time.Time
	if err := db.QueryRow("SELECT last_seen_at FROM user_presence WHERE user_id = ?", user.ID).Scan(&tsBack); err != nil {
		t.Fatal(err)
	}
	getHome()
	ts3, err := queries.GetUserPresence(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Compare against the backdated value, not ts1: CURRENT_TIMESTAMP has
	// second granularity, so a refreshed value can coincidentally equal ts1.
	if ts3.Equal(tsBack) {
		t.Errorf("stale presence row was not refreshed (still %v)", tsBack)
	}

	// 4. A user last seen >5 minutes ago is not listed. Use a second user:
	// the request below refreshes the viewer's own presence before rendering,
	// so the viewer can never observe their own staleness.
	stale, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "stale_user", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO user_presence (user_id, last_seen_at) VALUES (?, datetime('now','-6 minutes'))", stale.ID); err != nil {
		t.Fatal(err)
	}
	body = getHome() // request as onair_user; onair_user is refreshed, stale_user is not
	if !strings.Contains(body, "onair_user") {
		t.Errorf("viewer should appear in on-air strip:\n%s", body)
	}
	if strings.Contains(body, "stale_user") {
		t.Errorf("stale user (6m) should not appear in on-air strip:\n%s", body)
	}
}

func TestPresenceNotWrittenForPublicPaths(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	// A user must exist to leave first-run mode (else /login redirects to /setup)
	if _, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "public_path_user", PasswordHash: "hash"}); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Unauthenticated request to a public route: no presence write
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /login returned %d", rr.Code)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM user_presence").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected 0 presence rows after public request, got %d", n)
	}
}

func TestUserCardPageAndProfile(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	user, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "card_user", PasswordHash: "hash"})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	cookie := &http.Cookie{Name: auth.SessionCookieName, Value: auth.CreateSessionToken(user.ID, "test-secret-key-32-bytes-long!!"), Path: "/"}

	// Seed: 2 attributed posts
	boards, err := queries.ListBoards(ctx)
	if err != nil || len(boards) == 0 {
		t.Fatalf("expected seeded boards, got %v", err)
	}
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		BoardID: boards[0].ID, UserID: user.ID,
		Title: "card thread", ExpiresAt: sql.NullTime{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := queries.CreatePost(ctx, database.CreatePostParams{
			ThreadID: thread.ID, UserID: user.ID,
			Body: "attributed",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := queries.UpdateUserBio(ctx, database.UpdateUserBioParams{
		Bio: sql.NullString{String: "Hello **bio**", Valid: true}, ID: user.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO user_presence (user_id) VALUES (?)", user.ID); err != nil {
		t.Fatal(err)
	}

	getCard := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/users/card_user", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /users/card_user returned %d", rr.Code)
		}
		return rr.Body.String()
	}

	// 1. Card renders bio (markdown), post count (attributed only), last seen
	body := getCard()
	if !strings.Contains(body, "<strong>bio</strong>") {
		t.Errorf("expected rendered markdown bio, got:\n%s", body)
	}
	if !strings.Contains(body, "2 posts") {
		t.Errorf("expected exactly 2 posts, got:\n%s", body)
	}
	if !strings.Contains(body, "Last seen") {
		t.Errorf("expected 'Last seen' line, got:\n%s", body)
	}

	// 2. Unknown user → 404
	req := httptest.NewRequest(http.MethodGet, "/users/nosuchuser99", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown user, got %d", rr.Code)
	}

	// 3. Profile edit round-trips
	req = httptest.NewRequest(http.MethodPost, "/users/me/profile", strings.NewReader(url.QueryEscape("bio")+"="+url.QueryEscape("Updated _line_.")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected profile save redirect, got %d", rr.Code)
	}
	body = getCard()
	if !strings.Contains(body, "<em>line</em>") {
		t.Errorf("expected updated bio on card, got:\n%s", body)
	}

	// 4. Over-length bio is rejected and not stored
	longBio := strings.Repeat("x", 501)
	req = httptest.NewRequest(http.MethodPost, "/users/me/profile", strings.NewReader("bio="+url.QueryEscape(longBio)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect after rejected bio, got %d", rr.Code)
	}
	var foundFlash bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == "flash_error" {
			foundFlash = true
		}
	}
	if !foundFlash {
		t.Errorf("expected flash_error cookie for over-length bio")
	}
	body = getCard()
	if strings.Contains(body, longBio) {
		t.Errorf("over-length bio was stored")
	}

	// 5. Bio HTML is escaped (no raw script tag survives)
	req = httptest.NewRequest(http.MethodPost, "/users/me/profile", strings.NewReader("bio="+url.QueryEscape("<script>alert(1)</script>")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	body = getCard()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("raw script tag rendered in bio — XSS:\n%s", body)
	}
}

func TestUsernameCharsetValidation(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	postSetup := func(username string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader("username="+url.QueryEscape(username)+"&password=secret123"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	for _, bad := range []string{"a/b", "a_b", "a b", "ab", strings.Repeat("a", 33), "üser"} {
		rr := postSetup(bad)
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("expected redirect for rejected username %q, got %d", bad, rr.Code)
		}
		var rejected bool
		for _, c := range rr.Result().Cookies() {
			if c.Name == "flash_error" {
				rejected = true
			}
		}
		if !rejected {
			t.Errorf("expected flash_error for rejected username %q", bad)
		}
	}

	// A valid username succeeds and creates the account
	rr := postSetup("goodname")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect for valid username, got %d", rr.Code)
	}
	n, err := queries.CountUsers(ctx)
	if err != nil || n != 1 {
		t.Fatalf("expected 1 user after valid setup, got %d (err %v)", n, err)
	}
}

func TestRecentActivityTicker(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	alice, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "ticker_alice", PasswordHash: "hash"})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	cookie := &http.Cookie{Name: auth.SessionCookieName, Value: auth.CreateSessionToken(alice.ID, "test-secret-key-32-bytes-long!!"), Path: "/"}

	boards, err := queries.ListBoards(ctx)
	if err != nil || len(boards) == 0 {
		t.Fatalf("expected seeded boards, got %v", err)
	}
	board := boards[0]

	// Attributed thread + post
	attribThread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		BoardID: board.ID, UserID: alice.ID,
		Title: "ticker thread", ExpiresAt: sql.NullTime{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: attribThread.ID, UserID: alice.ID,
		Body: "hello",
	}); err != nil {
		t.Fatal(err)
	}

	// Expired thread's post must not appear
	expiredThread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		BoardID: board.ID, UserID: alice.ID,
		Title: "expired thread", ExpiresAt: sql.NullTime{Time: time.Now().UTC().Add(-time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: expiredThread.ID, UserID: alice.ID,
		Body: "gone soon",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / returned %d", rr.Code)
	}
	body := rr.Body.String()

	if !strings.Contains(body, "Recent activity") {
		t.Fatalf("homepage missing Recent activity section:\n%s", body)
	}
	// Attributed entry links the author card and the thread
	if !strings.Contains(body, `/users/ticker_alice`) || !strings.Contains(body, "posted in") {
		t.Errorf("expected attributed activity entry with author link:\n%s", body)
	}
	if !strings.Contains(body, `/threads/`) {
		t.Errorf("expected thread links in activity entries:\n%s", body)
	}
	// Expired thread is absent
	if strings.Contains(body, "expired thread") {
		t.Errorf("expired thread should not appear in activity ticker:\n%s", body)
	}
	// Cap: 10 entries max (only 1 non-expired entry exists; assert the query honors limit)
	rows, err := queries.ListRecentActivity(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("ListRecentActivity(limit=5) returned %d rows, want 1", len(rows))
	}
}

func TestActivityTickerEmptyState(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	if _, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "empty_home", PasswordHash: "hash"}); err != nil {
		t.Fatal(err)
	}
	// Wipe any seeded posts so the ticker is truly empty
	if _, err := db.Exec("DELETE FROM posts"); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: auth.SessionCookieName, Value: auth.CreateSessionToken(1, "test-secret-key-32-bytes-long!!"), Path: "/"}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / returned %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No activity yet") {
		t.Errorf("expected 'No activity yet' empty state:\n%s", body)
	}
}

// ---- User avatar uploads ----

const testAvatarSecret = "test-secret-key-32-bytes-long!!"

var (
	pngBytes  = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, make([]byte, 64)...)
	jpgBytes  = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 64)...)
	webpBytes = append(append([]byte("RIFF"), make([]byte, 4)...), append([]byte("WEBP"), make([]byte, 32)...)...)
	gifBytes  = append([]byte("GIF89a"), make([]byte, 32)...)
	txtBytes  = []byte("this is definitely not an image")
)

// profileMultipart builds a multipart profile form with the given fields and
// an optional "avatar" file part.
func profileMultipart(t *testing.T, fields map[string]string, fileName string, fileBytes []byte) (*bytes.Buffer, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if fileName != "" {
		fw, err := w.CreateFormFile("avatar", fileName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(fileBytes); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &b, w.FormDataContentType()
}

func TestUserAvatarUploads(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	user, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "avatar_user", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: auth.SessionCookieName, Value: auth.CreateSessionToken(user.ID, testAvatarSecret), Path: "/"}

	submitProfile := func(body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/users/me/profile", body)
		req.Header.Set("Content-Type", contentType)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	hasFlash := func(rr *httptest.ResponseRecorder, name string) bool {
		for _, c := range rr.Result().Cookies() {
			if c.Name == name && c.Value != "" {
				return true
			}
		}
		return false
	}

	avatarPath := func(t *testing.T) string {
		t.Helper()
		u, err := queries.GetUserByUsername(ctx, "avatar_user")
		if err != nil {
			t.Fatal(err)
		}
		if !u.AvatarPath.Valid {
			return ""
		}
		return u.AvatarPath.String
	}

	cardHTML := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/users/avatar_user", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /users/avatar_user returned %d", rr.Code)
		}
		return rr.Body.String()
	}

	// 1. PNG upload succeeds; file lands in uploadDir; card shows the avatar.
	body, ctype := profileMultipart(t, map[string]string{"bio": "hi"}, "me.png", pngBytes)
	rr := submitProfile(body, ctype)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 after avatar upload, got %d", rr.Code)
	}
	if hasFlash(rr, "flash_error") {
		t.Fatalf("unexpected flash_error on PNG upload")
	}
	pngPath := avatarPath(t)
	if pngPath == "" || !strings.Contains(pngPath, ".png") {
		t.Fatalf("expected png avatar path stored, got %q", pngPath)
	}
	if _, err := os.Stat(filepath.Join(uploadDir, pngPath)); err != nil {
		t.Fatalf("avatar file not saved in uploadDir: %v", err)
	}
	if got := cardHTML(); !strings.Contains(got, "avatar avatar-lg") || !strings.Contains(got, "/uploads/"+pngPath) {
		t.Errorf("user card did not show avatar image:\n%s", got)
	}

	// 2. Avatar is served back over HTTP.
	req := httptest.NewRequest(http.MethodGet, "/uploads/"+pngPath, nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 serving avatar, got %d", rr.Code)
	}
	if !bytes.Equal(rr.Body.Bytes(), pngBytes) {
		t.Errorf("served avatar bytes differ from uploaded bytes")
	}

	// 3. JPEG and WEBP uploads replace the previous avatar; old file is removed.
	for _, tc := range []struct {
		name string
		b    []byte
		ext  string
	}{
		{"me.jpg", jpgBytes, ".jpg"},
		{"me.webp", webpBytes, ".webp"},
	} {
		body, ctype = profileMultipart(t, map[string]string{"bio": "hi"}, tc.name, tc.b)
		rr = submitProfile(body, ctype)
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("%s: expected 303, got %d", tc.name, rr.Code)
		}
		if hasFlash(rr, "flash_error") {
			t.Fatalf("%s: unexpected flash_error", tc.name)
		}
		p := avatarPath(t)
		if !strings.Contains(p, tc.ext) {
			t.Fatalf("%s: expected avatar path with %s, got %q", tc.name, tc.ext, p)
		}
	}
	if _, err := os.Stat(filepath.Join(uploadDir, pngPath)); !os.IsNotExist(err) {
		t.Errorf("old png avatar file should have been removed, stat err=%v", err)
	}

	// 4. Non-image content is rejected even with a lying filename/content type.
	body, ctype = profileMultipart(t, map[string]string{"bio": "hi"}, "evil.png", txtBytes)
	rr = submitProfile(body, ctype)
	if rr.Code != http.StatusSeeOther || !hasFlash(rr, "flash_error") {
		t.Fatalf("expected rejected text file with flash_error")
	}
	if p := avatarPath(t); !strings.Contains(p, ".webp") {
		t.Errorf("avatar changed after rejected upload: %q", p)
	}

	// 5. GIF is an allowed attachment type but NOT a valid avatar format.
	body, ctype = profileMultipart(t, map[string]string{"bio": "hi"}, "anim.gif", gifBytes)
	rr = submitProfile(body, ctype)
	if rr.Code != http.StatusSeeOther || !hasFlash(rr, "flash_error") {
		t.Fatalf("expected rejected gif with flash_error")
	}
	if p := avatarPath(t); !strings.Contains(p, ".webp") {
		t.Errorf("avatar changed after rejected gif: %q", p)
	}

	// 6. Avatar renders next to attributed posts on the thread page.
	boards, err := queries.ListBoards(ctx)
	if err != nil || len(boards) == 0 {
		t.Fatal("expected seeded boards")
	}
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		BoardID: boards[0].ID, UserID: user.ID,
		Title: "avatar thread", ExpiresAt: sql.NullTime{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID, UserID: user.ID,
		Body: "hello",
	}); err != nil {
		t.Fatal(err)
	}
	p := avatarPath(t)
	req = httptest.NewRequest(http.MethodGet, "/threads/"+strconv.FormatInt(thread.ID, 10), nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("thread page returned %d", rr.Code)
	}
	page := rr.Body.String()
	if !strings.Contains(page, "avatar avatar-sm") || !strings.Contains(page, "/uploads/"+p) {
		t.Errorf("thread page did not show post author avatar:\n%s", page)
	}

	// 7. remove_avatar clears the DB row and deletes the file from disk.
	body, ctype = profileMultipart(t, map[string]string{"bio": "bye", "remove_avatar": "on"}, "", nil)
	rr = submitProfile(body, ctype)
	if rr.Code != http.StatusSeeOther || hasFlash(rr, "flash_error") {
		t.Fatalf("expected 303 without error after avatar removal")
	}
	if got := avatarPath(t); got != "" {
		t.Errorf("expected avatar cleared, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(uploadDir, p)); !os.IsNotExist(err) {
		t.Errorf("removed avatar file still on disk, stat err=%v", err)
	}
	if got := cardHTML(); strings.Contains(got, "avatar avatar-lg") {
		t.Errorf("card still shows avatar after removal:\n%s", got)
	}

	// 8. Plain urlencoded bio-only POST still works (no avatar field at all).
	req = httptest.NewRequest(http.MethodPost, "/users/me/profile", strings.NewReader(url.QueryEscape("bio")+"="+url.QueryEscape("plain bio")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("urlencoded profile save returned %d", rr.Code)
	}
	if got := cardHTML(); !strings.Contains(got, "plain bio") {
		t.Errorf("bio-only save did not persist:\n%s", got)
	}
}

func postThreadWithFile(t *testing.T, router http.Handler, cookie *http.Cookie, boardID int64, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("title", "attachment thread")
	_ = w.WriteField("body", "body")
	part, err := w.CreateFormFile("files", filename)
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("failed to write file content: %v", err)
	}
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/boards/%d/threads/new", boardID), &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// TestAttachmentTypeEnforcementAndServing verifies the upload security model:
// the stored extension is derived from the SNIFFED content type, never from
// the user-chosen filename, so a file cannot be served as a type its content
// is not (e.g. HTML saved as .html). Media is served inline; everything else
// is force-downloaded.
func TestAttachmentTypeEnforcementAndServing(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()

	ctx := context.Background()
	hash, _ := auth.HashPassword("password123")
	user, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "carol", PasswordHash: hash})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	authCookie := &http.Cookie{Name: auth.SessionCookieName, Value: auth.CreateSessionToken(user.ID, "test-secret-key-32-bytes-long!!"), Path: "/"}

	boards, err := queries.ListBoards(ctx)
	if err != nil || len(boards) == 0 {
		t.Fatalf("expected seeded boards: %v", err)
	}
	boardID := boards[0].ID

	// 1. Detected HTML content is rejected outright, with no thread left
	// behind and no file on disk.
	threadsBefore, err := queries.ListThreadsByBoard(ctx, boardID)
	if err != nil {
		t.Fatalf("failed to list threads: %v", err)
	}
	rr := postThreadWithFile(t, router, authCookie, boardID, "evil.html",
		[]byte("<html><body><script>alert(1)</script></body></html>"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect back to the form, got %d", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "/threads/new") {
		t.Fatalf("expected redirect to the thread form, got %s", loc)
	}
	if n, _ := queries.ListThreadsByBoard(ctx, boardID); len(n) != len(threadsBefore) {
		t.Fatalf("rejected upload must not create a thread")
	}
	if files := listUploadFiles(t, uploadDir); len(files) != 0 {
		t.Fatalf("rejected upload must not leave a file behind: %v", files)
	}

	// 2. Content that sniffs as text/plain but carries an HTML-ish name
	// must be stored with the .txt extension derived from the sniffed type,
	// never the user-chosen .html — so it can never be served as a page.
	rr = postThreadWithFile(t, router, authCookie, boardID, "sneaky.html",
		[]byte("plain words only, no magic bytes at all"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected thread create redirect, got %d", rr.Code)
	}
	stored := listUploadFiles(t, uploadDir)
	if len(stored) != 1 || !strings.HasSuffix(stored[0], ".txt") {
		t.Fatalf("expected text content stored with .txt extension, got %v", stored)
	}
	req := httptest.NewRequest(http.MethodGet, "/uploads/"+stored[0], nil)
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 serving upload, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("expected text/plain content type, got %q", ct)
	}
	if cd := rr.Header().Get("Content-Disposition"); cd != "attachment" {
		t.Fatalf("expected Content-Disposition: attachment for non-media file, got %q", cd)
	}

	// 3. Real JPEG content (user named it .bin) is stored as .jpg and
	// served inline.
	rr = postThreadWithFile(t, router, authCookie, boardID, "photo.bin",
		[]byte("\xFF\xD8\xFF\xE0\x00\x10JFIFfakeimagedata"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected thread create redirect, got %d", rr.Code)
	}
	var jpg string
	for _, f := range listUploadFiles(t, uploadDir) {
		if strings.HasSuffix(f, ".jpg") {
			jpg = f
		}
	}
	if jpg == "" {
		t.Fatalf("expected JPEG content stored with .jpg extension, got %v", listUploadFiles(t, uploadDir))
	}
	req = httptest.NewRequest(http.MethodGet, "/uploads/"+jpg, nil)
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 serving jpeg, got %d", rr.Code)
	}
	if cd := rr.Header().Get("Content-Disposition"); cd != "inline" {
		t.Fatalf("expected Content-Disposition: inline for media file, got %q", cd)
	}

	// 4. Content outside the allowlist entirely (zip magic) is rejected and
	// leaves no orphaned thread behind.
	rr = postThreadWithFile(t, router, authCookie, boardID, "archive.zip",
		[]byte("PK\x03\x04zipzipzip"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect back to the form, got %d", rr.Code)
	}
	if n, _ := queries.ListThreadsByBoard(ctx, boardID); len(n) != len(threadsBefore)+2 {
		t.Fatalf("rejected zip upload must not create a thread")
	}
}

func listUploadFiles(t *testing.T, uploadDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(uploadDir)
	if err != nil {
		t.Fatalf("failed to read upload dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestConsumeInviteAtomic verifies that an invite can only ever be consumed
// once: the second call reports 0 affected rows, which the registration
// handler treats as "already used" and rolls the new user back.
func TestConsumeInviteAtomic(t *testing.T) {
	_, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()

	hash, _ := auth.HashPassword("password123")
	user, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "dave", PasswordHash: hash})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	if _, err := queries.CreateInvite(ctx, database.CreateInviteParams{
		Code:      "unique-code-123",
		CreatedBy: user.ID,
	}); err != nil {
		t.Fatalf("failed to create invite: %v", err)
	}

	rows, err := queries.ConsumeInvite(ctx, database.ConsumeInviteParams{
		UsedBy: sql.NullInt64{Int64: user.ID, Valid: true},
		Code:   "unique-code-123",
	})
	if err != nil || rows != 1 {
		t.Fatalf("expected first consume to affect 1 row, got rows=%d err=%v", rows, err)
	}

	rows, err = queries.ConsumeInvite(ctx, database.ConsumeInviteParams{
		UsedBy: sql.NullInt64{Int64: user.ID, Valid: true},
		Code:   "unique-code-123",
	})
	if err != nil || rows != 0 {
		t.Fatalf("expected second consume to affect 0 rows, got rows=%d err=%v", rows, err)
	}
}
