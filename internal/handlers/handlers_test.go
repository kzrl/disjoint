package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"disjoint/internal/auth"
	"disjoint/internal/buildinfo"
	"disjoint/internal/database"
	"disjoint/internal/handlers"
	"disjoint/internal/worker"
)

func setupTestApp(t *testing.T) (*handlers.App, *sql.DB, *database.Queries, string) {
	t.Helper()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_disjoint.db")
	uploadDir := filepath.Join(tempDir, "uploads")

	db, queries, err := database.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}

	app, err := handlers.NewApp(db, queries, uploadDir)
	if err != nil {
		t.Fatalf("failed to create app: %v", err)
	}

	return app, db, queries, uploadDir
}

// authCookieFor creates a real server-side session row for the user and
// returns the matching cookie — the same path the login handler uses.
func authCookieFor(t *testing.T, queries *database.Queries, userID int64) *http.Cookie {
	t.Helper()
	token, err := auth.GenerateSessionToken()
	if err != nil {
		t.Fatalf("failed to generate session token: %v", err)
	}
	if err := queries.CreateSession(context.Background(), database.CreateSessionParams{
		TokenHash: auth.TokenHash(token),
		UserID:    userID,
		ExpiresAt: time.Now().UTC().Add(auth.SessionDuration).Unix(),
	}); err != nil {
		t.Fatalf("failed to create session row: %v", err)
	}
	return &http.Cookie{Name: auth.SessionCookieName, Value: token, Path: "/"}
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

// TestSetupRaceLeavesSingleAdmin fires concurrent first-run setup POSTs and
// asserts the invariant the rollback in SetupSubmit enforces: no matter how
// the CountUsers-check/CreateUser window interleaves, exactly one admin
// exists afterwards (the earliest row) and every loser bailed to /login.
func TestSetupRaceLeavesSingleAdmin(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()

	const n = 8
	responses := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			form := url.Values{}
			form.Set("username", fmt.Sprintf("racer%02d", i))
			form.Set("password", "password123")
			req := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			responses[i] = rr
		}(i)
	}
	wg.Wait()

	for i, rr := range responses {
		if rr.Code != http.StatusSeeOther {
			t.Errorf("setup %d: expected 303 redirect, got %d", i, rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != "/" && loc != "/login" {
			t.Errorf("setup %d: expected redirect to / or /login, got %s", i, loc)
		}
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("failed to count users: %v", err)
	}
	if count != 1 {
		t.Fatalf("exactly one admin may win the first-run race, got %d users", count)
	}

	// The winner is the response that landed on /; that username must be the
	// surviving row.
	winner := -1
	for i, rr := range responses {
		if rr.Header().Get("Location") == "/" {
			winner = i
		}
	}
	if winner == -1 {
		t.Fatalf("expected one setup to succeed (redirect to /)")
	}
	if _, err := queries.GetUserByUsername(context.Background(), fmt.Sprintf("racer%02d", winner)); err != nil {
		t.Fatalf("expected the winning setup user to exist: %v", err)
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

	adminCookie := authCookieFor(t, queries, admin.ID)
	userCookie := authCookieFor(t, queries, regularUser.ID)

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
	authCookie := authCookieFor(t, queries, admin.ID)

	// 2. Create a thread with an attachment
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

	req := httptest.NewRequest(http.MethodPost, "/threads/new", &b)
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

	// Quote excludes attachments by construction: the quote JS reads only the
	// .content div, so attachment markup must remain a SIBLING of it (closed
	// before it), not nested inside. This pins that structure.
	contentStart := strings.Index(bodyContent, `<div class="content is-medium">`)
	attachStart := strings.Index(bodyContent, `<div class="post-attachments">`)
	if contentStart < 0 || attachStart < 0 || contentStart > attachStart {
		t.Fatalf("expected .content div followed by .post-attachments in the post card")
	}
	if !strings.Contains(bodyContent[contentStart:attachStart], "</div>") {
		t.Errorf("attachment markup must be OUTSIDE the .content div (quote reads .content only)")
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

// TestImageOnlyPosts covers the clipboard-paste path: a post whose body is
// empty but which carries at least one file must be accepted, while a post
// with neither is still rejected.
func TestImageOnlyPosts(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	hash, _ := auth.HashPassword("password123")
	user, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "carol",
		PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	authCookie := authCookieFor(t, queries, user.ID)

	// Minimal bytes that sniff as image/png via http.DetectContentType.
	pngBytes := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}

	// Seed a thread with a normal body.
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("title", "Paste test thread")
	_ = w.WriteField("body", "Seed post.")
	_ = w.WriteField("expiration", "never")
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, "/threads/new", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(authCookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected thread create redirect, got %d, body: %s", rr.Code, rr.Body.String())
	}
	threadIDStr := strings.TrimPrefix(rr.Header().Get("Location"), "/threads/")

	// 1. Image-only HTMX reply (what a clipboard paste produces: empty
	//    body, one image file).
	var replyBuf bytes.Buffer
	rw := multipart.NewWriter(&replyBuf)
	_ = rw.WriteField("body", "")
	part, err := rw.CreateFormFile("files", "clipboard-image-1.png")
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	_, _ = part.Write(pngBytes)
	_ = rw.Close()

	req = httptest.NewRequest(http.MethodPost, "/threads/"+threadIDStr+"/posts", &replyBuf)
	req.Header.Set("Content-Type", rw.FormDataContentType())
	req.Header.Set("HX-Request", "true")
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected image-only HTMX reply to return 200, got %d, body: %s", rr.Code, rr.Body.String())
	}
	replyHTML := rr.Body.String()
	if !strings.Contains(replyHTML, "clipboard-image-1.png") {
		t.Fatalf("expected attachment filename in HTMX reply, got: %s", replyHTML)
	}
	if strings.Contains(replyHTML, `class="content is-medium"`) {
		t.Fatalf("expected no .content div for an empty-body post, got: %s", replyHTML)
	}
	if !strings.Contains(replyHTML, "carol") {
		t.Fatalf("expected author username in HTMX reply")
	}

	// 2. Empty body AND no files is still rejected.
	var emptyBuf bytes.Buffer
	ef := multipart.NewWriter(&emptyBuf)
	_ = ef.WriteField("body", "")
	_ = ef.Close()
	req = httptest.NewRequest(http.MethodPost, "/threads/"+threadIDStr+"/posts", &emptyBuf)
	req.Header.Set("Content-Type", ef.FormDataContentType())
	req.Header.Set("HX-Request", "true")
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty body with no files, got %d", rr.Code)
	}

	// 3. New thread with an empty body but an image attachment succeeds.
	var newBuf bytes.Buffer
	nw := multipart.NewWriter(&newBuf)
	_ = nw.WriteField("title", "Image-only thread")
	_ = nw.WriteField("body", "")
	_ = nw.WriteField("expiration", "never")
	np, err := nw.CreateFormFile("files", "clipboard-image-1.png")
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	_, _ = np.Write(pngBytes)
	_ = nw.Close()

	req = httptest.NewRequest(http.MethodPost, "/threads/new", &newBuf)
	req.Header.Set("Content-Type", nw.FormDataContentType())
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || !strings.HasPrefix(rr.Header().Get("Location"), "/threads/") {
		t.Fatalf("expected image-only thread create redirect, got %d -> %s", rr.Code, rr.Header().Get("Location"))
	}

	// 4. Missing title is still rejected, and the rejected upload must not
	//    leave orphan files on disk.
	entriesBefore, _ := os.ReadDir(uploadDir)
	var badBuf bytes.Buffer
	bw := multipart.NewWriter(&badBuf)
	_ = bw.WriteField("title", "")
	_ = bw.WriteField("body", "")
	_ = bw.WriteField("expiration", "never")
	bp, err := bw.CreateFormFile("files", "clipboard-image-1.png")
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	_, _ = bp.Write(pngBytes)
	_ = bw.Close()

	req = httptest.NewRequest(http.MethodPost, "/threads/new", &badBuf)
	req.Header.Set("Content-Type", bw.FormDataContentType())
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/threads/new" {
		t.Fatalf("expected redirect back to /threads/new, got %d -> %s", rr.Code, rr.Header().Get("Location"))
	}
	entriesAfter, _ := os.ReadDir(uploadDir)
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatalf("rejected upload left orphan files: %d before, %d after", len(entriesBefore), len(entriesAfter))
	}
}

func TestMentionRendering(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()

	ctx := context.Background()
	hash, _ := auth.HashPassword("password123")

	// Two real users plus one username that must not exist.
	bob, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "bob",
		PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	alice, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "alice",
		PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	authCookie := authCookieFor(t, queries, bob.ID)

	// Thread body exercises: a real mention, an unknown mention (link-all
	// behavior: it still links, and 404s at the user card), an e-mail
	// address (must NOT become a mention), and a code span (must NOT).
	body := "ping @alice and @nobody\nreach me at user@gmail.com or `@bob`"

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("title", "Mention test")
	_ = w.WriteField("body", body)
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, "/threads/new", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(authCookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected thread create redirect, got %d, body: %s", rr.Code, rr.Body.String())
	}
	threadPath := rr.Header().Get("Location")

	// 1. Thread page: both mentions render as mention anchors.
	req = httptest.NewRequest(http.MethodGet, threadPath, nil)
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected GET thread to return 200, got %d", rr.Code)
	}
	page := rr.Body.String()

	mentionAnchor := func(name string) string {
		return `<a class="mention" href="/users/` + name + `" data-mention="@` + name + `">@` + name + `</a>`
	}
	if !strings.Contains(page, mentionAnchor("alice")) {
		t.Errorf("thread page: want mention anchor for real user alice, got: %s", page)
	}
	if !strings.Contains(page, mentionAnchor("nobody")) {
		t.Errorf("thread page: want mention anchor for unknown user (link-all behavior), got: %s", page)
	}
	if c := strings.Count(page, `class="mention"`); c != 2 {
		t.Errorf("thread page: want exactly 2 mentions (alice, nobody), got %d in: %s", c, page)
	}
	if strings.Contains(page, "gmail\" data-mention") || strings.Contains(page, `/users/gmail`) {
		t.Errorf("thread page: e-mail address must not become a mention, got: %s", page)
	}
	if !strings.Contains(page, `<code>@bob</code>`) {
		t.Errorf("thread page: want code span preserved, got: %s", page)
	}

	// Quote affordance (Task 3): the single post card carries the Quote
	// button, and the container carries the thread id for permalinks.
	if c := strings.Count(page, `class="button is-small post-action quote-btn"`); c != 1 {
		t.Errorf("thread page: want exactly 1 quote button (one post), got %d", c)
	}
	if !strings.Contains(page, `id="posts-container" class="posts-container" data-thread-id="`) {
		t.Errorf("thread page: want data-thread-id on #posts-container, got: %s", page)
	}

	// 2. HTMX reply partial: a mention in the new post renders too.
	var replyBuf bytes.Buffer
	rw := multipart.NewWriter(&replyBuf)
	_ = rw.WriteField("body", "thanks @bob")
	_ = rw.Close()

	req = httptest.NewRequest(http.MethodPost, threadPath+"/posts", &replyBuf)
	req.Header.Set("Content-Type", rw.FormDataContentType())
	req.Header.Set("HX-Request", "true")
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected HTMX reply to return 200, got %d, body: %s", rr.Code, rr.Body.String())
	}
	partial := rr.Body.String()
	if !strings.Contains(partial, mentionAnchor("bob")) {
		t.Errorf("HTMX partial: want mention anchor for bob, got: %s", partial)
	}
	if c := strings.Count(partial, `class="button is-small post-action quote-btn"`); c != 1 {
		t.Errorf("HTMX partial: want exactly 1 quote button on the appended post, got %d", c)
	}

	// 3. The user card for a mentioned user still resolves (the link target).
	req = httptest.NewRequest(http.MethodGet, "/users/"+alice.Username, nil)
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected mention target /users/%s to return 200, got %d", alice.Username, rr.Code)
	}

	// 4. Unknown mention target 404s (documented link-all consequence).
	req = httptest.NewRequest(http.MethodGet, "/users/nobody", nil)
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected unknown mention target /users/nobody to 404, got %d", rr.Code)
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

	// Create a thread that is already expired (expires_at in past)
	pastTime := time.Now().UTC().Add(-1 * time.Hour).Round(0)
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID:     u.ID,
		Title:      "Expired Thread",
		ExpiresAt:  sql.NullTime{Time: pastTime, Valid: true},
		TtlSeconds: sql.NullInt64{Int64: 3600, Valid: true},
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

// TestFrontPageExpiryBadge pins the clutter rule: the TTL badge only appears
// on front-page rows whose thread expires within the urgency window; threads
// with days left show no badge at all.
func TestFrontPageExpiryBadge(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()

	u, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "dora",
		PasswordHash: "fakehash",
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	token, err := auth.GenerateSessionToken()
	if err != nil {
		t.Fatalf("failed to generate session token: %v", err)
	}
	if err := queries.CreateSession(ctx, database.CreateSessionParams{
		TokenHash: auth.TokenHash(token),
		UserID:    u.ID,
		ExpiresAt: time.Now().UTC().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	newThread := func(title string, expiry time.Time) {
		t.Helper()
		th, err := queries.CreateThread(ctx, database.CreateThreadParams{
			UserID:     u.ID,
			Title:      title,
			ExpiresAt:  sql.NullTime{Time: expiry, Valid: true},
			TtlSeconds: sql.NullInt64{Valid: false},
		})
		if err != nil {
			t.Fatalf("failed to create thread %q: %v", title, err)
		}
		if _, err := queries.CreatePost(ctx, database.CreatePostParams{
			ThreadID: th.ID,
			UserID:   u.ID,
			Body:     "hello",
		}); err != nil {
			t.Fatalf("failed to create opening post: %v", err)
		}
	}

	newThread("urgent thread", time.Now().Add(5*time.Hour))
	newThread("long-lived thread", time.Now().Add(72*time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token, Path: "/"})
	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected front page 200, got %d", rr.Code)
	}
	page := rr.Body.String()

	for _, title := range []string{"urgent thread", "long-lived thread"} {
		if !strings.Contains(page, title) {
			t.Fatalf("front page missing %q", title)
		}
	}
	if c := strings.Count(page, "&#x23F3;"); c != 1 {
		t.Errorf("want exactly 1 expiry badge (only the 5h thread), got %d", c)
	}
	if !strings.Contains(page, "in 4h") {
		t.Errorf("urgent thread: want 'in 4h' badge text, missing")
	}
	if strings.Contains(page, "in 3d") {
		t.Errorf("long-lived thread: badge must be omitted, found 'in 3d'")
	}
}

// TestFooterBuildCommit pins the footer: the default "dev" stamp is hidden,
// a stamped commit renders as a link to the public repository.
func TestFooterBuildCommit(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()

	u, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "erin",
		PasswordHash: "fakehash",
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	token, err := auth.GenerateSessionToken()
	if err != nil {
		t.Fatalf("failed to generate session token: %v", err)
	}
	if err := queries.CreateSession(ctx, database.CreateSessionParams{
		TokenHash: auth.TokenHash(token),
		UserID:    u.ID,
		ExpiresAt: time.Now().UTC().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token, Path: "/"})

	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected front page 200, got %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "github.com/kzrl/disjoint/commit/") {
		t.Errorf("footer must not show a commit link for the default dev build")
	}

	buildinfo.Commit = "abc1234"
	t.Cleanup(func() { buildinfo.Commit = "dev" })

	rr = httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected front page 200, got %d", rr.Code)
	}
	want := `<a class="is-size-7" href="https://github.com/kzrl/disjoint/commit/abc1234" title="View this build's commit">abc1234</a>`
	if !strings.Contains(rr.Body.String(), want) {
		t.Errorf("footer: want commit link %s, missing", want)
	}
}

