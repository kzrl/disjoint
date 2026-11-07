package handlers_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"disjoint/internal/auth"
	"disjoint/internal/database"
	"disjoint/internal/worker"
)

var resetTokenRE = regexp.MustCompile(`token=([0-9a-f]{64})`)

func flashValue(rr *httptest.ResponseRecorder, name string) string {
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func resetRequest(router http.Handler, method, target string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func userURL(id int64, suffix string) string {
	return "/admin/users/" + strconv.FormatInt(id, 10) + suffix
}

func TestAdminResetLinkFlow(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	admin, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "reset_admin", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.SetUserAdmin(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	adminCookie := authCookieFor(t, queries, admin.ID)

	oldHash, _ := auth.HashPassword("original-pass")
	target, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "reset_target", PasswordHash: oldHash})
	if err != nil {
		t.Fatal(err)
	}
	// A live session that the reset must revoke.
	targetCookie := authCookieFor(t, queries, target.ID)

	// 1. Non-admin cannot mint links.
	rr := postForm(router, userURL(target.ID, "/reset-link"), "", targetCookie)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("non-admin reset-link = %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// 2. Admin mints a link; the raw token appears once in the flash, and only
	//    its hash is stored.
	rr = postForm(router, userURL(target.ID, "/reset-link"), "", adminCookie)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin/users" {
		t.Fatalf("reset-link = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	flash := flashValue(rr, "flash_success")
	m := resetTokenRE.FindStringSubmatch(flash)
	if m == nil {
		t.Fatalf("flash has no reset token: %q", flash)
	}
	firstToken := m[1]
	var storedHash string
	if err := queries.DB().QueryRowContext(ctx, `SELECT token_hash FROM password_reset_tokens WHERE user_id = ?`, target.ID).Scan(&storedHash); err != nil {
		t.Fatalf("token row missing: %v", err)
	}
	if storedHash == firstToken {
		t.Fatalf("raw token stored in the database")
	}
	if storedHash != auth.TokenHash(firstToken) {
		t.Fatalf("stored hash is not the token's SHA-256")
	}

	// 3. The GET form renders for a valid token and is public.
	rr = resetRequest(router, http.MethodGet, "/reset?token="+firstToken, url.Values{}, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "reset_target") ||
		!strings.Contains(rr.Body.String(), `name="token"`) {
		t.Fatalf("reset page = %d, body missing form/user", rr.Code)
	}

	// 4. Reissuing invalidates the first link.
	rr = postForm(router, userURL(target.ID, "/reset-link"), "", adminCookie)
	second := resetTokenRE.FindStringSubmatch(flashValue(rr, "flash_success"))
	if second == nil {
		t.Fatalf("second link missing")
	}
	rr = resetRequest(router, http.MethodGet, "/reset?token="+firstToken, url.Values{}, nil)
	if !strings.Contains(rr.Body.String(), "invalid, has expired, or has already been used") {
		t.Fatalf("old token still renders the form")
	}

	// 5. A short password is rejected without consuming the token.
	rr = resetRequest(router, http.MethodPost, "/reset", url.Values{
		"token": {second[1]}, "new_password": {"short"}, "confirm_password": {"short"},
	}, nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("short reset = %d", rr.Code)
	}
	rr = resetRequest(router, http.MethodGet, "/reset?token="+second[1], url.Values{}, nil)
	if !strings.Contains(rr.Body.String(), `name="new_password"`) {
		t.Fatalf("token was consumed by a rejected password")
	}

	// 6. The real reset stores the password, revokes sessions and burns the token.
	rr = resetRequest(router, http.MethodPost, "/reset", url.Values{
		"token": {second[1]}, "new_password": {"brand-new-pass"}, "confirm_password": {"brand-new-pass"},
	}, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("reset = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if !flashCookieSet(rr, "flash_success", "Password updated. Sign in with your new password.") {
		t.Fatalf("missing success flash")
	}
	updated, err := queries.GetUserByID(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !auth.CheckPassword(updated.PasswordHash, "brand-new-pass") {
		t.Fatalf("new password not stored")
	}
	if updated.PasswordSet != 1 {
		t.Fatalf("password_set = %d after reset", updated.PasswordSet)
	}
	// The old session cookie no longer authenticates.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(targetCookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Fatalf("pre-reset session still works: %d to %s", rec.Code, rec.Header().Get("Location"))
	}

	// 7. Replay is refused.
	rr = resetRequest(router, http.MethodPost, "/reset", url.Values{
		"token": {second[1]}, "new_password": {"another-pass"}, "confirm_password": {"another-pass"},
	}, nil)
	if rr.Header().Get("Location") != "/login" {
		t.Fatalf("replay = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if !flashCookieSet(rr, "flash_error", "That reset link is invalid or has expired. Ask an administrator for a new one.") {
		t.Fatalf("replay did not report an invalid link")
	}
	after, _ := queries.GetUserByID(ctx, target.ID)
	if auth.CheckPassword(after.PasswordHash, "another-pass") {
		t.Fatalf("replay changed the password")
	}

	// 8. Admins cannot mint a link for themselves.
	rr = postForm(router, userURL(admin.ID, "/reset-link"), "", adminCookie)
	if !flashCookieSet(rr, "flash_error", "Use the Change password form for your own account") {
		t.Fatalf("self reset-link was allowed")
	}
}

func TestResetLinkForPasskeyOnlyAccount(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	admin, _ := queries.CreateUser(ctx, database.CreateUserParams{Username: "pw_admin", PasswordHash: "h"})
	if err := queries.SetUserAdmin(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	adminCookie := authCookieFor(t, queries, admin.ID)

	user, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "pk_only", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.DB().ExecContext(ctx, `UPDATE users SET password_set = 0 WHERE id = ?`, user.ID); err != nil {
		t.Fatal(err)
	}

	rr := postForm(router, userURL(user.ID, "/reset-link"), "", adminCookie)
	m := resetTokenRE.FindStringSubmatch(flashValue(rr, "flash_success"))
	if m == nil {
		t.Fatalf("no link for passkey-only account")
	}
	resetRequest(router, http.MethodPost, "/reset", url.Values{
		"token": {m[1]}, "new_password": {"escape-hatch"}, "confirm_password": {"escape-hatch"},
	}, nil)

	updated, _ := queries.GetUserByID(ctx, user.ID)
	if updated.PasswordSet != 1 || !auth.CheckPassword(updated.PasswordHash, "escape-hatch") {
		t.Fatalf("passkey-only reset did not enable password login: %+v", updated)
	}
}

func TestResetTokenPruning(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	_ = app
	defer db.Close()
	ctx := context.Background()

	admin, _ := queries.CreateUser(ctx, database.CreateUserParams{Username: "prune_admin", PasswordHash: "h"})
	target, _ := queries.CreateUser(ctx, database.CreateUserParams{Username: "prune_target", PasswordHash: "h"})
	for i, expires := range []time.Time{
		time.Now().UTC().Add(-time.Hour), // expired
		time.Now().UTC().Add(time.Hour),  // live
	} {
		if err := queries.CreatePasswordResetToken(ctx, database.CreatePasswordResetTokenParams{
			TokenHash: auth.TokenHash("token-" + strconv.Itoa(i)),
			UserID:    target.ID,
			CreatedBy: sql.NullInt64{Int64: admin.ID, Valid: true},
			ExpiresAt: expires,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := worker.New(queries, t.TempDir(), time.Minute).CleanupOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := queries.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM password_reset_tokens WHERE user_id = ?`, target.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("reset tokens after prune = %d, want 1", count)
	}
}
