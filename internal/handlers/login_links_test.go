package handlers_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"disjoint/internal/auth"
	"disjoint/internal/database"
	"disjoint/internal/worker"
)

var loginLinkRE = regexp.MustCompile(`value="([^"]*/auth/magic\?token=[^"]+)"`)

func extractLoginLink(t *testing.T, body string) string {
	t.Helper()
	m := loginLinkRE.FindStringSubmatch(body)
	if len(m) != 2 {
		t.Fatalf("login link not found in response")
	}
	return m[1]
}

func linkToken(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse login link: %v", err)
	}
	token := u.Query().Get("token")
	if token == "" {
		t.Fatal("login link has no token")
	}
	return token
}

func sqlID(id int64) string {
	return fmt.Sprintf("%d", id)
}

func TestAdminLoginLinkFlow(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()
	router := app.Routes()

	admin, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username: "link_admin", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.SetUserAdmin(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	targetHash, err := auth.HashPassword("oldpassword")
	if err != nil {
		t.Fatal(err)
	}
	target, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username: "link_target", PasswordHash: targetHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	adminCookie := authCookieFor(t, queries, admin.ID)

	generate := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/admin/users/"+sqlID(target.ID)+"/login-link", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	// Non-admins cannot mint links.
	req := httptest.NewRequest(http.MethodPost, "/admin/users/"+sqlID(target.ID)+"/login-link", nil)
	req.AddCookie(authCookieFor(t, queries, target.ID))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("non-admin link generation = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM login_tokens WHERE user_id = ?`, target.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("non-admin generated a token")
	}

	// Admin generation returns the raw link once and stores only the hash.
	rr = generate(adminCookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("generate = %d: %s", rr.Code, rr.Body.String())
	}
	link := extractLoginLink(t, rr.Body.String())
	token := linkToken(t, link)
	var storedHash string
	if err := db.QueryRowContext(ctx, `SELECT token_hash FROM login_tokens WHERE user_id = ?`, target.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == token || storedHash != auth.TokenHash(token) {
		t.Fatal("login token was not stored as its SHA-256 hash")
	}

	// GET validates but does not consume.
	req = httptest.NewRequest(http.MethodGet, link, nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Sign in as @"+target.Username) {
		t.Fatalf("confirmation page = %d: %s", rr.Code, rr.Body.String())
	}
	row, err := queries.GetLoginToken(ctx, auth.TokenHash(token))
	if err != nil || row.UsedAt.Valid {
		t.Fatal("GET consumed the login token")
	}

	// POST consumes it, creates a session, and removes pending tokens.
	form := url.Values{"token": {token}}
	req = httptest.NewRequest(http.MethodPost, "/auth/magic", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("consume = %d to %s: %s", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	var targetCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			targetCookie = c
		}
	}
	if targetCookie == nil || !sessionIsValid(t, queries, targetCookie) {
		t.Fatal("magic login did not create a valid session")
	}
	if _, err := queries.GetLoginToken(ctx, auth.TokenHash(token)); err == nil {
		t.Fatal("consumed login token still exists")
	}

	// Replay is rejected.
	req = httptest.NewRequest(http.MethodPost, "/auth/magic", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "invalid or expired") {
		t.Fatalf("replayed token was accepted: %d %s", rr.Code, rr.Body.String())
	}

	// Generating a replacement invalidates the previous link.
	rr = generate(adminCookie)
	link1 := extractLoginLink(t, rr.Body.String())
	rr = generate(adminCookie)
	link2 := extractLoginLink(t, rr.Body.String())
	req = httptest.NewRequest(http.MethodGet, link1, nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "invalid or expired") {
		t.Fatal("old link survived a new generation")
	}

	// Password change removes pending login links.
	req = httptest.NewRequest(http.MethodGet, link2, nil)
	req.AddCookie(targetCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Sign in as @"+target.Username) {
		t.Fatalf("replacement link was not valid while signed in: %d %s", rr.Code, rr.Body.String())
	}
	passwordForm := url.Values{
		"current_password": {"oldpassword"},
		"new_password":     {"newpassword123"},
		"confirm_password": {"newpassword123"},
	}
	req = httptest.NewRequest(http.MethodPost, "/account/password", strings.NewReader(passwordForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(targetCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("password change = %d", rr.Code)
	}
	var pending int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM login_tokens WHERE user_id = ?`, target.ID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("password change left %d pending login links", pending)
	}
}

func TestMagicLinkExpiryAndInvalidToken(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()
	router := app.Routes()

	admin, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "expiry_admin", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.SetUserAdmin(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	target, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "expiry_target", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/users/"+sqlID(target.ID)+"/login-link", nil)
	req.AddCookie(authCookieFor(t, queries, admin.ID))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	link := extractLoginLink(t, rr.Body.String())
	token := linkToken(t, link)

	if _, err := db.ExecContext(ctx, `UPDATE login_tokens SET expires_at = ? WHERE user_id = ?`,
		time.Now().Add(-time.Minute).Unix(), target.ID); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, link, nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "invalid or expired") {
		t.Fatal("expired link was accepted on GET")
	}
	form := url.Values{"token": {token}}
	req = httptest.NewRequest(http.MethodPost, "/auth/magic", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "invalid or expired") {
		t.Fatal("expired link was accepted on POST")
	}

	req = httptest.NewRequest(http.MethodGet, "/auth/magic?token=not-a-real-token", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "invalid or expired") {
		t.Fatal("unknown token was accepted")
	}

	// The worker prunes expired token rows.
	if err := worker.New(queries, uploadDir, time.Minute).CleanupOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM login_tokens WHERE user_id = ?`, target.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("worker left %d expired login tokens", count)
	}
}

func TestLoginLinkUsesConfiguredBaseURL(t *testing.T) {
	t.Setenv("PUBLIC_BASE_URL", "https://forum.example")
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()

	admin, err := queries.CreateUser(context.Background(), database.CreateUserParams{Username: "base_admin", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.SetUserAdmin(context.Background(), admin.ID); err != nil {
		t.Fatal(err)
	}
	target, err := queries.CreateUser(context.Background(), database.CreateUserParams{Username: "base_target", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/users/"+sqlID(target.ID)+"/login-link", nil)
	req.AddCookie(authCookieFor(t, queries, admin.ID))
	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `value="https://forum.example/auth/magic?token=`) {
		t.Fatalf("configured base URL not used: %s", rr.Body.String())
	}
}