// TestReplyBumpsExpiration verifies the TTL reset rule: every new post pushes
// the thread's expiry deadline out to last-comment + ttl, while never-threads
// keep a NULL deadline.
func TestReplyBumpsExpiration(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	user, _ := queries.CreateUser(ctx, database.CreateUserParams{Username: "bump", PasswordHash: "h"})
	authCookie := authCookieFor(t, queries, user.ID)

	ttl := time.Hour
	created := time.Now().UTC().Round(0)
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID:     user.ID,
		Title:      "Bump Thread",
		ExpiresAt:  sql.NullTime{Time: created.Add(ttl), Valid: true},
		TtlSeconds: sql.NullInt64{Int64: int64(ttl.Seconds()), Valid: true},
	})
	if err != nil {
		t.Fatalf("failed to create thread: %v", err)
	}

	// A never-expiring control thread.
	never, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID:    user.ID,
		Title:     "Never Thread",
		ExpiresAt: sql.NullTime{Valid: false},
	})
	if err != nil {
		t.Fatalf("failed to create never thread: %v", err)
	}

	reply := func(threadID int64) {
		t.Helper()
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		_ = w.WriteField("body", "a reply")
		_ = w.Close()
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/threads/%d/posts", threadID), &b)
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.AddCookie(authCookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("expected reply redirect, got %d, body: %s", rr.Code, rr.Body.String())
		}
	}

	before, err := queries.GetThreadByID(ctx, thread.ID)
	if err != nil {
		t.Fatalf("failed to load thread: %v", err)
	}
	if !before.ExpiresAt.Valid || !before.ExpiresAt.Time.Equal(created.Add(ttl)) {
		t.Fatalf("expected initial deadline at created+ttl, got %v", before.ExpiresAt.Time)
	}

	time.Sleep(1200 * time.Millisecond) // make the bump observable
	reply(thread.ID)

	after, err := queries.GetThreadByID(ctx, thread.ID)
	if err != nil {
		t.Fatalf("failed to load thread after reply: %v", err)
	}
	if !after.ExpiresAt.Valid || !after.ExpiresAt.Time.After(before.ExpiresAt.Time) {
		t.Fatalf("expected expiry bumped forward, got %v (was %v)", after.ExpiresAt.Time, before.ExpiresAt.Time)
	}
	// The new deadline must be reply+ttl, i.e. strictly after created+ttl.
	if after.ExpiresAt.Time.Before(created.Add(ttl + time.Second)) {
		t.Fatalf("expected new deadline ~now+ttl, got %v", after.ExpiresAt.Time)
	}

	reply(never.ID)
	afterNever, err := queries.GetThreadByID(ctx, never.ID)
	if err != nil {
		t.Fatalf("failed to load never thread: %v", err)
	}
	if afterNever.ExpiresAt.Valid {
		t.Fatalf("never-thread must keep NULL expires_at, got %v", afterNever.ExpiresAt.Time)
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
	authCookie := authCookieFor(t, queries, user.ID)

	thread, _ := queries.CreateThread(ctx, database.CreateThreadParams{
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

	adminCookie := authCookieFor(t, queries, admin.ID)
	userCookie := authCookieFor(t, queries, regular.ID)

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
	cookie := authCookieFor(t, queries, user.ID)

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
	cookie := authCookieFor(t, queries, user.ID)

	// Seed: 2 attributed posts
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: user.ID,
		Title:  "card thread", ExpiresAt: sql.NullTime{},
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
	cookie := authCookieFor(t, queries, user.ID)

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
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: user.ID,
		Title:  "avatar thread", ExpiresAt: sql.NullTime{},
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
	// The card now falls back to the default pixel avatar after removal.
	if got := cardHTML(); !strings.Contains(got, `src="/avatars/avatar_user?v=0"`) || strings.Contains(got, "/uploads/"+p) {
		t.Errorf("card should show the default pixel avatar after removal, not the removed upload:\n%s", got)
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

func postThreadWithFile(t *testing.T, router http.Handler, cookie *http.Cookie, filename string, content []byte) *httptest.ResponseRecorder {
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

	req := httptest.NewRequest(http.MethodPost, "/threads/new", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// ---- Default pixel avatars ----

// TestDefaultPixelAvatars verifies the /avatars/{name} fallback route: it
// renders a deterministic pixel-art SVG for users without an upload, 404s
// for unknown users, 302s to the upload once one exists, and appears in the
// templates that show author avatars.
func TestDefaultPixelAvatars(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	user, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "pixel_user", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)

	getAvatar := func() *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/avatars/pixel_user", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	// 1. No upload yet: 200 with an SVG, cached for a day, deterministic.
	rr := getAvatar()
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for default avatar, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("Content-Type = %q, want image/svg+xml", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "public") || !strings.Contains(cc, "max-age=86400") {
		t.Errorf("Cache-Control = %q, want public max-age=86400", cc)
	}
	body := rr.Body.String()
	if !strings.HasPrefix(body, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"`) {
		t.Errorf("unexpected SVG body: %.80s…", body)
	}
	if got := getAvatar().Body.String(); got != body {
		t.Error("avatar SVG is not deterministic across requests")
	}

	// 2. Unknown user: 404 (the route is not a free render endpoint).
	req := httptest.NewRequest(http.MethodGet, "/avatars/nosuchuser", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown user, got %d", rr.Code)
	}

	// 3. User card shows the default avatar (big) and the edit-form preview.
	req = httptest.NewRequest(http.MethodGet, "/users/pixel_user", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("user card returned %d", rr.Code)
	}
	page := rr.Body.String()
	if !strings.Contains(page, `class="avatar avatar-lg" src="/avatars/pixel_user?v=0"`) {
		t.Errorf("user card did not show the default avatar-lg img:\n%s", page)
	}
	if !strings.Contains(page, `Your default avatar:`) {
		t.Errorf("user card edit form did not show the default avatar preview")
	}

	// 4. Thread page shows the small default avatar next to the author.
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: user.ID,
		Title:  "pixel thread", ExpiresAt: sql.NullTime{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: thread.ID, UserID: user.ID, Body: "hello"}); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/threads/"+strconv.FormatInt(thread.ID, 10), nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("thread page returned %d", rr.Code)
	}
	if page := rr.Body.String(); !strings.Contains(page, `class="avatar avatar-sm" src="/avatars/pixel_user?v=0"`) {
		t.Errorf("thread page did not show the default avatar-sm img:\n%s", page)
	}

	// 5. Shuffle: sets a fresh nonce; the rendered sprite and the ?v= URL
	// in the templates change with it.
	rr = getAvatar()
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 before shuffle, got %d", rr.Code)
	}
	before := rr.Body.String()

	req = httptest.NewRequest(http.MethodPost, "/users/me/profile", strings.NewReader("shuffle_avatar=on"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 after shuffle, got %d", rr.Code)
	}
	shuffled, err := queries.GetUserByUsername(ctx, "pixel_user")
	if err != nil {
		t.Fatal(err)
	}
	if shuffled.AvatarNonce == 0 {
		t.Fatal("shuffle did not set a nonce")
	}
	rr = getAvatar()
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 after shuffle, got %d", rr.Code)
	}
	if after := rr.Body.String(); after == before {
		t.Error("shuffled avatar rendered identically to the original roll")
	}
	req = httptest.NewRequest(http.MethodGet, "/users/pixel_user", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("user card after shuffle returned %d", rr.Code)
	}
	wantV := "?v=" + strconv.FormatInt(shuffled.AvatarNonce, 10)
	if page := rr.Body.String(); !strings.Contains(page, `/avatars/pixel_user`+wantV) {
		t.Errorf("user card does not carry the new nonce in the avatar URL:\n%s", page)
	}

	// 6. After an upload the route 302s to the uploaded file.
	bodyBuf, ctype := profileMultipart(t, map[string]string{"bio": "hi"}, "me.png", pngBytes)
	req = httptest.NewRequest(http.MethodPost, "/users/me/profile", bodyBuf)
	req.Header.Set("Content-Type", ctype)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 after avatar upload, got %d", rr.Code)
	}
	rr = getAvatar()
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302 after upload, got %d", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "/uploads/") || !strings.Contains(loc, ".png") {
		t.Errorf("redirect Location = %q, want /uploads/….png", loc)
	}
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
	authCookie := authCookieFor(t, queries, user.ID)

	// 1. Detected HTML content is rejected outright, with no thread left
	// behind and no file on disk.
	var threadsBefore int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&threadsBefore); err != nil {
		t.Fatalf("failed to count threads: %v", err)
	}
	rr := postThreadWithFile(t, router, authCookie, "evil.html",
		[]byte("<html><body><script>alert(1)</script></body></html>"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect back to the form, got %d", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "/threads/new") {
		t.Fatalf("expected redirect to the thread form, got %s", loc)
	}
	var nAfter int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&nAfter); err != nil {
		t.Fatalf("failed to count threads: %v", err)
	}
	if nAfter != threadsBefore {
		t.Fatalf("rejected upload must not create a thread")
	}
	if files := listUploadFiles(t, uploadDir); len(files) != 0 {
		t.Fatalf("rejected upload must not leave a file behind: %v", files)
	}

	// 2. Content that sniffs as text/plain but carries an HTML-ish name
	// must be stored with the .txt extension derived from the sniffed type,
	// never the user-chosen .html — so it can never be served as a page.
	rr = postThreadWithFile(t, router, authCookie, "sneaky.html",
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
	rr = postThreadWithFile(t, router, authCookie, "photo.bin",
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
	rr = postThreadWithFile(t, router, authCookie, "archive.zip",
		[]byte("PK\x03\x04zipzipzip"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect back to the form, got %d", rr.Code)
	}
	var nFinal int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&nFinal); err != nil {
		t.Fatalf("failed to count threads: %v", err)
	}
	if nFinal != threadsBefore+2 {
		t.Fatalf("rejected zip upload must not create a thread")
	}

	// 5. WebP content whose first chunk is not a VP* chunk (here an ICCP
	// color profile) sniffs as octet-stream, so it is only accepted via the
	// RIFF/WEBP magic fallback; stored with the .webp extension, served
	// inline.
	rr = postThreadWithFile(t, router, authCookie, "anim.dat",
		[]byte("RIFF\x24\x00\x00\x00WEBPICCP\x00\x00\x00\x00\x00\x00\x00\x00"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected thread create redirect, got %d", rr.Code)
	}
	var webp string
	for _, f := range listUploadFiles(t, uploadDir) {
		if strings.HasSuffix(f, ".webp") {
			webp = f
		}
	}
	if webp == "" {
		t.Fatalf("expected WebP content stored with .webp extension, got %v", listUploadFiles(t, uploadDir))
	}
	req = httptest.NewRequest(http.MethodGet, "/uploads/"+webp, nil)
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 serving webp, got %d", rr.Code)
	}
	if cd := rr.Header().Get("Content-Disposition"); cd != "inline" {
		t.Fatalf("expected Content-Disposition: inline for webp, got %q", cd)
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

// TestSessionLifecycle verifies server-side sessions: a valid token works,
// logout deletes the row so the token stops working immediately, and an
// expired session is treated as anonymous.
func TestSessionLifecycle(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	hash, _ := auth.HashPassword("password123")
	user, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "erin", PasswordHash: hash})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	cookie := authCookieFor(t, queries, user.ID)

	// A valid session reaches the home page.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with a valid session, got %d", rr.Code)
	}

	// Logout deletes the session row and expires the cookie.
	req = httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected logout redirect, got %d", rr.Code)
	}

	// The old token must no longer authenticate.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected revoked token to be anonymous (redirect to /login), got %d", rr.Code)
	}

	// An expired session is anonymous as well.
	expiredToken, err := auth.GenerateSessionToken()
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	if err := queries.CreateSession(ctx, database.CreateSessionParams{
		TokenHash: auth.TokenHash(expiredToken),
		UserID:    user.ID,
		ExpiresAt: time.Now().UTC().Add(-time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("failed to create expired session: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: expiredToken, Path: "/"})
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected expired session to be anonymous, got %d", rr.Code)
	}
}

func TestLoginRateLimiting(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()

	// A user must exist so the middleware lets /login through to the
	// rate-limited handler instead of redirecting to first-run setup.
	hash, err := auth.HashPassword("password123")
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}
	if _, err := queries.CreateUser(context.Background(), database.CreateUserParams{
		Username:     "admin",
		PasswordHash: hash,
	}); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	// No explicit refresh needed: the middleware lazily re-counts users
	// on every request while it believes the install is empty.

	router := app.Routes()

	body := "username=nobody&password=wrong"
	var last int
	for i := 0; i < 12; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		last = rr.Code
		if rr.Code == http.StatusTooManyRequests {
			t.Logf("rate limit triggered on attempt %d", i+1)
			break
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("expected 429 within 12 attempts, last code %d", last)
	}

	// GET /login must remain reachable while POSTs are limited.
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /login after limit: expected 200, got %d", rr.Code)
	}
}

func TestSecurityHeadersPresent(t *testing.T) {
	app, db, _, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
	}
	for header, value := range want {
		if got := rr.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
}

