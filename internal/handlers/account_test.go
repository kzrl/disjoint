package handlers_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"disjoint/internal/auth"
	"disjoint/internal/database"
)

func sessionIsValid(t *testing.T, queries *database.Queries, cookie *http.Cookie) bool {
	t.Helper()
	_, err := queries.GetSessionUserID(context.Background(), database.GetSessionUserIDParams{
		TokenHash: auth.TokenHash(cookie.Value),
		ExpiresAt: time.Now().Unix(),
		IdleCutoff: sql.NullInt64{
			Int64: time.Now().Add(-auth.SessionIdleTimeout).Unix(),
			Valid: true,
		},
	})
	return err == nil
}

func TestAccountPasswordChangeAndSignOutEverywhere(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()

	hash, err := auth.HashPassword("oldpassword")
	if err != nil {
		t.Fatal(err)
	}
	user, err := queries.CreateUser(context.Background(), database.CreateUserParams{
		Username:     "account_user",
		PasswordHash: hash,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)

	// The account page is reachable and exposes both actions.
	req := httptest.NewRequest(http.MethodGet, "/account", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /account = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Change password", "Sign out everywhere"} {
		if !strings.Contains(body, want) {
			t.Errorf("account page missing %q", want)
		}
	}

	postPassword := func(current, next, confirm string) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{}
		form.Set("current_password", current)
		form.Set("new_password", next)
		form.Set("confirm_password", confirm)
		req := httptest.NewRequest(http.MethodPost, "/account/password", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	// Wrong current password: rejected, session survives.
	rr = postPassword("wrong-password", "newpassword", "newpassword")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/account" {
		t.Fatalf("wrong current password = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if !sessionIsValid(t, queries, cookie) {
		t.Fatal("wrong current password must not revoke the session")
	}
	stored, err := queries.GetUserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !auth.CheckPassword(stored.PasswordHash, "oldpassword") {
		t.Fatal("wrong current password changed the hash")
	}

	// Mismatch and short-password checks.
	if rr = postPassword("oldpassword", "newpassword", "different"); rr.Header().Get("Location") != "/account" {
		t.Fatalf("mismatched confirmation was accepted")
	}
	if rr = postPassword("oldpassword", "short", "short"); rr.Header().Get("Location") != "/account" {
		t.Fatalf("short password was accepted")
	}

	// Successful change: new hash, all sessions revoked, cookie cleared.
	rr = postPassword("oldpassword", "newpassword123", "newpassword123")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("password change = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	stored, err = queries.GetUserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !auth.CheckPassword(stored.PasswordHash, "newpassword123") {
		t.Fatal("new password hash not stored")
	}
	if auth.CheckPassword(stored.PasswordHash, "oldpassword") {
		t.Fatal("old password still works")
	}
	if sessionIsValid(t, queries, cookie) {
		t.Fatal("password change must revoke every session")
	}

	// The old cookie cannot access authenticated pages.
	req = httptest.NewRequest(http.MethodGet, "/account", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("old session access = %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// Sign in with the new password, then sign out everywhere.
	form := url.Values{}
	form.Set("username", "account_user")
	form.Set("password", "newpassword123")
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("login with new password = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	var newCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			newCookie = c
			break
		}
	}
	if newCookie == nil {
		t.Fatal("login did not set a session cookie")
	}
	// A second session for the same user must also be revoked.
	otherCookie := authCookieFor(t, queries, user.ID)

	req = httptest.NewRequest(http.MethodPost, "/account/sign-out-everywhere", nil)
	req.AddCookie(newCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("sign out everywhere = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if sessionIsValid(t, queries, newCookie) || sessionIsValid(t, queries, otherCookie) {
		t.Fatal("sign out everywhere must revoke all sessions")
	}
}

func TestAccountRequiresLogin(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	if _, err := queries.CreateUser(context.Background(), database.CreateUserParams{
		Username:     "existing_account_user",
		PasswordHash: "hash",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/account", nil)
	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("guest /account = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
}

func TestPasswordChangeRateLimit(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()

	hash, err := auth.HashPassword("oldpassword")
	if err != nil {
		t.Fatal(err)
	}
	user, err := queries.CreateUser(context.Background(), database.CreateUserParams{
		Username:     "password_limit_user",
		PasswordHash: hash,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)

	attempt := func() *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{}
		form.Set("current_password", "wrong-password")
		form.Set("new_password", "newpassword123")
		form.Set("confirm_password", "newpassword123")
		req := httptest.NewRequest(http.MethodPost, "/account/password", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	for i := 0; i < 5; i++ {
		if rr := attempt(); rr.Code != http.StatusSeeOther {
			t.Fatalf("attempt %d = %d", i+1, rr.Code)
		}
	}
	rr := attempt()
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("limited attempt = %d", rr.Code)
	}
	var limited bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == "flash_error" && strings.Contains(c.Value, "Too many password attempts") {
			limited = true
		}
	}
	if !limited {
		t.Fatal("sixth attempt was not rate-limited")
	}
	stored, err := queries.GetUserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !auth.CheckPassword(stored.PasswordHash, "oldpassword") {
		t.Fatal("rate-limited attempt changed the password")
	}
}
