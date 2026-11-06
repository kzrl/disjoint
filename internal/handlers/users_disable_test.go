package handlers_test

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"disjoint/internal/auth"
	"disjoint/internal/database"
)

// newAdminAndMember creates one admin and one regular member with real
// bcrypt hashes so the login flow can be exercised end to end.
func newAdminAndMember(t *testing.T, queries *database.Queries) (admin, member database.User) {
	t.Helper()
	ctx := context.Background()

	hash, err := auth.HashPassword("correcthorse")
	if err != nil {
		t.Fatal(err)
	}

	admin, err = queries.CreateUser(ctx, database.CreateUserParams{Username: "disable_admin", PasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.SetUserAdmin(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}

	member, err = queries.CreateUser(ctx, database.CreateUserParams{Username: "disable_member", PasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	return admin, member
}

func postForm(router http.Handler, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func loginForm(username, password string) string {
	form := url.Values{}
	form.Set("username", username)
	form.Set("password", password)
	return form.Encode()
}

func TestDisableUserSuspendsAccount(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	admin, member := newAdminAndMember(t, queries)
	adminCookie := authCookieFor(t, queries, admin.ID)
	memberCookie := authCookieFor(t, queries, member.ID)

	// Capture audit() output (it logs rather than writing to the database).
	var buf bytes.Buffer
	prevWriter := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prevWriter)

	// 1. Disabling a member succeeds and marks the row.
	rr := postForm(router, fmt.Sprintf("/admin/users/%d/disable", member.ID), "", adminCookie)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin/users" {
		t.Fatalf("disable = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if !flashCookieSet(rr, "flash_success", "User disabled") {
		t.Fatalf("expected success flash when disabling")
	}
	disabled, err := queries.GetUserByID(ctx, member.ID)
	if err != nil || disabled.IsDisabled != 1 {
		t.Fatalf("expected is_disabled=1, got %v (err: %v)", disabled, err)
	}

	// 2. The member's live session is dead: guarded request redirects to login.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(memberCookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Fatalf("disabled session GET / = %d to %s", rec.Code, rec.Header().Get("Location"))
	}

	// 3. Login with the correct password is refused and starts no session.
	rr = postForm(router, "/login", loginForm("disable_member", "correcthorse"), nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("disabled login = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if !flashCookieSet(rr, "flash_error", "This account has been disabled. Contact an administrator.") {
		t.Fatalf("expected disabled-account flash on login")
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookieName && c.Value != "" {
			t.Fatalf("disabled login must not set a session cookie")
		}
	}

	// 4. The admin list marks the account as disabled.
	req = httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	req.AddCookie(adminCookie)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/users = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Disabled") || !strings.Contains(body, fmt.Sprintf("/admin/users/%d/enable", member.ID)) {
		t.Fatalf("admin list does not mark the disabled user or offer Enable")
	}

	// 5. Enable restores the account and login works again.
	rr = postForm(router, fmt.Sprintf("/admin/users/%d/enable", member.ID), "", adminCookie)
	if rr.Code != http.StatusSeeOther || !flashCookieSet(rr, "flash_success", "User enabled") {
		t.Fatalf("enable = %d", rr.Code)
	}
	enabled, err := queries.GetUserByID(ctx, member.ID)
	if err != nil || enabled.IsDisabled != 0 {
		t.Fatalf("expected is_disabled=0 after enable, got %v (err: %v)", enabled, err)
	}
	rr = postForm(router, "/login", loginForm("disable_member", "correcthorse"), nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("login after enable = %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// 6. The audit trail records both transitions.
	logged := buf.String()
	if !strings.Contains(logged, "action=disable_user") || !strings.Contains(logged, "action=enable_user") {
		t.Fatalf("audit log = %q, want disable_user and enable_user entries", logged)
	}
}

func TestDisableUserRejectsSelfAndAdmins(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	admin, member := newAdminAndMember(t, queries)
	secondAdmin, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "other_admin", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.SetUserAdmin(ctx, secondAdmin.ID); err != nil {
		t.Fatal(err)
	}
	adminCookie := authCookieFor(t, queries, admin.ID)

	// 1. Self-disable is refused.
	rr := postForm(router, fmt.Sprintf("/admin/users/%d/disable", admin.ID), "", adminCookie)
	if !flashCookieSet(rr, "flash_error", "You cannot disable your own account") {
		t.Fatalf("expected self-disable flash_error")
	}
	if self, err := queries.GetUserByID(ctx, admin.ID); err != nil || self.IsDisabled != 0 {
		t.Fatalf("admin must stay enabled, got %v (err: %v)", self, err)
	}

	// 2. Disabling another admin is refused by the guarded UPDATE.
	rr = postForm(router, fmt.Sprintf("/admin/users/%d/disable", secondAdmin.ID), "", adminCookie)
	if !flashCookieSet(rr, "flash_error", "Only non-admin users can be disabled. Demote them first") {
		t.Fatalf("expected admin-target flash_error")
	}
	if other, err := queries.GetUserByID(ctx, secondAdmin.ID); err != nil || other.IsDisabled != 0 {
		t.Fatalf("second admin must stay enabled, got %v (err: %v)", other, err)
	}

	// 3. A non-admin cannot disable anyone.
	memberCookie := authCookieFor(t, queries, member.ID)
	rr = postForm(router, fmt.Sprintf("/admin/users/%d/disable", admin.ID), "", memberCookie)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("non-admin disable = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if !flashCookieSet(rr, "flash_error", "Unauthorized") {
		t.Fatalf("expected Unauthorized flash for non-admin disable")
	}
	if target, err := queries.GetUserByID(ctx, admin.ID); err != nil || target.IsDisabled != 0 {
		t.Fatalf("target must be unchanged, got %v (err: %v)", target, err)
	}

	// 4. Promoting a disabled account is refused so role and status agree.
	if rows, err := queries.DisableUser(ctx, member.ID); err != nil || rows != 1 {
		t.Fatalf("seed disable failed: rows=%d err=%v", rows, err)
	}
	rr = postForm(router, fmt.Sprintf("/admin/users/%d/promote", member.ID), "", adminCookie)
	if !flashCookieSet(rr, "flash_error", "Enable the account before promoting it to admin") {
		t.Fatalf("expected promote-disabled flash_error")
	}
	if member, err := queries.GetUserByID(ctx, member.ID); err != nil || member.IsAdmin != 0 {
		t.Fatalf("disabled user must not be promoted, got %v (err: %v)", member, err)
	}
}

func TestDisabledSessionIsPurgedByMiddleware(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	_, member := newAdminAndMember(t, queries)
	// Session created first, then the account disabled behind its back (the
	// handler would normally also delete it; this proves the middleware guard).
	cookie := authCookieFor(t, queries, member.ID)
	if rows, err := queries.DisableUser(ctx, member.ID); err != nil || rows != 1 {
		t.Fatalf("disable failed: rows=%d err=%v", rows, err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("disabled session GET / = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/login?disabled=1" {
		t.Fatalf("Location = %q, want /login?disabled=1", loc)
	}
	cleared := false
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookieName && c.Value == "" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("expected the session cookie to be expired")
	}
	// The row is gone, so replaying the token cannot authenticate.
	if _, err := queries.GetSessionUserID(ctx, database.GetSessionUserIDParams{
		TokenHash: auth.TokenHash(cookie.Value),
		ExpiresAt: 1 << 40,
	}); err == nil {
		t.Fatalf("expected session row to be deleted")
	}

	// The login page explains the eviction.
	req = httptest.NewRequest(http.MethodGet, "/login?disabled=1", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "account has been disabled") {
		t.Fatalf("login?disabled=1 = %d, expected disabled notice", rr.Code)
	}
}