// realPNG / realGIF encode small genuine images so the emoji upload pipeline's
// sniff + image.Decode + dimension checks run against valid bytes (the
// package-level pngBytes/gifBytes fakes only satisfy the sniffer).
func realPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func realGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gif.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// emojiMultipart builds the /emojis upload form (name field + "file" part).
func emojiMultipart(t *testing.T, name, fileName string, fileBytes []byte) (*bytes.Buffer, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if err := w.WriteField("name", name); err != nil {
		t.Fatal(err)
	}
	if fileBytes != nil {
		fw, err := w.CreateFormFile("file", fileName)
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

func TestCustomEmojiUploads(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	ally, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "emoji_ally", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "emoji_bob", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, ally.ID)
	bobCookie := authCookieFor(t, queries, bob.ID)

	getPage := func(c *http.Cookie) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/emojis", nil)
		if c != nil {
			req.AddCookie(c)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /emojis returned %d", rr.Code)
		}
		return rr.Body.String()
	}

	upload := func(c *http.Cookie, body *bytes.Buffer, ctype string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/emojis", body)
		req.Header.Set("Content-Type", ctype)
		req.AddCookie(c)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	hasFlash := func(rr *httptest.ResponseRecorder, name string) bool {
		t.Helper()
		for _, c := range rr.Result().Cookies() {
			if c.Name == name && c.Value != "" {
				return true
			}
		}
		return false
	}

	deleteEmoji := func(c *http.Cookie, id int64) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/emojis/%d/delete", id), nil)
		req.AddCookie(c)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	fileCount := func() int {
		t.Helper()
		entries, err := os.ReadDir(uploadDir)
		if err != nil {
			t.Fatalf("reading uploadDir: %v", err)
		}
		return len(entries)
	}

	// 1. Unauthenticated: redirect to login; signed-in: page lists built-ins.
	req := httptest.NewRequest(http.MethodGet, "/emojis", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("expected 303 to /login for guest, got %d %q", rr.Code, rr.Header().Get("Location"))
	}
	page := getPage(cookie)
	for _, name := range []string{":joy:", ":party:", "Community Emoji", "Add Emoji"} {
		if !strings.Contains(page, name) {
			t.Errorf("emoji page missing %q", name)
		}
	}
	if got := strings.Count(page, "built-in"); got != 12 {
		t.Errorf("expected 12 built-in tiles, got %d", got)
	}

	// 2. PNG upload succeeds: row, file on disk, rendered tile with uploader.
	body, ctype := emojiMultipart(t, "mario_fire", "mario.png", realPNG(t, 64, 64))
	rr = upload(cookie, body, ctype)
	if rr.Code != http.StatusSeeOther || hasFlash(rr, "flash_error") {
		t.Fatalf("expected clean 303 after PNG upload, got %d (err=%v)", rr.Code, hasFlash(rr, "flash_error"))
	}
	mario, err := queries.GetEmojiByName(ctx, "mario_fire")
	if err != nil {
		t.Fatalf("expected mario_fire row: %v", err)
	}
	if mario.Source != "custom" || !mario.StoragePath.Valid || !strings.HasSuffix(mario.StoragePath.String, ".png") {
		t.Fatalf("unexpected mario_fire row: %+v", mario)
	}
	if _, err := os.Stat(filepath.Join(uploadDir, mario.StoragePath.String)); err != nil {
		t.Fatalf("emoji file not saved: %v", err)
	}
	page = getPage(cookie)
	if !strings.Contains(page, ":mario_fire:") || !strings.Contains(page, "/uploads/"+mario.StoragePath.String) {
		t.Errorf("page did not render mario_fire tile:\n%s", page)
	}
	if !strings.Contains(page, `/users/emoji_ally">emoji_ally</a>`) {
		t.Errorf("page did not attribute mario_fire to emoji_ally")
	}

	// 3. GIF upload succeeds (animated GIFs are the slackmojis.com staple).
	body, ctype = emojiMultipart(t, "star_wink", "star.gif", realGIF(t, 32, 32))
	rr = upload(cookie, body, ctype)
	if rr.Code != http.StatusSeeOther || hasFlash(rr, "flash_error") {
		t.Fatalf("expected clean 303 after GIF upload, got %d", rr.Code)
	}
	if _, err := queries.GetEmojiByName(ctx, "star_wink"); err != nil {
		t.Fatalf("expected star_wink row: %v", err)
	}

	// 4. Duplicate name: rejected, row and disk untouched.
	before := fileCount()
	body, ctype = emojiMultipart(t, "mario_fire", "again.png", realPNG(t, 8, 8))
	rr = upload(cookie, body, ctype)
	if !hasFlash(rr, "flash_error") {
		t.Fatalf("expected flash_error for duplicate name")
	}
	again, err := queries.GetEmojiByName(ctx, "mario_fire")
	if err != nil || again.StoragePath.String != mario.StoragePath.String {
		t.Fatalf("duplicate upload must not replace the existing emoji: %+v (err %v)", again, err)
	}
	if got := fileCount(); got != before {
		t.Errorf("rejected upload left a file behind: %d -> %d", before, got)
	}

	// 5. Invalid name: rejected, no row.
	body, ctype = emojiMultipart(t, "Bad-Name", "x.png", realPNG(t, 8, 8))
	rr = upload(cookie, body, ctype)
	if !hasFlash(rr, "flash_error") {
		t.Fatalf("expected flash_error for invalid name")
	}
	if _, err := queries.GetEmojiByName(ctx, "bad-name"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected no row for invalid name, got %v", err)
	}

	// 6. Non-image upload: rejected, nothing written to disk.
	before = fileCount()
	body, ctype = emojiMultipart(t, "not_image", "evil.html", txtBytes)
	rr = upload(cookie, body, ctype)
	if !hasFlash(rr, "flash_error") {
		t.Fatalf("expected flash_error for non-image upload")
	}
	if _, err := queries.GetEmojiByName(ctx, "not_image"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected no row for non-image upload, got %v", err)
	}
	if got := fileCount(); got != before {
		t.Errorf("rejected non-image left a file behind: %d -> %d", before, got)
	}

	// 7. Oversized dimensions: rejected.
	body, ctype = emojiMultipart(t, "too_wide", "wide.png", realPNG(t, 300, 10))
	rr = upload(cookie, body, ctype)
	if !hasFlash(rr, "flash_error") {
		t.Fatalf("expected flash_error for oversized image")
	}
	if _, err := queries.GetEmojiByName(ctx, "too_wide"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected no row for oversized image, got %v", err)
	}

	// 8. Oversized file: rejected by the size cap (before sniffing).
	big := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, make([]byte, 300<<10)...)
	body, ctype = emojiMultipart(t, "too_big", "big.png", big)
	rr = upload(cookie, body, ctype)
	if !hasFlash(rr, "flash_error") {
		t.Fatalf("expected flash_error for oversized file")
	}
	if _, err := queries.GetEmojiByName(ctx, "too_big"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected no row for oversized file, got %v", err)
	}

	// 9. Deletion permissions: stranger no, built-in never, owner yes.
	rr = deleteEmoji(bobCookie, mario.ID)
	if !hasFlash(rr, "flash_error") {
		t.Fatalf("expected flash_error when stranger deletes someone else's emoji")
	}
	if _, err := queries.GetEmojiByID(ctx, mario.ID); err != nil {
		t.Fatalf("mario_fire should still exist: %v", err)
	}
	rr = deleteEmoji(cookie, 1) // built-in row (first seeded ID)
	if !hasFlash(rr, "flash_error") {
		t.Fatalf("expected flash_error deleting a built-in")
	}
	if _, err := queries.GetEmojiByID(ctx, 1); err != nil {
		t.Fatalf("built-in emoji must not be deletable: %v", err)
	}
	star, err := queries.GetEmojiByName(ctx, "star_wink")
	if err != nil {
		t.Fatal(err)
	}
	starFile := filepath.Join(uploadDir, star.StoragePath.String)
	rr = deleteEmoji(cookie, star.ID)
	if rr.Code != http.StatusSeeOther || hasFlash(rr, "flash_error") {
		t.Fatalf("expected clean delete by owner, got %d", rr.Code)
	}
	if _, err := queries.GetEmojiByID(ctx, star.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected star_wink row gone, got %v", err)
	}
	if _, err := os.Stat(starFile); !os.IsNotExist(err) {
		t.Errorf("expected star_wink file removed, stat err=%v", err)
	}

	// 10. Admin can delete another user's emoji.
	if err := queries.SetUserAdmin(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	rr = deleteEmoji(bobCookie, mario.ID)
	if rr.Code != http.StatusSeeOther || hasFlash(rr, "flash_error") {
		t.Fatalf("expected clean admin delete, got %d", rr.Code)
	}
	if _, err := queries.GetEmojiByID(ctx, mario.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected mario_fire row gone after admin delete, got %v", err)
	}

	// 11. Deleting an emoji cascades its reactions away.
	body, ctype = emojiMultipart(t, "cascade_emoji", "c.png", realPNG(t, 16, 16))
	upload(cookie, body, ctype)
	cascade, err := queries.GetEmojiByName(ctx, "cascade_emoji")
	if err != nil {
		t.Fatal(err)
	}
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: ally.ID, Title: "emoji cascade",
		ExpiresAt: sql.NullTime{}, TtlSeconds: sql.NullInt64{},
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: thread.ID, UserID: ally.ID, Body: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AddReaction(ctx, database.AddReactionParams{PostID: post.ID, UserID: ally.ID, EmojiID: cascade.ID}); err != nil {
		t.Fatal(err)
	}
	if n, _ := queries.ListReactionsByThreadID(ctx, thread.ID); len(n) != 1 {
		t.Fatalf("expected 1 reaction before emoji delete, got %d", len(n))
	}
	rr = deleteEmoji(cookie, cascade.ID)
	if rr.Code != http.StatusSeeOther || hasFlash(rr, "flash_error") {
		t.Fatalf("expected clean delete, got %d", rr.Code)
	}
	if n, err := queries.ListReactionsByThreadID(ctx, thread.ID); err != nil || len(n) != 0 {
		t.Errorf("expected reactions to cascade away with the emoji, got %d (err %v)", len(n), err)
	}
}

func TestReactionToggles(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	_ = uploadDir
	router := app.Routes()
	ctx := context.Background()

	ally, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "react_ally", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "react_bob", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	allyCookie := authCookieFor(t, queries, ally.ID)
	bobCookie := authCookieFor(t, queries, bob.ID)

	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: ally.ID, Title: "reaction thread",
		ExpiresAt: sql.NullTime{}, TtlSeconds: sql.NullInt64{},
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: thread.ID, UserID: ally.ID, Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}

	joy, err := queries.GetEmojiByName(ctx, "joy")
	if err != nil {
		t.Fatal(err)
	}

	toggle := func(c *http.Cookie, postID int64, emojiID int64, htmx bool) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{}
		form.Set("emoji_id", strconv.FormatInt(emojiID, 10))
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/posts/%d/reactions", postID), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if c != nil {
			req.AddCookie(c)
		}
		if htmx {
			req.Header.Set("HX-Request", "true")
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	reactionCount := func(postID int64) int {
		t.Helper()
		n, err := queries.ListReactionsByPostID(ctx, postID)
		if err != nil {
			t.Fatal(err)
		}
		return len(n)
	}

	// 1. HTMX request without a session: 401 + HX-Redirect to login.
	rr := toggle(nil, post.ID, joy.ID, true)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for anonymous HTMX toggle, got %d", rr.Code)
	}
	if loc := rr.Header().Get("HX-Redirect"); loc != "/login" {
		t.Fatalf("expected HX-Redirect /login, got %q", loc)
	}

	// 2. Standard (non-HTMX) request without a session: redirect to login.
	rr = toggle(nil, post.ID, joy.ID, false)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("expected 303 to /login for anonymous toggle, got %d %q", rr.Code, rr.Header().Get("Location"))
	}

	// 3. Toggle on: bar renders an active chip with count 1.
	rr = toggle(allyCookie, post.ID, joy.ID, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 from HTMX toggle, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`class="reaction-chip is-active"`,
		`hx-vals='{"emoji_id": ` + strconv.FormatInt(joy.ID, 10) + `}'`,
		`>1<`,                                  // count
		`title="Reacted by: react_ally (you)"`, // hover tooltip lists the reactant
	} {
		if !strings.Contains(body, want) {
			t.Errorf("toggled-on bar missing %q in:\n%s", want, body)
		}
	}
	if n := reactionCount(post.ID); n != 1 {
		t.Errorf("expected 1 reaction row after toggle-on, got %d", n)
	}

	// 4. Toggle off: the same click removes it again.
	rr = toggle(allyCookie, post.ID, joy.ID, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 from toggle-off, got %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "is-active") {
		t.Errorf("expected no active chip after toggle-off:\n%s", rr.Body.String())
	}
	if n := reactionCount(post.ID); n != 0 {
		t.Errorf("expected 0 reaction rows after toggle-off, got %d", n)
	}

	// 5. Second user reacts: count 2, active only for the requesting user.
	toggle(allyCookie, post.ID, joy.ID, true)
	rr = toggle(bobCookie, post.ID, joy.ID, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body = rr.Body.String()
	if !strings.Contains(body, ">2<") || !strings.Contains(body, "Reacted by: react_ally, react_bob (you)") {
		t.Errorf("expected count 2 and both reactants in the tooltip:\n%s", body)
	}
	if !strings.Contains(body, `class="reaction-chip is-active"`) {
		t.Errorf("expected active chip for the requesting user (bob):\n%s", body)
	}
	// The same bar rendered for ally must also be active (ally reacted too),
	// and both views agree on the count.
	rr = toggle(allyCookie, post.ID, joy.ID, true) // ally toggles OFF
	if n := reactionCount(post.ID); n != 1 {
		t.Errorf("expected 1 reaction row after ally toggled off, got %d", n)
	}

	// 6. Custom emoji renders as an <img> chip, not a glyph.
	png, ctype := emojiMultipart(t, "react_custom", "c.png", realPNG(t, 24, 24))
	ur := httptest.NewRequest(http.MethodPost, "/emojis", png)
	ur.Header.Set("Content-Type", ctype)
	ur.AddCookie(allyCookie)
	urr := httptest.NewRecorder()
	router.ServeHTTP(urr, ur)
	if urr.Code != http.StatusSeeOther {
		t.Fatalf("emoji upload failed: %d", urr.Code)
	}
	custom, err := queries.GetEmojiByName(ctx, "react_custom")
	if err != nil {
		t.Fatal(err)
	}
	rr = toggle(bobCookie, post.ID, custom.ID, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 toggling custom emoji, got %d", rr.Code)
	}
	body = rr.Body.String()
	if !strings.Contains(body, `<img class="emoji-img" src="/uploads/`+custom.StoragePath.String) {
		t.Errorf("custom emoji chip did not render an <img>:\n%s", body)
	}
	if !strings.Contains(body, `title="Reacted by: react_bob (you)"`) {
		t.Errorf("custom emoji chip missing reactant tooltip:\n%s", body)
	}

	// 7. Unknown post / unknown emoji: 404.
	rr = toggle(allyCookie, 99999, joy.ID, true)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown post, got %d", rr.Code)
	}
	rr = toggle(allyCookie, post.ID, 99999, true)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown emoji, got %d", rr.Code)
	}
}

func TestThreadPageReactionBars(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	ally, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "bar_ally", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "bar_bob", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	allyCookie := authCookieFor(t, queries, ally.ID)
	bobCookie := authCookieFor(t, queries, bob.ID)

	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: ally.ID, Title: "bar thread",
		ExpiresAt: sql.NullTime{}, TtlSeconds: sql.NullInt64{},
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: thread.ID, UserID: ally.ID, Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}

	// ally reacts with built-in joy and a custom emoji; bob with joy only.
	joy, err := queries.GetEmojiByName(ctx, "joy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AddReaction(ctx, database.AddReactionParams{PostID: post.ID, UserID: ally.ID, EmojiID: joy.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AddReaction(ctx, database.AddReactionParams{PostID: post.ID, UserID: bob.ID, EmojiID: joy.ID}); err != nil {
		t.Fatal(err)
	}
	custom, err := queries.CreateEmoji(ctx, database.CreateEmojiParams{
		Name: "bar_custom", Source: "custom",
		StoragePath: sql.NullString{String: "bar-custom.png", Valid: true},
		MimeType:    sql.NullString{String: "image/png", Valid: true},
		FileSize:    sql.NullInt64{Int64: 1, Valid: true},
		CreatedBy:   sql.NullInt64{Int64: ally.ID, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AddReaction(ctx, database.AddReactionParams{PostID: post.ID, UserID: ally.ID, EmojiID: custom.ID}); err != nil {
		t.Fatal(err)
	}

	getThread := func(c *http.Cookie) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/threads/%d", thread.ID), nil)
		if c != nil {
			req.AddCookie(c)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET thread returned %d", rr.Code)
		}
		return rr.Body.String()
	}

	// 1. ally's view: both of her chips active, custom chip is an <img>,
	//    counts grouped (joy = 2), picker carries the full emoji list.
	page := getThread(allyCookie)
	if got := strings.Count(page, `class="reaction-chip is-active"`); got != 2 {
		t.Errorf("expected 2 active chips for ally, got %d", got)
	}
	if !strings.Contains(page, `title="Reacted by: bar_ally, bar_bob (you)"`) {
		t.Errorf("joy chip tooltip should list both reactants and mark the viewer")
	}
	if !strings.Contains(page, `<img class="emoji-img" src="/uploads/bar-custom.png"`) {
		t.Errorf("custom reaction chip missing <img>")
	}
	if !strings.Contains(page, `title="Reacted by: bar_ally (you)"`) {
		t.Errorf("custom chip tooltip should list only ally")
	}
	if !strings.Contains(page, `title=":joy:"`) || !strings.Contains(page, `title=":party:"`) || !strings.Contains(page, `title=":bar_custom:"`) {
		t.Errorf("picker missing emoji entries")
	}
	if !strings.Contains(page, `hx-post="/posts/`+strconv.FormatInt(post.ID, 10)+`/reactions"`) {
		t.Errorf("chips missing hx-post to the post's reaction endpoint")
	}

	// 2. bob's view: only joy active (custom is ally's), same counts.
	page = getThread(bobCookie)
	if got := strings.Count(page, `class="reaction-chip is-active"`); got != 1 {
		t.Errorf("expected 1 active chip for bob, got %d", got)
	}
	if !strings.Contains(page, `title="Reacted by: bar_ally, bar_bob (you)"`) {
		t.Errorf("bob's view should list the same reactants with himself marked")
	}

	// 3. HTMX reply: the appended post card carries a reaction bar with a
	//    working picker (no reactions yet on the new post).
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if err := w.WriteField("body", "a reply"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/threads/%d/posts", thread.ID), &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("HX-Request", "true")
	req.AddCookie(allyCookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("HTMX reply returned %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `class="reaction-bar"`) {
		t.Errorf("appended post missing reaction bar")
	}
	if !strings.Contains(body, `class="emoji-picker is-hidden"`) || !strings.Contains(body, `title=":joy:"`) {
		t.Errorf("appended post's bar missing working picker")
	}
}

func TestFrontPageReactions(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	ally, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "fp_ally", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "fp_bob", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	allyCookie := authCookieFor(t, queries, ally.ID)
	bobCookie := authCookieFor(t, queries, bob.ID)

	// Two threads; the first gets reactions on its opening post.
	threadA, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: ally.ID, Title: "fp thread a",
		ExpiresAt: sql.NullTime{}, TtlSeconds: sql.NullInt64{},
	})
	if err != nil {
		t.Fatal(err)
	}
	postA, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: threadA.ID, UserID: ally.ID, Body: "opening A"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: bob.ID, Title: "fp thread b",
		ExpiresAt: sql.NullTime{}, TtlSeconds: sql.NullInt64{},
	}); err != nil {
		t.Fatal(err)
	}

	// ally: joy + a custom emoji; bob: joy.
	joy, err := queries.GetEmojiByName(ctx, "joy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AddReaction(ctx, database.AddReactionParams{PostID: postA.ID, UserID: ally.ID, EmojiID: joy.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AddReaction(ctx, database.AddReactionParams{PostID: postA.ID, UserID: bob.ID, EmojiID: joy.ID}); err != nil {
		t.Fatal(err)
	}
	custom, err := queries.CreateEmoji(ctx, database.CreateEmojiParams{
		Name: "fp_custom", Source: "custom",
		StoragePath: sql.NullString{String: "fp-custom.png", Valid: true},
		MimeType:    sql.NullString{String: "image/png", Valid: true},
		FileSize:    sql.NullInt64{Int64: 1, Valid: true},
		CreatedBy:   sql.NullInt64{Int64: ally.ID, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AddReaction(ctx, database.AddReactionParams{PostID: postA.ID, UserID: ally.ID, EmojiID: custom.ID}); err != nil {
		t.Fatal(err)
	}

	getFront := func(c *http.Cookie) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if c != nil {
			req.AddCookie(c)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET / returned %d", rr.Code)
		}
		return rr.Body.String()
	}

	// 1. ally's front page: thread A's row carries both of her chips
	//    (active), the grouped joy count, and a full picker; thread B's
	//    row has a bare bar.
	page := getFront(allyCookie)
	if got := strings.Count(page, `class="reaction-chip is-active"`); got != 2 {
		t.Errorf("expected 2 active chips on the front page, got %d", got)
	}
	if !strings.Contains(page, `title="Reacted by: fp_ally, fp_bob (you)"`) {
		t.Errorf("front-page joy chip tooltip should list both reactants")
	}
	if !strings.Contains(page, `<img class="emoji-img" src="/uploads/fp-custom.png"`) {
		t.Errorf("custom reaction chip missing <img>")
	}
	if !strings.Contains(page, `hx-post="/posts/`+strconv.FormatInt(postA.ID, 10)+`/reactions"`) {
		t.Errorf("front-page chips missing hx-post to the opening post")
	}
	// Both rows carry a picker (thread B has no chips but can still react).
	if got := strings.Count(page, `class="emoji-picker is-hidden"`); got != 2 {
		t.Errorf("expected a picker on both rows, got %d", got)
	}
	if !strings.Contains(page, `title=":fp_custom:"`) {
		t.Errorf("picker missing the custom emoji entry")
	}

	// 2. bob's view: same counts, only joy active.
	page = getFront(bobCookie)
	if got := strings.Count(page, `class="reaction-chip is-active"`); got != 1 {
		t.Errorf("expected 1 active chip for bob, got %d", got)
	}
	if !strings.Contains(page, `title="Reacted by: fp_ally, fp_bob (you)"`) {
		t.Errorf("bob's front-page view should list the same reactants with himself marked")
	}

	// 3. Toggling from the front page uses the same endpoint and returns
	//    the bar for the swap.
	form := url.Values{"emoji_id": {strconv.FormatInt(custom.ID, 10)}}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/posts/%d/reactions", postA.ID),
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.AddCookie(allyCookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `class="reaction-bar"`) {
		t.Errorf("toggle from the front page failed: %d %s", rr.Code, rr.Body.String())
	}
}

func TestFrontPageImageStrips(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	hash, _ := auth.HashPassword("password123")
	user, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "strip_user",
		PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	authCookie := authCookieFor(t, queries, user.ID)

	// Minimal bytes that sniff as image/png via http.DetectContentType.
	pngBytes := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}

	// threadWithFiles creates a thread whose opening post carries the given
	// files and returns its id (map order is random; names are unique).
	threadWithFiles := func(title string, files map[string][]byte) string {
		t.Helper()
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		_ = w.WriteField("title", title)
		_ = w.WriteField("body", "opening post")
		_ = w.WriteField("expiration", "never")
		for name, data := range files {
			part, err := w.CreateFormFile("files", name)
			if err != nil {
				t.Fatalf("failed to create form file: %v", err)
			}
			_, _ = part.Write(data)
		}
		_ = w.Close()
		req := httptest.NewRequest(http.MethodPost, "/threads/new", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.AddCookie(authCookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("expected thread create redirect, got %d, body: %s", rr.Code, rr.Body.String())
		}
		return strings.TrimPrefix(rr.Header().Get("Location"), "/threads/")
	}

	// A: two images + one text file (text is not in the strip).
	threadA := threadWithFiles("strip a", map[string][]byte{
		"a-1.png":   pngBytes,
		"a-2.png":   pngBytes,
		"notes.txt": []byte("plain text, not an image"),
	})
	// B: body only — no strip, even after an image reply.
	threadB := threadWithFiles("strip b", nil)
	// C: nine images — capped at 8 thumbnails + a "+1" chip.
	cFiles := make(map[string][]byte, 9)
	for i := 0; i < 9; i++ {
		cFiles[fmt.Sprintf("c-%d.png", i)] = pngBytes
	}
	threadC := threadWithFiles("strip c", cFiles)

	// An image on a *reply* must not appear in B's strip: the strip shows
	// opening-post attachments only.
	var replyBuf bytes.Buffer
	rw := multipart.NewWriter(&replyBuf)
	_ = rw.WriteField("body", "a photo in the replies")
	part, err := rw.CreateFormFile("files", "reply.png")
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	_, _ = part.Write(pngBytes)
	_ = rw.Close()
	req := httptest.NewRequest(http.MethodPost, "/threads/"+threadB+"/posts", &replyBuf)
	req.Header.Set("Content-Type", rw.FormDataContentType())
	req.Header.Set("HX-Request", "true")
	req.AddCookie(authCookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected reply 200, got %d, body: %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / returned %d", rr.Code)
	}
	page := rr.Body.String()

	// Two strips (A and C), 10 thumbnails (2 + capped 8), one "+1" chip.
	if got := strings.Count(page, `class="front-strip"`); got != 2 {
		t.Errorf("expected 2 image strips, got %d", got)
	}
	if got := strings.Count(page, `class="front-strip-thumb"`); got != 10 {
		t.Errorf("expected 10 thumbnails (2 + capped 8), got %d", got)
	}
	if !strings.Contains(page, `class="front-strip-more" href="/threads/`+threadC+`">+1`) {
		t.Errorf("expected a \"+1\" chip linking into thread C")
	}
	if !strings.Contains(page, `<a class="front-strip-thumb" role="listitem" href="/threads/`+threadA+`">`) {
		t.Errorf("thumbnails on thread A should link into the thread")
	}
	if !strings.Contains(page, `alt="a-1.png"`) || !strings.Contains(page, `alt="a-2.png"`) {
		t.Errorf("expected image filenames in alt text")
	}
	// Non-image attachments and reply images are never in a strip.
	for _, unwanted := range []string{"notes.txt", "reply.png"} {
		if strings.Contains(page, `alt="`+unwanted+`"`) {
			t.Errorf("front-page strip must not contain %s", unwanted)
		}
	}
	// Thread B's row (bounded by the board-row divs) has no strip at all.
	titleIdx := strings.Index(page, "strip b")
	if titleIdx < 0 {
		t.Fatal("thread B missing from the front page")
	}
	rowStart := strings.LastIndex(page[:titleIdx], `<div class="board-row">`)
	rowEnd := strings.Index(page[titleIdx:], `<div class="board-row">`)
	if rowStart < 0 {
		rowStart = 0
	}
	if rowEnd >= 0 {
		rowEnd += titleIdx
	} else {
		rowEnd = len(page)
	}
	if strings.Contains(page[rowStart:rowEnd], "front-strip") {
		t.Errorf("image-less thread B must not render a strip: %s", page[rowStart:rowEnd])
	}
}

func TestPostURLField(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	ally, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "url_ally", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	allyCookie := authCookieFor(t, queries, ally.ID)

	submitThread := func(fields map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		for k, v := range fields {
			if err := w.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/threads/new", &b)
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.AddCookie(allyCookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	// 1. Link-only thread (no body): accepted, link card rendered with
	//    noopener and an escaped href.
	rr := submitThread(map[string]string{
		"title": "link thread",
		"url":   "https://example.com/a?b=1&c=2",
	})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/threads/1" {
		t.Fatalf("link-only thread not created: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	pageReq := httptest.NewRequest(http.MethodGet, "/threads/1", nil)
	pageReq.AddCookie(allyCookie)
	pageRR := httptest.NewRecorder()
	router.ServeHTTP(pageRR, pageReq)
	page := pageRR.Body.String()
	for _, want := range []string{
		`class="post-url"`,
		`href="https://example.com/a?b=1&amp;c=2"`,
		`rel="noopener noreferrer"`,
		`target="_blank"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("thread page missing %q", want)
		}
	}

	// 2. Invalid links are rejected and no thread is created:
	//    javascript: (the XSS case), a non-http(s) scheme, and a bare
	//    hostname without scheme.
	for _, bad := range []string{"javascript:alert(1)", "ftp://example.com/file", "example.com/no-scheme"} {
		rr = submitThread(map[string]string{"title": "bad link", "url": bad})
		if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/threads/new" {
			t.Errorf("invalid url %q: expected redirect back to /threads/new, got %d %q", bad, rr.Code, rr.Header().Get("Location"))
		}
	}
	frontReq := httptest.NewRequest(http.MethodGet, "/", nil)
	frontReq.AddCookie(allyCookie)
	frontRR := httptest.NewRecorder()
	router.ServeHTTP(frontRR, frontReq)
	if strings.Contains(frontRR.Body.String(), "bad link") {
		t.Errorf("a thread was created for an invalid link")
	}

	// 3. Link-only reply (no body, no files) via HTMX: accepted.
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if err := w.WriteField("url", "https://example.org/reply-link"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/threads/1/posts", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("HX-Request", "true")
	req.AddCookie(allyCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("link-only reply rejected: %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `href="https://example.org/reply-link"`) {
		t.Errorf("appended reply missing the link card")
	}

	// 4. Body + link reply: both render.
	b.Reset()
	w = multipart.NewWriter(&b)
	if err := w.WriteField("body", "check this out"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("url", "https://example.org/with-body"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/threads/1/posts", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("HX-Request", "true")
	req.AddCookie(allyCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	body := rr.Body.String()
	if rr.Code != http.StatusOK || !strings.Contains(body, "check this out") || !strings.Contains(body, `href="https://example.org/with-body"`) {
		t.Errorf("body+link reply wrong: %d\n%s", rr.Code, body)
	}

	// 5. Invalid reply link: 400, and an empty reply (no body, link, or
	//    file) stays rejected.
	b.Reset()
	w = multipart.NewWriter(&b)
	if err := w.WriteField("url", "javascript:alert(1)"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/threads/1/posts", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("HX-Request", "true")
	req.AddCookie(allyCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for javascript: reply link, got %d", rr.Code)
	}

	b.Reset()
	w = multipart.NewWriter(&b)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/threads/1/posts", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("HX-Request", "true")
	req.AddCookie(allyCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty reply, got %d", rr.Code)
	}

	// 6. The stored value is the trimmed, verbatim link (query round trip).
	rows, err := queries.ListPostsByThread(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 posts, got %d", len(rows))
	}
	if rows[0].Url != "https://example.com/a?b=1&c=2" {
		t.Errorf("opening post url = %q", rows[0].Url)
	}
	if rows[1].Url != "https://example.org/reply-link" || rows[2].Url != "https://example.org/with-body" {
		t.Errorf("reply urls = %q, %q", rows[1].Url, rows[2].Url)
	}
}

func TestPostEditing(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	author, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "edit_ally", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "edit_other", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	authorCookie := authCookieFor(t, queries, author.ID)
	otherCookie := authCookieFor(t, queries, other.ID)

	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{UserID: author.ID, Title: "editing thread"})
	if err != nil {
		t.Fatal(err)
	}
	post, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID,
		UserID:   author.ID,
		Body:     "original body",
		Url:      sql.NullString{String: "https://example.com/orig", Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	createdAt := post.CreatedAt

	editPost := func(id int64, cookie *http.Cookie, fields map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{}
		for k, v := range fields {
			form.Set(k, v)
		}
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/posts/%d/edit", id), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	// 1. Author edit: body + url updated, updated_at set, created_at kept,
	//    redirect anchors to the post, success flash set.
	rr := editPost(post.ID, authorCookie, map[string]string{
		"body": "revised body",
		"url":  "https://example.com/new",
	})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != fmt.Sprintf("/threads/%d#post-%d", thread.ID, post.ID) {
		t.Fatalf("edit redirect: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if !flashCookieSet(rr, "flash_success", "Post updated.") {
		t.Errorf("missing 'Post updated.' flash")
	}
	p, err := queries.GetPostByID(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Body != "revised body" || p.Url != "https://example.com/new" {
		t.Errorf("post not updated: %q %q", p.Body, p.Url)
	}
	if !p.UpdatedAt.Valid || p.CreatedAt != createdAt {
		t.Errorf("timestamps wrong: created_at %v updated_at %v", p.CreatedAt, p.UpdatedAt)
	}

	// 2. No-op save (identical body+url): succeeds but leaves updated_at
	//    untouched.
	before := p.UpdatedAt.Time
	rr = editPost(post.ID, authorCookie, map[string]string{
		"body": "revised body",
		"url":  "https://example.com/new",
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("no-op save: %d", rr.Code)
	}
	p, _ = queries.GetPostByID(ctx, post.ID)
	if !p.UpdatedAt.Valid || !p.UpdatedAt.Time.Equal(before) {
		t.Errorf("no-op save changed updated_at: %v -> %v", before, p.UpdatedAt)
	}

	// 3. Non-author: 404 (not 403), and the post is untouched.
	rr = editPost(post.ID, otherCookie, map[string]string{"body": "hijacked"})
	if rr.Code != http.StatusNotFound {
		t.Errorf("non-author edit: expected 404, got %d", rr.Code)
	}
	p, _ = queries.GetPostByID(ctx, post.ID)
	if p.Body != "revised body" {
		t.Errorf("non-author edit modified the post: %q", p.Body)
	}

	// 4. Invalid link rejected with 400; stored url unchanged.
	rr = editPost(post.ID, authorCookie, map[string]string{"body": "x", "url": "javascript:alert(1)"})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("javascript: url: expected 400, got %d", rr.Code)
	}
	p, _ = queries.GetPostByID(ctx, post.ID)
	if p.Url != "https://example.com/new" {
		t.Errorf("invalid edit changed url: %q", p.Url)
	}

	// 5. Empty body + empty link rejected when the post has no attachments.
	rr = editPost(post.ID, authorCookie, map[string]string{})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty edit: expected 400, got %d", rr.Code)
	}

	// 6. Empty body + link IS allowed when the post carries attachments
	//    (they are immutable on edit and still satisfy the content rule).
	attPost, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID, UserID: author.ID, Body: "will be cleared",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateAttachment(ctx, database.CreateAttachmentParams{
		PostID: attPost.ID, Filename: "img.png",
		StoragePath: "dummy/img.png", MimeType: "image/png", FileSize: 10,
	}); err != nil {
		t.Fatal(err)
	}
	rr = editPost(attPost.ID, authorCookie, map[string]string{})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("attachment-only edit rejected: %d", rr.Code)
	}
	p, _ = queries.GetPostByID(ctx, attPost.ID)
	if p.Body != "" || p.Url != "" {
		t.Errorf("attachment-only edit left content: %q %q", p.Body, p.Url)
	}

	// 7. Expired thread: not editable (GetThreadByID filters expired rows,
	//    so this surfaces as 404 — same behavior as reply creation).
	expThread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: author.ID, Title: "expired",
		ExpiresAt: sql.NullTime{Time: time.Now().UTC().Add(-time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	expPost, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: expThread.ID, UserID: author.ID, Body: "old"})
	if err != nil {
		t.Fatal(err)
	}
	rr = editPost(expPost.ID, authorCookie, map[string]string{"body": "new"})
	if rr.Code != http.StatusNotFound {
		t.Errorf("expired thread edit: expected 404, got %d", rr.Code)
	}

	// 8. Unauthenticated: redirected to login.
	rr = editPost(post.ID, nil, map[string]string{"body": "anon"})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Errorf("unauthenticated edit: %d %q", rr.Code, rr.Header().Get("Location"))
	}

	// 9. UI: the author's thread page shows the Edit button, the "(edited)"
	//    marker, and the new content; a non-author sees the marker (it is
	//    content metadata) but NOT the Edit button; the edit page prefills
	//    body + url; non-author edit page is a 404.
	render := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	rr = render(fmt.Sprintf("/threads/%d", thread.ID), authorCookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("author thread page: %d", rr.Code)
	}
	page := rr.Body.String()
	for _, want := range []string{
		fmt.Sprintf(`href="/posts/%d/edit"`, post.ID),
		"(edited)",
		"revised body",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("author thread page missing %q", want)
		}
	}

	rr = render(fmt.Sprintf("/threads/%d", thread.ID), otherCookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("non-author thread page: %d", rr.Code)
	}
	page = rr.Body.String()
	if strings.Contains(page, fmt.Sprintf(`/posts/%d/edit`, post.ID)) {
		t.Errorf("non-author thread page shows the Edit button")
	}
	if !strings.Contains(page, "(edited)") {
		t.Errorf("(edited) marker missing for non-author (it is content metadata)")
	}

	rr = render(fmt.Sprintf("/posts/%d/edit", post.ID), authorCookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("author edit page: %d", rr.Code)
	}
	page = rr.Body.String()
	for _, want := range []string{
		fmt.Sprintf(`action="/posts/%d/edit"`, post.ID),
		`value="https://example.com/new"`,
		"revised body",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("edit page missing %q", want)
		}
	}

	rr = render(fmt.Sprintf("/posts/%d/edit", post.ID), otherCookie)
	if rr.Code != http.StatusNotFound {
		t.Errorf("non-author edit page: expected 404, got %d", rr.Code)
	}
}

func TestThreadTitleEditing(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	author, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "title_ally", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "title_other", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	authorCookie := authCookieFor(t, queries, author.ID)
	otherCookie := authCookieFor(t, queries, other.ID)

	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{UserID: author.ID, Title: "original title"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: thread.ID, UserID: author.ID, Body: "hello"}); err != nil {
		t.Fatal(err)
	}

	rename := func(cookie *http.Cookie, title string) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{"title": {title}}
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/threads/%d/title", thread.ID), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	// 1. Author rename: redirect to the thread, success flash, DB updated.
	rr := rename(authorCookie, "  renamed title  ")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != fmt.Sprintf("/threads/%d", thread.ID) {
		t.Fatalf("rename redirect: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if !flashCookieSet(rr, "flash_success", "Title updated.") {
		t.Errorf("missing 'Title updated.' flash")
	}
	got, err := queries.GetThreadByID(ctx, thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "renamed title" {
		t.Errorf("title not trimmed+stored: %q", got.Title)
	}

	// 2. New title renders on the thread page and the front page.
	for _, path := range []string{fmt.Sprintf("/threads/%d", thread.ID), "/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(authorCookie)
		rr = httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "renamed title") {
			t.Errorf("%s missing the renamed title: %d", path, rr.Code)
		}
	}

	// 3. No-op rename: succeeds, title untouched.
	rr = rename(authorCookie, "renamed title")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("no-op rename: %d", rr.Code)
	}
	if !flashCookieSet(rr, "flash_success", "No changes to save.") {
		t.Errorf("missing 'No changes to save.' flash")
	}

	// 4. Empty / whitespace-only titles rejected, title untouched.
	for _, bad := range []string{"", "   "} {
		rr = rename(authorCookie, bad)
		if rr.Code != http.StatusSeeOther {
			t.Errorf("empty title %q: expected redirect, got %d", bad, rr.Code)
		}
		if !flashCookieSet(rr, "flash_error", "Thread title is required.") {
			t.Errorf("empty title %q: missing error flash", bad)
		}
	}
	got, _ = queries.GetThreadByID(ctx, thread.ID)
	if got.Title != "renamed title" {
		t.Errorf("rejected rename changed the title: %q", got.Title)
	}

	// 5. Over-length titles rejected, title untouched.
	rr = rename(authorCookie, strings.Repeat("x", 201))
	if rr.Code != http.StatusSeeOther || !flashCookieSet(rr, "flash_error", "Title is too long (max 200 characters).") {
		t.Errorf("over-length title: %d", rr.Code)
	}
	got, _ = queries.GetThreadByID(ctx, thread.ID)
	if got.Title != "renamed title" {
		t.Errorf("over-length rename changed the title: %q", got.Title)
	}

	// 6. Non-author: 404, title untouched.
	rr = rename(otherCookie, "hijacked")
	if rr.Code != http.StatusNotFound {
		t.Errorf("non-author rename: expected 404, got %d", rr.Code)
	}
	got, _ = queries.GetThreadByID(ctx, thread.ID)
	if got.Title != "renamed title" {
		t.Errorf("non-author rename changed the title: %q", got.Title)
	}

	// 7. Expired thread: not editable (404 via the expiry filter).
	expThread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: author.ID, Title: "expired thread",
		ExpiresAt: sql.NullTime{Time: time.Now().UTC().Add(-time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"title": {"new"}}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/threads/%d/title", expThread.ID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(authorCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expired thread rename: expected 404, got %d", rr.Code)
	}

	// 8. Unauthenticated: redirected to login.
	form = url.Values{"title": {"anon"}}
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/threads/%d/title", thread.ID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Errorf("unauthenticated rename: %d %q", rr.Code, rr.Header().Get("Location"))
	}

	// 9. UI: the author's thread page carries the toggle button and the
	//    prefilled inline form; a non-author's page carries neither.
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/threads/%d", thread.ID), nil)
	req.AddCookie(authorCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("author thread page: %d", rr.Code)
	}
	page := rr.Body.String()
	for _, want := range []string{
		`id="edit-title-btn"`,
		// The form must start hidden (is-hidden, not the hidden attribute:
		// Bulma's .has-addons display:flex would override [hidden]).
		`id="title-edit-form" class="field has-addons mb-1 is-hidden"`,
		fmt.Sprintf(`action="/threads/%d/title"`, thread.ID),
		`name="title"`,
		`value="renamed title"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("author thread page missing %q", want)
		}
	}

	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/threads/%d", thread.ID), nil)
	req.AddCookie(otherCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("non-author thread page: %d", rr.Code)
	}
	page = rr.Body.String()
	for _, absent := range []string{`id="edit-title-btn"`, fmt.Sprintf(`action="/threads/%d/title"`, thread.ID)} {
		if strings.Contains(page, absent) {
			t.Errorf("non-author thread page shows %q", absent)
		}
	}
}

// TestReplyFormFileInput pins the reply form's single, clean Bulma .file
// structure: the native input must carry the file-input class (Bulma
// hides it as an invisible overlay), otherwise the browser's native
// "Browse…" button renders alongside the styled "Choose files…" CTA.
func TestReplyFormFileInput(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	user, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "fileinput", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{UserID: user.ID, Title: "file input thread"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)

	req := httptest.NewRequest(http.MethodGet, "/threads/"+strconv.FormatInt(thread.ID, 10), nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("thread page: %d", rr.Code)
	}
	body := rr.Body.String()

	// Extract the reply form element.
	start := strings.Index(body, `id="reply-form"`)
	if start == -1 {
		t.Fatalf("reply form not found")
	}
	form := body[start:]
	if end := strings.Index(form, "</form>"); end != -1 {
		form = form[:end]
	}

	if got := strings.Count(form, `type="file"`); got != 1 {
		t.Errorf("reply form should contain exactly one file input, got %d", got)
	}
	for _, want := range []string{
		`class="file"`,
		`class="file-label"`,
		`class="file-cta"`,
		`class="file-name"`,
		`class="file-input"`,
	} {
		if !strings.Contains(form, want) {
			t.Errorf("reply form missing %s", want)
		}
	}
}

func TestThreadExpirationEditing(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	author, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "exp_ally", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "exp_other", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	authorCookie := authCookieFor(t, queries, author.ID)
	otherCookie := authCookieFor(t, queries, other.ID)

	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{UserID: author.ID, Title: "expiry test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: thread.ID, UserID: author.ID, Body: "hello"}); err != nil {
		t.Fatal(err)
	}

	setExpiry := func(cookie *http.Cookie, choice string) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{"expiration": {choice}}
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/threads/%d/expiration", thread.ID), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	within := func(got, want time.Time, tol time.Duration) bool {
		return got.Sub(want) < tol && got.Sub(want) > -tol
	}

	// 1. never -> 7d: ttl stored, deadline reset to ~now+7d, success flash.
	rr := setExpiry(authorCookie, "7d")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != fmt.Sprintf("/threads/%d", thread.ID) {
		t.Fatalf("7d change redirect: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if !flashCookieSet(rr, "flash_success", "Expiration updated.") {
		t.Errorf("missing 'Expiration updated.' flash")
	}
	got, err := queries.GetThreadByID(ctx, thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TtlSeconds.Valid || got.TtlSeconds.Int64 != 7*24*3600 {
		t.Errorf("ttl not stored: %+v", got.TtlSeconds)
	}
	if !got.ExpiresAt.Valid || !within(got.ExpiresAt.Time, time.Now().UTC().Add(7*24*time.Hour), 10*time.Second) {
		t.Errorf("deadline not ~now+7d: %v", got.ExpiresAt)
	}
	deadlineBeforeReset := got.ExpiresAt.Time

	// 2. 7d -> 24h: the deadline resets EARLIER (now + 24h), not kept.
	rr = setExpiry(authorCookie, "24h")
	if rr.Code != http.StatusSeeOther || !flashCookieSet(rr, "flash_success", "Expiration updated.") {
		t.Errorf("24h change: %d", rr.Code)
	}
	got, _ = queries.GetThreadByID(ctx, thread.ID)
	if !got.TtlSeconds.Valid || got.TtlSeconds.Int64 != 24*3600 {
		t.Errorf("ttl not updated: %+v", got.TtlSeconds)
	}
	if !got.ExpiresAt.Valid || !within(got.ExpiresAt.Time, time.Now().UTC().Add(24*time.Hour), 10*time.Second) {
		t.Errorf("deadline not ~now+24h: %v", got.ExpiresAt)
	}
	if !got.ExpiresAt.Time.Before(deadlineBeforeReset) {
		t.Errorf("reset did not move the deadline earlier: %v >= %v", got.ExpiresAt.Time, deadlineBeforeReset)
	}

	// 3. -> never: both columns NULL again; header shows "Never expires".
	rr = setExpiry(authorCookie, "never")
	if rr.Code != http.StatusSeeOther || !flashCookieSet(rr, "flash_success", "Expiration updated.") {
		t.Errorf("never change: %d", rr.Code)
	}
	got, _ = queries.GetThreadByID(ctx, thread.ID)
	if got.TtlSeconds.Valid || got.ExpiresAt.Valid {
		t.Errorf("never change left values: ttl=%+v expires=%+v", got.TtlSeconds, got.ExpiresAt)
	}
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/threads/%d", thread.ID), nil)
	req.AddCookie(authorCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Never expires") {
		t.Errorf("thread page after ->never: %d, missing 'Never expires'", rr.Code)
	}

	// 4. Invalid choice rejected, thread untouched.
	rr = setExpiry(authorCookie, "forever")
	if rr.Code != http.StatusSeeOther || !flashCookieSet(rr, "flash_error", "Unknown lifespan.") {
		t.Errorf("invalid choice: %d", rr.Code)
	}
	got, _ = queries.GetThreadByID(ctx, thread.ID)
	if got.TtlSeconds.Valid || got.ExpiresAt.Valid {
		t.Errorf("invalid choice changed the thread: %+v %+v", got.TtlSeconds, got.ExpiresAt)
	}

	// 5. No-op resubmit: success flash, expires_at byte-identical (no reset).
	rr = setExpiry(authorCookie, "24h")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("pre-setup 24h: %d", rr.Code)
	}
	before, _ := queries.GetThreadByID(ctx, thread.ID)
	rr = setExpiry(authorCookie, "24h")
	if rr.Code != http.StatusSeeOther || !flashCookieSet(rr, "flash_success", "No changes to save.") {
		t.Errorf("no-op change: %d", rr.Code)
	}
	after, _ := queries.GetThreadByID(ctx, thread.ID)
	if before.ExpiresAt != after.ExpiresAt || before.TtlSeconds != after.TtlSeconds {
		t.Errorf("no-op resubmit reset the deadline: %v -> %v", before.ExpiresAt, after.ExpiresAt)
	}

	// 6. Non-author: 404, thread untouched.
	rr = setExpiry(otherCookie, "7d")
	if rr.Code != http.StatusNotFound {
		t.Errorf("non-author change: expected 404, got %d", rr.Code)
	}
	got, _ = queries.GetThreadByID(ctx, thread.ID)
	if !got.TtlSeconds.Valid || got.TtlSeconds.Int64 != 24*3600 {
		t.Errorf("non-author change altered the thread: %+v", got.TtlSeconds)
	}

	// 7. Expired thread: not editable (404 via the expiry filter).
	expThread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: author.ID, Title: "expired thread",
		ExpiresAt: sql.NullTime{Time: time.Now().UTC().Add(-time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/threads/%d/expiration", expThread.ID), strings.NewReader(url.Values{"expiration": {"never"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(authorCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expired thread change: expected 404, got %d", rr.Code)
	}

	// 8. Unauthenticated: redirected to login.
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/threads/%d/expiration", thread.ID), strings.NewReader(url.Values{"expiration": {"7d"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Errorf("unauthenticated change: %d %q", rr.Code, rr.Header().Get("Location"))
	}

	// 9. UI: the author's thread page carries both toggle buttons and both
	//    forms starting is-hidden; the select preselects the stored choice
	//    (24h after step 5). A non-author's page carries neither.
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/threads/%d", thread.ID), nil)
	req.AddCookie(authorCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("author thread page: %d", rr.Code)
	}
	page := rr.Body.String()
	for _, want := range []string{
		`id="edit-title-btn" data-toggle-form="#title-edit-form"`,
		`id="title-edit-form" class="field has-addons mb-1 is-hidden"`,
		`id="change-expiration-btn" data-toggle-form="#expiration-edit-form"`,
		`id="expiration-edit-form" class="field has-addons mb-1 is-hidden"`,
		fmt.Sprintf(`action="/threads/%d/expiration"`, thread.ID),
		`name="expiration"`,
		`<option value="24h" selected>24 hours</option>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("author thread page missing %q", want)
		}
	}
	for _, absent := range []string{
		`<option value="7d" selected`,
		`<option value="never" selected`,
	} {
		if strings.Contains(page, absent) {
			t.Errorf("author thread page preselects %q alongside 24h", absent)
		}
	}

	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/threads/%d", thread.ID), nil)
	req.AddCookie(otherCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("non-author thread page: %d", rr.Code)
	}
	page = rr.Body.String()
	for _, absent := range []string{
		`id="change-expiration-btn"`,
		`id="expiration-edit-form"`,
		fmt.Sprintf(`action="/threads/%d/expiration"`, thread.ID),
	} {
		if strings.Contains(page, absent) {
			t.Errorf("non-author thread page shows %q", absent)
		}
	}
}

// TestFrontPageSearchCoversTitlesAndBodies pins the front-page search: the
// term matches thread titles and post bodies, is case-insensitive, survives
// in the input, and has a dedicated no-results state.
func TestFrontPageSearchCoversTitlesAndBodies(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()

	u, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "searcher",
		PasswordHash: "fakehash",
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	cookie := authCookieFor(t, queries, u.ID)

	newThread := func(title, body string) {
		t.Helper()
		th, err := queries.CreateThread(ctx, database.CreateThreadParams{
			UserID: u.ID,
			Title:  title,
		})
		if err != nil {
			t.Fatalf("failed to create thread %q: %v", title, err)
		}
		if _, err := queries.CreatePost(ctx, database.CreatePostParams{
			ThreadID: th.ID,
			UserID:   u.ID,
			Body:     body,
		}); err != nil {
			t.Fatalf("failed to create opening post for %q: %v", title, err)
		}
	}

	newThread("Go concurrency patterns", "channels and goroutines are fun")
	newThread("Cooking thread", "pasta recipes and sauce")
	newThread("Goroutine leaks", "memory profiles")

	search := func(q string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/?q="+url.QueryEscape(q), nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		app.Routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("search %q: expected 200, got %d", q, rr.Code)
		}
		return rr.Body.String()
	}

	// "goroutine" appears in one title and one body.
	page := search("goroutine")
	for _, want := range []string{"Go concurrency patterns", "Goroutine leaks"} {
		if !strings.Contains(page, want) {
			t.Errorf("search goroutine: missing %q", want)
		}
	}
	if strings.Contains(page, "Cooking thread") {
		t.Errorf("search goroutine: unrelated thread leaked into results")
	}
	if !strings.Contains(page, `value="goroutine"`) {
		t.Errorf("search goroutine: query not preserved in the input")
	}
	if !strings.Contains(page, "Results for") {
		t.Errorf("search goroutine: missing results heading")
	}

	// Body-only match, case-insensitively.
	page = search("PASTA")
	if !strings.Contains(page, "Cooking thread") {
		t.Errorf("search PASTA: expected body match, got none")
	}
	if strings.Contains(page, "Goroutine leaks") {
		t.Errorf("search PASTA: unrelated thread leaked into results")
	}

	// No matches: dedicated empty state + escape hatch.
	page = search("zzzz-no-such-thing")
	if !strings.Contains(page, "No threads match") {
		t.Errorf("no-results search: missing empty state")
	}
	if !strings.Contains(page, "Show all threads") {
		t.Errorf("no-results search: missing clear-search link")
	}
}

// TestUnreadThreadIndicators pins the per-user read markers: another user's
// post shows a dot, opening the thread clears it, own posts never count, and
// guests get no dots.
func TestUnreadThreadIndicators(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()

	createUser := func(name string) *database.User {
		t.Helper()
		u, err := queries.CreateUser(ctx, database.CreateUserParams{
			Username:     name,
			PasswordHash: "fakehash",
		})
		if err != nil {
			t.Fatalf("failed to create user %q: %v", name, err)
		}
		return &u
	}
	alice := createUser("alice")
	bob := createUser("bob")

	// Backdate both accounts so the opening post (created now) is newer than
	// the fallback "account created" timestamp on the first visit.
	if _, err := db.ExecContext(ctx,
		"UPDATE users SET created_at = datetime('now', '-1 day') WHERE id IN (?, ?)",
		alice.ID, bob.ID); err != nil {
		t.Fatalf("failed to backdate users: %v", err)
	}

	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: alice.ID,
		Title:  "unread test thread",
	})
	if err != nil {
		t.Fatalf("failed to create thread: %v", err)
	}
	if _, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID,
		UserID:   alice.ID,
		Body:     "opening post",
	}); err != nil {
		t.Fatalf("failed to create opening post: %v", err)
	}

	bobCookie := authCookieFor(t, queries, bob.ID)
	aliceCookie := authCookieFor(t, queries, alice.ID)

	unreadDots := func(cookie *http.Cookie) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rr := httptest.NewRecorder()
		app.Routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("front page: expected 200, got %d", rr.Code)
		}
		return strings.Count(rr.Body.String(), `class="unread-dot"`)
	}

	// Bob has never opened the thread → unread.
	if got := unreadDots(bobCookie); got != 1 {
		t.Fatalf("bob before opening: want 1 unread dot, got %d", got)
	}

	// Alice's own thread is never unread for her.
	if got := unreadDots(aliceCookie); got != 0 {
		t.Errorf("alice (own thread): want 0 unread dots, got %d", got)
	}

	// Opening the thread records the marker and clears the dot.
	req := httptest.NewRequest(http.MethodGet, "/threads/"+strconv.FormatInt(thread.ID, 10), nil)
	req.AddCookie(bobCookie)
	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("bob opening thread: expected 200, got %d", rr.Code)
	}
	if got := unreadDots(bobCookie); got != 0 {
		t.Fatalf("bob after reading: want 0 unread dots, got %d", got)
	}

	// A new reply from Alice becomes unread again. created_at is bumped past
	// the read marker explicitly so the test does not depend on second-
	// resolution timestamps.
	reply, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID,
		UserID:   alice.ID,
		Body:     "new reply",
	})
	if err != nil {
		t.Fatalf("failed to create reply: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE posts SET created_at = datetime('now', '+1 second') WHERE id = ?", reply.ID); err != nil {
		t.Fatalf("failed to bump reply timestamp: %v", err)
	}
	if got := unreadDots(bobCookie); got != 1 {
		t.Fatalf("bob after new reply: want 1 unread dot, got %d", got)
	}
}

// TestThemeCookie pins the theme toggle's server half: only light/dark
// cookies set data-theme on <html>; anything else follows the OS.
func TestThemeCookie(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()

	u, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "themed",
		PasswordHash: "fakehash",
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	session := authCookieFor(t, queries, u.ID)

	page := func(cookieValue string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(session)
		if cookieValue != "" {
			req.AddCookie(&http.Cookie{Name: "disjoint_theme", Value: cookieValue, Path: "/"})
		}
		rr := httptest.NewRecorder()
		app.Routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("front page: expected 200, got %d", rr.Code)
		}
		return rr.Body.String()
	}

	if body := page(""); !strings.Contains(body, `<html lang="en">`) {
		t.Errorf("no cookie: want no data-theme attribute")
	}
	if body := page("light"); !strings.Contains(body, `<html lang="en" data-theme="light">`) {
		t.Errorf("light cookie: want data-theme=light")
	}
	if body := page("dark"); !strings.Contains(body, `<html lang="en" data-theme="dark">`) {
		t.Errorf("dark cookie: want data-theme=dark")
	}
	if body := page("banana"); !strings.Contains(body, `<html lang="en">`) {
		t.Errorf("invalid cookie: want no data-theme attribute")
	}
	if body := page(""); !strings.Contains(body, `id="theme-toggle"`) {
		t.Errorf("want the theme toggle in the navbar")
	}
}

// --- Mentions & replies inbox ---------------------------------------------

// inboxTestEnv bundles the pieces the inbox tests share: a running router, the
// raw DB handle for notification assertions, and multipart form helpers that
// exercise the real write paths.
type inboxTestEnv struct {
	t       *testing.T
	app     *handlers.App
	db      *sql.DB
	queries *database.Queries
	router  http.Handler
	ctx     context.Context
}

func newInboxTestEnv(t *testing.T) *inboxTestEnv {
	t.Helper()
	app, db, queries, _ := setupTestApp(t)
	t.Cleanup(func() { db.Close() })
	return &inboxTestEnv{
		t:       t,
		app:     app,
		db:      db,
		queries: queries,
		router:  app.Routes(),
		ctx:     context.Background(),
	}
}

func (e *inboxTestEnv) user(name string) *database.User {
	e.t.Helper()
	u, err := e.queries.CreateUser(e.ctx, database.CreateUserParams{
		Username:     name,
		PasswordHash: "fakehash",
	})
	if err != nil {
		e.t.Fatalf("failed to create user %q: %v", name, err)
	}
	return &u
}

// form posts a multipart form as the given user and returns the recorder.
func (e *inboxTestEnv) form(path string, u *database.User, fields map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			e.t.Fatalf("failed to write field %q: %v", k, err)
		}
	}
	if err := w.Close(); err != nil {
		e.t.Fatalf("failed to close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(authCookieFor(e.t, e.queries, u.ID))
	rr := httptest.NewRecorder()
	e.router.ServeHTTP(rr, req)
	return rr
}

func (e *inboxTestEnv) newThread(u *database.User, title, body string) int64 {
	e.t.Helper()
	rr := e.form("/threads/new", u, map[string]string{
		"title":      title,
		"body":       body,
		"expiration": "never",
	})
	if rr.Code != http.StatusSeeOther {
		e.t.Fatalf("create thread: expected 303, got %d: %s", rr.Code, rr.Body.String())
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(rr.Header().Get("Location"), "/threads/"), 10, 64)
	if err != nil {
		e.t.Fatalf("create thread: bad redirect %q: %v", rr.Header().Get("Location"), err)
	}
	return id
}

func (e *inboxTestEnv) reply(threadID int64, u *database.User, body string) int64 {
	e.t.Helper()
	rr := e.form(fmt.Sprintf("/threads/%d/posts", threadID), u, map[string]string{"body": body})
	if rr.Code != http.StatusSeeOther {
		e.t.Fatalf("reply: expected 303, got %d: %s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	idx := strings.LastIndex(loc, "#post-")
	if idx < 0 {
		e.t.Fatalf("reply: redirect %q carries no post anchor", loc)
	}
	id, err := strconv.ParseInt(loc[idx+len("#post-"):], 10, 64)
	if err != nil {
		e.t.Fatalf("reply: bad post anchor in %q: %v", loc, err)
	}
	return id
}

type inboxNotif struct {
	Reason string
	PostID int64
	Unread bool
}

func (e *inboxTestEnv) notif(userID, threadID int64) (inboxNotif, bool) {
	e.t.Helper()
	var (
		n      inboxNotif
		readAt sql.NullTime
	)
	err := e.db.QueryRowContext(e.ctx,
		`SELECT reason, post_id, read_at FROM notifications WHERE user_id = ? AND thread_id = ?`,
		userID, threadID).Scan(&n.Reason, &n.PostID, &readAt)
	if err == sql.ErrNoRows {
		return inboxNotif{}, false
	}
	if err != nil {
		e.t.Fatalf("failed to read notification: %v", err)
	}
	n.Unread = !readAt.Valid
	return n, true
}

func (e *inboxTestEnv) notifCount(userID int64) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(e.ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id = ?`, userID).Scan(&n); err != nil {
		e.t.Fatalf("failed to count notifications: %v", err)
	}
	return n
}

func (e *inboxTestEnv) get(path string, u *database.User) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if u != nil {
		req.AddCookie(authCookieFor(e.t, e.queries, u.ID))
	}
	rr := httptest.NewRecorder()
	e.router.ServeHTTP(rr, req)
	return rr
}

func TestInboxRecipientsAndReasonUpgrade(t *testing.T) {
	e := newInboxTestEnv(t)
	alice := e.user("alice")
	bob := e.user("bob")
	carol := e.user("carol")
	dave := e.user("dave")

	// Opening post: the author is the only participant and is the actor, so
	// nobody is notified.
	threadID := e.newThread(alice, "Thread one", "opening post, no mentions")
	if n := e.notifCount(alice.ID); n != 0 {
		t.Fatalf("opening post should notify nobody, alice has %d rows", n)
	}

	// A reply mentioning carol notifies carol (mention) and the thread author
	// alice (reply), but never the actor bob.
	p1 := e.reply(threadID, bob, "hi @carol, look at this")
	if n, ok := e.notif(carol.ID, threadID); !ok || n.Reason != "mention" || n.PostID != p1 || !n.Unread {
		t.Fatalf("carol notification = %+v (ok=%v), want mention on post %d unread", n, ok, p1)
	}
	if n, ok := e.notif(alice.ID, threadID); !ok || n.Reason != "reply" || n.PostID != p1 {
		t.Fatalf("alice notification = %+v (ok=%v), want reply on post %d", n, ok, p1)
	}
	if _, ok := e.notif(bob.ID, threadID); ok {
		t.Fatalf("actor must not receive a notification")
	}
	if _, ok := e.notif(dave.ID, threadID); ok {
		t.Fatalf("non-participant must not receive a reply notification")
	}

	// A second plain reply bumps the author's row (one row per thread) and
	// does not touch carol, who has not participated in the thread.
	p2 := e.reply(threadID, bob, "second message")
	if n, _ := e.notif(alice.ID, threadID); n.PostID != p2 {
		t.Fatalf("author row should bump to post %d, got %+v", p2, n)
	}
	if n, _ := e.notif(carol.ID, threadID); n.PostID != p1 {
		t.Fatalf("carol's row should still point at post %d, got %+v", p1, n)
	}
	if got := e.notifCount(alice.ID); got != 1 {
		t.Fatalf("alice should have exactly one row for the thread, got %d", got)
	}

	// Carol's first notification in this thread was a mention, and reason is
	// sticky: later plain replies bump the row but never downgrade it.
	e.reply(threadID, carol, "joining in")
	p4 := e.reply(threadID, bob, "another plain reply")
	if n, _ := e.notif(carol.ID, threadID); n.Reason != "mention" || n.PostID != p4 {
		t.Fatalf("carol should keep mention while bumping to %d, got %+v", p4, n)
	}

	// Dave joins as a participant first, so his first row is a reply; a later
	// mention upgrades that same row to mention.
	e.reply(threadID, dave, "dave joins")
	p6 := e.reply(threadID, bob, "plain reply for dave")
	if n, _ := e.notif(dave.ID, threadID); n.Reason != "reply" || n.PostID != p6 {
		t.Fatalf("dave should be notified as reply on %d, got %+v", p6, n)
	}
	p7 := e.reply(threadID, bob, "@dave upgraded")
	if n, _ := e.notif(dave.ID, threadID); n.Reason != "mention" || n.PostID != p7 {
		t.Fatalf("dave's reason should upgrade to mention on %d, got %+v", p7, n)
	}
	if n, _ := e.notif(dave.ID, threadID); !n.Unread {
		t.Fatalf("a new trigger must make the row unread again")
	}

	// A read row is re-raised (read_at cleared) by the next trigger.
	if err := e.queries.MarkAllNotificationsRead(e.ctx, dave.ID); err != nil {
		t.Fatalf("failed to mark read: %v", err)
	}
	e.reply(threadID, bob, "@dave again")
	if n, _ := e.notif(dave.ID, threadID); !n.Unread {
		t.Fatalf("re-triggered row should be unread again, got %+v", n)
	}
}

func TestInboxMentionExtraction(t *testing.T) {
	e := newInboxTestEnv(t)
	alice := e.user("alice")
	bob := e.user("bob")

	// Self-mention in an opening post notifies nobody.
	e.newThread(alice, "Self mention", "hello @alice")
	if n := e.notifCount(alice.ID); n != 0 {
		t.Fatalf("self-mention must not notify, got %d rows", n)
	}

	// Unknown users, e-mail addresses, and code spans never notify.
	threadID := e.newThread(alice, "Extraction", "no mentions")
	e.reply(threadID, bob, "@nobodyhere hello")
	e.reply(threadID, bob, "mail user@gmail.com")
	e.reply(threadID, bob, "code `@alice` only")
	if n := e.notifCount(alice.ID); n != 1 {
		t.Fatalf("alice should only have the reply notification, got %d", n)
	}
	if n, _ := e.notif(alice.ID, threadID); n.Reason != "reply" {
		t.Fatalf("non-mention triggers should stay reply, got %+v", n)
	}
}

func TestInboxEditsDoNotNotify(t *testing.T) {
	e := newInboxTestEnv(t)
	alice := e.user("alice")
	bob := e.user("bob")

	threadID := e.newThread(alice, "Edits", "opening")
	postID := e.reply(threadID, bob, "no mention yet")
	if n := e.notifCount(alice.ID); n != 1 {
		t.Fatalf("expected one reply notification, got %d", n)
	}

	// Edit bob's post to mention alice: no new notification, no row change.
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/posts/%d/edit", postID),
		strings.NewReader("body=now+with+%40alice&url="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(authCookieFor(t, e.queries, bob.ID))
	rr := httptest.NewRecorder()
	e.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("edit: expected 303, got %d: %s", rr.Code, rr.Body.String())
	}

	if n := e.notifCount(alice.ID); n != 1 {
		t.Fatalf("edit must not create notifications, alice has %d rows", n)
	}
	if n, _ := e.notif(alice.ID, threadID); n.PostID != postID || n.Reason != "reply" {
		t.Fatalf("edit must not touch the existing row, got %+v", n)
	}
}

func TestInboxPageBadgeAndMarkAllRead(t *testing.T) {
	e := newInboxTestEnv(t)
	alice := e.user("alice")
	bob := e.user("bob")

	threadID := e.newThread(alice, "Badge thread", "opening")
	postID := e.reply(threadID, bob, "@alice ping")

	// Guests are redirected (read and bulk-write alike).
	guestGet := httptest.NewRequest(http.MethodGet, "/inbox", nil)
	rr := httptest.NewRecorder()
	e.router.ServeHTTP(rr, guestGet)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("guest GET /inbox = %d %q, want 303 /login", rr.Code, rr.Header().Get("Location"))
	}
	guestPost := httptest.NewRequest(http.MethodPost, "/inbox/read", nil)
	rr = httptest.NewRecorder()
	e.router.ServeHTTP(rr, guestPost)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("guest POST /inbox/read = %d %q, want 303 /login", rr.Code, rr.Header().Get("Location"))
	}

	// First view: highlighted row, badge, and the bulk button — and the view
	// must NOT mark anything read (explicit-read decision).
	page := e.get("/inbox", alice)
	if page.Code != http.StatusOK {
		t.Fatalf("GET /inbox: expected 200, got %d", page.Code)
	}
	html := page.Body.String()
	for _, want := range []string{
		"Inbox",
		"Mark all read",
		`class="inbox-badge" aria-hidden="true">1<`,
		`class="board-row inbox-row is-unread"`,
		"mentioned you in",
		"/threads/" + strconv.FormatInt(threadID, 10) + "#post-" + strconv.FormatInt(postID, 10),
	} {
		if !strings.Contains(html, want) {
			t.Errorf("inbox page missing %q", want)
		}
	}
	if n, _ := e.notif(alice.ID, threadID); !n.Unread {
		t.Fatalf("viewing /inbox must not mark the entry read, got %+v", n)
	}

	// Reload: still unread (no implicit clearing).
	if again := e.get("/inbox", alice); !strings.Contains(again.Body.String(), "is-unread") {
		t.Fatalf("reload should still show the unread highlight")
	}

	// Mark all read: redirect + flash, then nothing is highlighted, the
	// button is gone, and the badge is gone.
	rr = e.form("/inbox/read", alice, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/inbox" {
		t.Fatalf("POST /inbox/read = %d %q, want 303 /inbox", rr.Code, rr.Header().Get("Location"))
	}
	if flash := rr.Result().Cookies(); len(flash) == 0 {
		t.Errorf("expected a flash cookie on the mark-all-read redirect")
	}
	if n, _ := e.notif(alice.ID, threadID); n.Unread {
		t.Fatalf("mark all read must clear read_at, got %+v", n)
	}
	cleared := e.get("/inbox", alice).Body.String()
	for _, unwanted := range []string{"is-unread", "Mark all read", "inbox-badge"} {
		if strings.Contains(cleared, unwanted) {
			t.Errorf("cleared inbox still contains %q", unwanted)
		}
	}

	// A fresh trigger re-raises the entry and the badge.
	e.reply(threadID, bob, "another ping")
	if raised := e.get("/inbox", alice).Body.String(); !strings.Contains(raised, "inbox-badge") {
		t.Errorf("new activity should restore the badge")
	}
}

func TestInboxReadThroughOnThreadVisit(t *testing.T) {
	e := newInboxTestEnv(t)
	alice := e.user("alice")
	bob := e.user("bob")

	t1 := e.newThread(alice, "First", "opening")
	t2 := e.newThread(alice, "Second", "opening")
	e.reply(t1, bob, "reply one")
	e.reply(t2, bob, "reply two")

	// Opening t1 counts as dealing with its inbox entry only.
	if page := e.get("/threads/"+strconv.FormatInt(t1, 10), alice); page.Code != http.StatusOK {
		t.Fatalf("GET thread: expected 200, got %d", page.Code)
	}
	if n, _ := e.notif(alice.ID, t1); n.Unread {
		t.Fatalf("opening the thread must clear its notification")
	}
	if n, _ := e.notif(alice.ID, t2); !n.Unread {
		t.Fatalf("other threads must stay unread, got %+v", n)
	}

	html := e.get("/inbox", alice).Body.String()
	if c := strings.Count(html, `class="board-row inbox-row is-unread"`); c != 1 {
		t.Fatalf("expected exactly 1 unread row, got %d", c)
	}
	if !strings.Contains(html, `class="inbox-badge" aria-hidden="true">1<`) {
		t.Errorf("badge should count only the remaining unread thread")
	}
}

func TestInboxPagination(t *testing.T) {
	e := newInboxTestEnv(t)
	alice := e.user("alice")
	bob := e.user("bob")

	// 31 notified threads, seeded directly (the write path is covered above).
	for i := 0; i < 31; i++ {
		th, err := e.queries.CreateThread(e.ctx, database.CreateThreadParams{
			UserID: bob.ID,
			Title:  fmt.Sprintf("thread %02d", i),
		})
		if err != nil {
			t.Fatalf("failed to create thread %d: %v", i, err)
		}
		p, err := e.queries.CreatePost(e.ctx, database.CreatePostParams{
			ThreadID: th.ID,
			UserID:   bob.ID,
			Body:     "hello",
		})
		if err != nil {
			t.Fatalf("failed to create post %d: %v", i, err)
		}
		if err := e.queries.UpsertNotification(e.ctx, database.UpsertNotificationParams{
			UserID:   alice.ID,
			ThreadID: th.ID,
			PostID:   p.ID,
			Reason:   "reply",
		}); err != nil {
			t.Fatalf("failed to seed notification %d: %v", i, err)
		}
	}

	first := e.get("/inbox", alice).Body.String()
	if c := strings.Count(first, `class="board-row inbox-row`); c != 30 {
		t.Fatalf("page 1 should hold 30 rows, got %d", c)
	}
	// Match the pagination labels, not the bare words: the shared layout's
	// lightbox controls also contain "Previous"/"Next" in their aria-labels.
	if !strings.Contains(first, "Next &rarr;") || strings.Contains(first, "&larr; Prev") {
		t.Errorf("page 1 should offer Next only")
	}

	second := e.get("/inbox?page=2", alice).Body.String()
	if c := strings.Count(second, `class="board-row inbox-row`); c != 1 {
		t.Fatalf("page 2 should hold 1 row, got %d", c)
	}
	if !strings.Contains(second, "&larr; Prev") || strings.Contains(second, "Next &rarr;") {
		t.Errorf("page 2 should offer Prev only")
	}

	if beyond := e.get("/inbox?page=99", alice).Body.String(); !strings.Contains(beyond, "Nothing here yet") {
		t.Errorf("page beyond the end should show the empty state")
	}
}

func TestInboxExpiryAndCascade(t *testing.T) {
	t.Run("expired threads are filtered and purged", func(t *testing.T) {
		e := newInboxTestEnv(t)
		alice := e.user("alice")
		bob := e.user("bob")

		rr := e.form("/threads/new", alice, map[string]string{
			"title":      "ephemeral",
			"body":       "opening",
			"expiration": "24h",
		})
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("create thread: expected 303, got %d", rr.Code)
		}
		threadID, err := strconv.ParseInt(strings.TrimPrefix(rr.Header().Get("Location"), "/threads/"), 10, 64)
		if err != nil {
			t.Fatalf("bad redirect: %v", err)
		}
		e.reply(threadID, bob, "reply before expiry")

		// Expire it without waiting for the clean-up worker.
		if _, err := e.db.ExecContext(e.ctx,
			`UPDATE threads SET expires_at = datetime('now', '-1 hour') WHERE id = ?`, threadID); err != nil {
			t.Fatalf("failed to expire thread: %v", err)
		}

		// Listing and badge both filter in SQL: no dead links, no phantom count.
		html := e.get("/inbox", alice).Body.String()
		if strings.Contains(html, "is-unread") || strings.Contains(html, "inbox-badge") {
			t.Errorf("expired thread must not appear in the inbox or the badge")
		}
		if !strings.Contains(html, "Nothing here yet") {
			t.Errorf("inbox should show its empty state")
		}

		// The worker's purge cascades the notification row away.
		w := worker.New(e.queries, t.TempDir(), time.Minute)
		if err := w.CleanupOnce(e.ctx); err != nil {
			t.Fatalf("cleanup failed: %v", err)
		}
		if n := e.notifCount(alice.ID); n != 0 {
			t.Fatalf("purge should cascade notifications, got %d rows", n)
		}
	})

	t.Run("deleting a user cascades their notifications", func(t *testing.T) {
		e := newInboxTestEnv(t)
		alice := e.user("alice")
		bob := e.user("bob")

		threadID := e.newThread(alice, "doomed author", "opening")
		e.reply(threadID, bob, "reply")
		if n := e.notifCount(alice.ID); n != 1 {
			t.Fatalf("expected 1 notification, got %d", n)
		}

		if err := e.queries.DeleteUser(e.ctx, bob.ID); err != nil {
			t.Fatalf("failed to delete user: %v", err)
		}
		if n := e.notifCount(alice.ID); n != 0 {
			t.Fatalf("deleting the actor should cascade the entry, got %d", n)
		}
	})
}

func TestInboxMentionCap(t *testing.T) {
	e := newInboxTestEnv(t)
	alice := e.user("alice")
	bob := e.user("bob")

	const total = 25
	names := make([]string, 0, total)
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("user%02d", i)
		e.user(name)
		names = append(names, "@"+name)
	}

	threadID := e.newThread(alice, "many mentions", "opening")
	e.reply(threadID, bob, strings.Join(names, " "))

	var stored int
	if err := e.db.QueryRowContext(e.ctx,
		`SELECT COUNT(*) FROM notifications WHERE thread_id = ? AND reason = 'mention'`,
		threadID).Scan(&stored); err != nil {
		t.Fatalf("failed to count mention rows: %v", err)
	}
	if stored != 20 {
		t.Fatalf("expected the mention cap to store 20 rows, got %d", stored)
	}
}

func TestInboxLinkOnlySnippetFallback(t *testing.T) {
	e := newInboxTestEnv(t)
	alice := e.user("alice")
	bob := e.user("bob")

	threadID := e.newThread(alice, "links", "opening")
	e.form(fmt.Sprintf("/threads/%d/posts", threadID), bob, map[string]string{
		"url": "https://example.com/a-link-only-reply",
	})

	html := e.get("/inbox", alice).Body.String()
	if !strings.Contains(html, "https://example.com/a-link-only-reply") {
		t.Errorf("link-only replies should preview their URL in the inbox")
	}
}

func TestInboxBadgeCapAndGuestBell(t *testing.T) {
	e := newInboxTestEnv(t)
	alice := e.user("alice")
	bob := e.user("bob")

	// Guests never see the bell.
	guest := e.get("/login", nil)
	if guest.Code != http.StatusOK {
		t.Fatalf("GET /login: expected 200, got %d", guest.Code)
	}
	if strings.Contains(guest.Body.String(), "inbox-link") {
		t.Errorf("guest pages must not render the inbox bell")
	}

	// The visual badge caps at 99+ while the accessible label keeps the real
	// number. 100 notified threads, seeded directly.
	for i := 0; i < 100; i++ {
		th, err := e.queries.CreateThread(e.ctx, database.CreateThreadParams{
			UserID: bob.ID,
			Title:  fmt.Sprintf("cap thread %03d", i),
		})
		if err != nil {
			t.Fatalf("failed to create thread %d: %v", i, err)
		}
		p, err := e.queries.CreatePost(e.ctx, database.CreatePostParams{
			ThreadID: th.ID,
			UserID:   bob.ID,
			Body:     "ping",
		})
		if err != nil {
			t.Fatalf("failed to create post %d: %v", i, err)
		}
		if err := e.queries.UpsertNotification(e.ctx, database.UpsertNotificationParams{
			UserID:   alice.ID,
			ThreadID: th.ID,
			PostID:   p.ID,
			Reason:   "reply",
		}); err != nil {
			t.Fatalf("failed to seed notification %d: %v", i, err)
		}
	}

	html := e.get("/inbox", alice).Body.String()
	if !strings.Contains(html, `class="inbox-badge" aria-hidden="true">99+<`) {
		t.Errorf("badge should cap at 99+")
	}
	if !strings.Contains(html, "aria-label=\"Inbox, 100 unread\"") {
		t.Errorf("accessible label should carry the real count")
	}
}

func TestInlineEmojiInPostsAndBios(t *testing.T) {
	e := newInboxTestEnv(t)
	alice := e.user("alice")
	bob := e.user("bob")

	// 1. Upload a custom emoji through the real endpoint, which also refreshes
	// the renderer's snapshot.
	buf, contentType := emojiMultipart(t, "shipit", "shipit.png", realPNG(t, 32, 32))
	uploadReq := httptest.NewRequest(http.MethodPost, "/emojis", buf)
	uploadReq.Header.Set("Content-Type", contentType)
	uploadReq.AddCookie(authCookieFor(t, e.queries, alice.ID))
	rr := httptest.NewRecorder()
	e.router.ServeHTTP(rr, uploadReq)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("emoji upload: expected 303, got %d: %s", rr.Code, rr.Body.String())
	}

	var emojiID int64
	var storagePath string
	if err := e.db.QueryRowContext(e.ctx,
		`SELECT id, storage_path FROM emojis WHERE name = 'shipit'`).Scan(&emojiID, &storagePath); err != nil {
		t.Fatalf("failed to load uploaded emoji: %v", err)
	}
	imgTag := `<img class="emoji-img emoji-inline" src="/uploads/` + storagePath + `" alt=":shipit:" loading="lazy">`

	// 2. Thread body: unicode built-in, custom upload, unknown name, code span.
	threadID := e.newThread(alice, "Emoji thread",
		"unicode :joy: custom :shipit: unknown :nope: code `:joy:`")
	threadPath := "/threads/" + strconv.FormatInt(threadID, 10)

	html := e.get(threadPath, alice).Body.String()
	for _, want := range []string{"🙂", imgTag, ":nope:", "<code>:joy:</code>"} {
		if !strings.Contains(html, want) {
			t.Errorf("thread page missing %q", want)
		}
	}
	if strings.Contains(html, `alt=":nope:"`) {
		t.Errorf("unknown token must not become an image")
	}

	// 3. HTMX reply partial uses the same renderer.
	var replyBuf bytes.Buffer
	rw := multipart.NewWriter(&replyBuf)
	if err := rw.WriteField("body", "reply with :shipit: and :joy:"); err != nil {
		t.Fatal(err)
	}
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}
	replyReq := httptest.NewRequest(http.MethodPost, threadPath+"/posts", &replyBuf)
	replyReq.Header.Set("Content-Type", rw.FormDataContentType())
	replyReq.Header.Set("HX-Request", "true")
	replyReq.AddCookie(authCookieFor(t, e.queries, bob.ID))
	rr = httptest.NewRecorder()
	e.router.ServeHTTP(rr, replyReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("HTMX reply: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	replyHTML := rr.Body.String()
	for _, want := range []string{"🙂", imgTag} {
		if !strings.Contains(replyHTML, want) {
			t.Errorf("HTMX partial missing %q", want)
		}
	}

	// 4. Bios share the pipeline.
	e.form("/users/me/profile", alice, map[string]string{"bio": "about :joy: and :shipit:"})
	card := e.get("/users/alice", alice).Body.String()
	for _, want := range []string{"🙂", imgTag} {
		if !strings.Contains(card, want) {
			t.Errorf("user card bio missing %q", want)
		}
	}

	// 5. Deleting the custom emoji downgrades every old use to literal text —
	// on the thread page and in the bio — proving the cache was invalidated,
	// not just that the parser skips unknown names.
	if rr := e.form(fmt.Sprintf("/emojis/%d/delete", emojiID), alice, nil); rr.Code != http.StatusSeeOther {
		t.Fatalf("emoji delete: expected 303, got %d: %s", rr.Code, rr.Body.String())
	}
	afterDelete := e.get(threadPath, alice).Body.String()
	if !strings.Contains(afterDelete, ":shipit:") {
		t.Errorf("deleted emoji token should render literally on the thread page")
	}
	if strings.Contains(afterDelete, storagePath) {
		t.Errorf("deleted emoji image must disappear from the thread page")
	}
	afterBio := e.get("/users/alice", alice).Body.String()
	if !strings.Contains(afterBio, ":shipit:") || strings.Contains(afterBio, storagePath) {
		t.Errorf("deleted emoji should fall back to literal text in the bio")
	}
}

// --- Liveness: inbox badge poll + thread new-replies pill -------------------

func TestInboxBellPolling(t *testing.T) {
	e := newInboxTestEnv(t)
	alice := e.user("alice")
	bob := e.user("bob")

	// Guests: HX requests get the redirect header, plain requests a 303.
	guestHX := httptest.NewRequest(http.MethodGet, "/inbox/count", nil)
	guestHX.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	e.router.ServeHTTP(rr, guestHX)
	if rr.Code != http.StatusUnauthorized || rr.Header().Get("HX-Redirect") != "/login" {
		t.Fatalf("guest HX poll = %d %q, want 401 with HX-Redirect", rr.Code, rr.Header().Get("HX-Redirect"))
	}
	if rr := e.get("/inbox/count", nil); rr.Code != http.StatusSeeOther {
		t.Fatalf("guest plain poll = %d, want 303", rr.Code)
	}

	threadID := e.newThread(alice, "Bell poll", "opening")
	e.reply(threadID, bob, "@alice ping")

	poll := func() string {
		e.t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/inbox/count", nil)
		req.Header.Set("HX-Request", "true")
		req.AddCookie(authCookieFor(t, e.queries, alice.ID))
		rr := httptest.NewRecorder()
		e.router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("inbox count poll: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("poll fragments must not be cacheable, got Cache-Control %q", cc)
		}
		return rr.Body.String()
	}

	fragment := poll()
	for _, want := range []string{
		`class="navbar-item inbox-link"`,
		`class="inbox-badge" aria-hidden="true">1<`,
		// The swapped element must keep polling, so the attributes live on it.
		`hx-get="/inbox/count" hx-trigger="every 60s [!document.hidden]"`,
	} {
		if !strings.Contains(fragment, want) {
			t.Errorf("bell fragment missing %q", want)
		}
	}
	if strings.Contains(fragment, "<html") {
		t.Errorf("the poll endpoint must return a fragment, not a page")
	}

	// The full page ships the same polling attributes on the bell.
	if page := e.get("/", alice).Body.String(); !strings.Contains(page, `hx-trigger="every 60s [!document.hidden]"`) {
		t.Errorf("navbar bell is missing its poll trigger")
	}

	if err := e.queries.MarkAllNotificationsRead(e.ctx, alice.ID); err != nil {
		t.Fatalf("failed to mark read: %v", err)
	}
	if cleared := poll(); strings.Contains(cleared, "inbox-badge") {
		t.Errorf("cleared bell must not carry a badge: %s", cleared)
	}
}

func TestThreadNewPostsPolling(t *testing.T) {
	e := newInboxTestEnv(t)
	alice := e.user("alice")
	bob := e.user("bob")

	threadID := e.newThread(alice, "Poll thread", "opening post")
	threadPath := "/threads/" + strconv.FormatInt(threadID, 10)

	// Derive ids from the DB rather than hardcoding them: the assertions are
	// about "newest rendered post", not about a particular sequence.
	postIDs := func() []int64 {
		e.t.Helper()
		posts, err := e.queries.ListPostsByThread(e.ctx, threadID)
		if err != nil {
			e.t.Fatalf("failed to list posts: %v", err)
		}
		ids := make([]int64, 0, len(posts))
		for _, p := range posts {
			ids = append(ids, p.ID)
		}
		return ids
	}
	openingID := postIDs()[0]

	page := e.get(threadPath, alice).Body.String()
	for _, want := range []string{
		`id="new-posts"`,
		fmt.Sprintf(`hx-get="/threads/%d/updates?after=%d"`, threadID, openingID),
		`every 20s [!document.hidden]`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("thread page missing %q", want)
		}
	}
	if strings.Contains(page, "new reply") {
		t.Errorf("a freshly loaded thread must not show a new-replies pill")
	}

	updates := func(viewer *database.User, after int64) string {
		e.t.Helper()
		req := httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/threads/%d/updates?after=%d", threadID, after), nil)
		req.Header.Set("HX-Request", "true")
		req.AddCookie(authCookieFor(t, e.queries, viewer.ID))
		rr := httptest.NewRecorder()
		e.router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("updates: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("poll fragments must not be cacheable, got Cache-Control %q", cc)
		}
		return rr.Body.String()
	}

	// Bob replies: Alice sees one new reply, anchored at it, and the container
	// keeps polling.
	bobPost := e.reply(threadID, bob, "first reply")
	frag := updates(alice, openingID)
	for _, want := range []string{
		"1 new reply",
		fmt.Sprintf(`href="/threads/%d?from=%d#post-%d"`, threadID, bobPost, bobPost),
		`hx-trigger="every 20s [!document.hidden]"`,
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("updates fragment missing %q", want)
		}
	}

	// The author's own post never counts for them: Bob's "seen" is up to date.
	if own := updates(bob, bobPost-1); strings.Contains(own, "new repl") {
		t.Errorf("a viewer's own post must not be offered as a new reply: %s", own)
	}

	// The count is live, not a snapshot: a second reply raises it.
	e.reply(threadID, bob, "second reply")
	if two := updates(alice, openingID); !strings.Contains(two, "2 new replies") {
		t.Errorf("expected 2 new replies, got %s", two)
	}

	// Reloading pins the poll past everything rendered, so the pill is gone.
	all := postIDs()
	newest := all[len(all)-1]
	reloaded := e.get(threadPath, alice).Body.String()
	if !strings.Contains(reloaded, fmt.Sprintf(`hx-get="/threads/%d/updates?after=%d"`, threadID, newest)) {
		t.Errorf("reloaded thread should poll from the newest post (after=%d)", newest)
	}
	if strings.Contains(reloaded, "new repl") {
		t.Errorf("reloaded thread must not show the pill")
	}

	// Expired and unknown threads stop polling quietly (200, no trigger).
	if _, err := e.db.ExecContext(e.ctx,
		`UPDATE threads SET expires_at = datetime('now','-1 hour') WHERE id = ?`, threadID); err != nil {
		t.Fatalf("failed to expire thread: %v", err)
	}
	if stopped := updates(alice, openingID); strings.Contains(stopped, "hx-trigger") || strings.Contains(stopped, "new repl") {
		t.Errorf("an expired thread must stop the poll: %s", stopped)
	}
	unknown := httptest.NewRequest(http.MethodGet, "/threads/9999/updates?after=0", nil)
	unknown.Header.Set("HX-Request", "true")
	unknown.AddCookie(authCookieFor(t, e.queries, alice.ID))
	rr := httptest.NewRecorder()
	e.router.ServeHTTP(rr, unknown)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "hx-trigger") {
		t.Errorf("an unknown thread must answer 200 with a non-polling container, got %d: %s", rr.Code, rr.Body.String())
	}
}

// --- Image thumbnails ------------------------------------------------------

// opaquePNG encodes a genuinely opaque image so the thumbnail pipeline picks
// JPEG (realPNG's zero-value pixels are fully transparent and select PNG).
func opaquePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 0xff
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// uploadThreadWithFiles creates a thread whose opening post carries the given
// files and returns its id.
func uploadThreadWithFiles(t *testing.T, router http.Handler, cookie *http.Cookie, files map[string][]byte) int64 {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("title", "Thumb thread"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("body", "seed"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("expiration", "never"); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		fw, err := w.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/threads/new", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("thread create: expected 303, got %d: %s", rr.Code, rr.Body.String())
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(rr.Header().Get("Location"), "/threads/"), 10, 64)
	if err != nil {
		t.Fatalf("bad redirect %q: %v", rr.Header().Get("Location"), err)
	}
	return id
}

func TestThumbnailServing(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	alice, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "alice", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, alice.ID)

	uploadThreadWithFiles(t, router, cookie, map[string][]byte{"photo.png": opaquePNG(t, 1000, 500)})

	var storagePath string
	if err := db.QueryRowContext(ctx,
		`SELECT storage_path FROM attachments ORDER BY id DESC LIMIT 1`).Scan(&storagePath); err != nil {
		t.Fatalf("failed to load attachment: %v", err)
	}
	base := strings.TrimSuffix(storagePath, filepath.Ext(storagePath))

	get := func(url string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	// First request generates and caches a 320-wide JPEG.
	rr := get("/thumbs/" + storagePath + "?w=320")
	if rr.Code != http.StatusOK {
		t.Fatalf("thumb: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/jpeg") {
		t.Errorf("Content-Type = %q, want image/jpeg for an opaque source", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", cc)
	}
	img, _, err := image.Decode(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("failed to decode thumbnail: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 320 || b.Dy() != 160 {
		t.Errorf("thumbnail size = %dx%d, want 320x160", b.Dx(), b.Dy())
	}
	cachePath := filepath.Join(uploadDir, "thumbs", base+".320.v1.jpg")
	if _, err := os.Stat(cachePath); err != nil {
		t.Errorf("expected cached thumbnail at %s: %v", cachePath, err)
	}

	// Second request is a cache hit serving identical bytes.
	first := rr.Body.Bytes()
	rr = get("/thumbs/" + storagePath + "?w=320")
	if rr.Code != http.StatusOK || !bytes.Equal(first, rr.Body.Bytes()) {
		t.Errorf("second request should be a cache hit with identical bytes")
	}

	// Default width is 640.
	rr = get("/thumbs/" + storagePath)
	img, _, err = image.Decode(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("failed to decode default-width thumbnail: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 640 {
		t.Errorf("default width = %d, want 640", b.Dx())
	}

	// Whitelist and path-shape errors.
	if rr := get("/thumbs/" + storagePath + "?w=999"); rr.Code != http.StatusNotFound {
		t.Errorf("unknown width = %d, want 404", rr.Code)
	}
	if rr := get("/thumbs/" + storagePath + "?w=abc"); rr.Code != http.StatusNotFound {
		t.Errorf("non-numeric width = %d, want 404", rr.Code)
	}
	if rr := get("/thumbs/..%2F..%2Fetc%2Fpasswd?w=320"); rr.Code == http.StatusOK {
		t.Errorf("traversal attempt must not succeed")
	}
	if rr := get("/thumbs/does-not-exist.png?w=320"); rr.Code != http.StatusNotFound {
		t.Errorf("missing file = %d, want 404", rr.Code)
	}

	// Non-image attachments have no thumbnail view.
	uploadThreadWithFiles(t, router, cookie, map[string][]byte{"doc.pdf": []byte("%PDF-1.4\nfake")})
	var pdfPath string
	if err := db.QueryRowContext(ctx,
		`SELECT storage_path FROM attachments WHERE mime_type = 'application/pdf' ORDER BY id DESC LIMIT 1`).Scan(&pdfPath); err != nil {
		t.Fatalf("failed to load pdf attachment: %v", err)
	}
	if rr := get("/thumbs/" + pdfPath + "?w=320"); rr.Code != http.StatusNotFound {
		t.Errorf("pdf thumb = %d, want 404", rr.Code)
	}

	// Guests are redirected by the global middleware.
	req := httptest.NewRequest(http.MethodGet, "/thumbs/"+storagePath+"?w=320", nil)
	guest := httptest.NewRecorder()
	router.ServeHTTP(guest, req)
	if guest.Code != http.StatusSeeOther {
		t.Errorf("guest thumb = %d, want 303", guest.Code)
	}
}

func TestThumbnailFallbacks(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	alice, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "alice", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, alice.ID)

	fakePNG := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}
	uploadThreadWithFiles(t, router, cookie, map[string][]byte{
		"small.png":  opaquePNG(t, 200, 100), // narrower than any width
		"broken.png": fakePNG,                // sniffs as PNG, cannot decode
		"anim.gif":   realGIF(t, 64, 64),     // never thumbnailed
	})

	get := func(name string) *httptest.ResponseRecorder {
		var path string
		if err := db.QueryRowContext(ctx,
			`SELECT storage_path FROM attachments WHERE filename = ? ORDER BY id DESC LIMIT 1`, name).Scan(&path); err != nil {
			t.Fatalf("failed to find attachment %q: %v", name, err)
		}
		req := httptest.NewRequest(http.MethodGet, "/thumbs/"+path+"?w=640", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	// Every fallback serves the original bytes with the original type and the
	// original (short) cache policy, not a broken image.
	checks := []struct {
		name string
		ct   string
	}{
		{"small.png", "image/png"},
		{"broken.png", "image/png"},
		{"anim.gif", "image/gif"},
	}
	for _, c := range checks {
		rr := get(c.name)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: expected 200 fallback, got %d", c.name, rr.Code)
			continue
		}
		if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, c.ct) {
			t.Errorf("%s: Content-Type = %q, want %s", c.name, ct, c.ct)
		}
		if cc := rr.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
			t.Errorf("%s: fallback responses must not claim immutability (%q)", c.name, cc)
		}
	}

	// No cache entries are written for fallbacks.
	matches, _ := filepath.Glob(filepath.Join(uploadDir, "thumbs", "*"))
	if len(matches) != 0 {
		t.Errorf("fallbacks must not create cache files, got %v", matches)
	}
}

func TestThumbnailTemplates(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	alice, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "alice", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, alice.ID)

	threadID := uploadThreadWithFiles(t, router, cookie, map[string][]byte{"photo.png": opaquePNG(t, 800, 400)})
	var storagePath string
	if err := db.QueryRowContext(ctx,
		`SELECT storage_path FROM attachments ORDER BY id DESC LIMIT 1`).Scan(&storagePath); err != nil {
		t.Fatalf("failed to load attachment: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/threads/"+strconv.FormatInt(threadID, 10), nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	html := rr.Body.String()

	wants := []string{
		`src="/thumbs/` + storagePath + `?w=640"`,
		`srcset="/thumbs/` + storagePath + `?w=320 320w, /thumbs/` + storagePath + `?w=640 640w, /thumbs/` + storagePath + `?w=1280 1280w"`,
		`sizes="(max-width: 768px) 100vw, 25vw"`,
		// The lightbox still opens the full-size original.
		`href="/uploads/` + storagePath + `" class="lightbox-link"`,
	}
	for _, want := range wants {
		if !strings.Contains(html, want) {
			t.Errorf("thread page missing %q", want)
		}
	}

	// The front-page strip uses the small variant (tiles are ~144px).
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), `src="/thumbs/`+storagePath+`?w=320"`) {
		t.Errorf("front-page strip should request the 320px variant")
	}
}

func TestWorkerRemovesThumbnails(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	alice, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "alice", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, alice.ID)

	// TTL thread so the worker's expiry predicate applies.
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("title", "expiring thumb")
	_ = w.WriteField("body", "seed")
	_ = w.WriteField("expiration", "24h")
	fw, _ := w.CreateFormFile("files", "photo.png")
	_, _ = fw.Write(opaquePNG(t, 900, 450))
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, "/threads/new", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create: got %d", rr.Code)
	}

	var storagePath string
	if err := db.QueryRowContext(ctx,
		`SELECT storage_path FROM attachments ORDER BY id DESC LIMIT 1`).Scan(&storagePath); err != nil {
		t.Fatalf("failed to load attachment: %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/thumbs/"+storagePath+"?w=320", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("thumb: got %d", rr.Code)
	}
	thumbsDir := filepath.Join(uploadDir, "thumbs")
	if matches, _ := filepath.Glob(filepath.Join(thumbsDir, "*")); len(matches) == 0 {
		t.Fatalf("expected a cached thumbnail before the purge")
	}

	if _, err := db.ExecContext(ctx,
		`UPDATE threads SET expires_at = datetime('now','-1 hour') WHERE id = (SELECT thread_id FROM posts WHERE id = (SELECT post_id FROM attachments WHERE storage_path = ?))`,
		storagePath); err != nil {
		t.Fatalf("failed to expire thread: %v", err)
	}
	worker.New(queries, uploadDir, time.Minute).CleanupOnce(ctx)

	if matches, _ := filepath.Glob(filepath.Join(thumbsDir, "*")); len(matches) != 0 {
		t.Errorf("purge left thumbnails behind: %v", matches)
	}
	if _, err := os.Stat(filepath.Join(uploadDir, storagePath)); !os.IsNotExist(err) {
		t.Errorf("purge should delete the original too")
	}
}

func TestLightboxNavigationMarkup(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	alice, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "alice", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, alice.ID)

	threadID := uploadThreadWithFiles(t, router, cookie, map[string][]byte{
		"one.png": opaquePNG(t, 800, 400),
		"two.png": opaquePNG(t, 400, 800),
	})

	req := httptest.NewRequest(http.MethodGet, "/threads/"+strconv.FormatInt(threadID, 10), nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("thread: expected 200, got %d", rr.Code)
	}
	html := rr.Body.String()

	// The dialog's controls exist and are labelled; the group attribute on the
	// posts container is what scopes navigation to one thread's images.
	wants := []string{
		`id="lightbox"`,
		`aria-label="Image viewer"`,
		`class="lightbox-nav lightbox-prev" aria-label="Previous image" hidden`,
		`class="lightbox-nav lightbox-next" aria-label="Next image" hidden`,
		`class="lightbox-img" alt="" draggable="false"`,
		`class="lightbox-counter" aria-live="polite"`,
		`id="posts-container" class="posts-container" data-thread-id="` + strconv.FormatInt(threadID, 10) + `" data-lightbox-group`,
	}
	for _, want := range wants {
		if !strings.Contains(html, want) {
			t.Errorf("thread page missing %q", want)
		}
	}

	// Every image is a lightbox link to the original (the JS collects these in
	// document order), while the card shows a thumbnail.
	if c := strings.Count(html, `class="lightbox-link"`); c != 2 {
		t.Errorf("expected 2 lightbox links, got %d", c)
	}
	if !strings.Contains(html, `href="/uploads/`) || !strings.Contains(html, `srcset="/thumbs/`) {
		t.Errorf("lightbox links must point at originals while the card uses thumbnails")
	}

	// The dialog lives in the shared layout, so it is present on other pages
	// too; navigation is simply inactive outside a group.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	front := rr.Body.String()
	if !strings.Contains(front, `id="lightbox"`) {
		t.Errorf("front page should still carry the shared dialog")
	}
	if strings.Contains(front, "data-lightbox-group") {
		t.Errorf("the front page is not a lightbox group; its strip links to the thread")
	}
}
