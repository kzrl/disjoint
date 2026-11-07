package handlers_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"

	"disjoint/internal/auth"
	"disjoint/internal/database"
	"disjoint/internal/passkeys"
)

// stubPasskeys implements handlers.PasskeyService without any WebAuthn crypto,
// so the HTTP contract (auth, password confirmation, cookies, session start,
// scoping) can be tested deterministically. The real ceremony is covered by
// internal/passkeys' virtual-authenticator tests.
type stubPasskeys struct {
	beginRegFn     func(ctx context.Context, account *database.User) (passkeys.Options, error)
	finishRegFn    func(ctx context.Context, account *database.User, token string, r *http.Request) (string, error)
	beginLogin     func(ctx context.Context) (passkeys.Options, error)
	finishLogin    func(ctx context.Context, token string, r *http.Request) (int64, string, error)
	beginSignupFn  func(ctx context.Context, code, username string) (passkeys.Options, error)
	finishSignupFn func(ctx context.Context, token string, r *http.Request) (database.User, error)
	beginReauthFn  func(ctx context.Context, account *database.User) (passkeys.Options, error)
	finishReauthFn func(ctx context.Context, account *database.User, token string, r *http.Request) error
	listFn         func(ctx context.Context, userID int64) ([]database.WebauthnCredential, error)
	deleteFn       func(ctx context.Context, userID int64, id string) (bool, error)
	renameFn       func(ctx context.Context, userID int64, id, name string) (bool, error)

	lastToken string
}

func (s *stubPasskeys) BeginRegistration(ctx context.Context, account *database.User) (passkeys.Options, error) {
	if s.beginRegFn != nil {
		return s.beginRegFn(ctx, account)
	}
	return passkeys.Options{PublicKey: map[string]string{"challenge": "abc"}, Token: "ceremony-token"}, nil
}

func (s *stubPasskeys) FinishRegistration(ctx context.Context, account *database.User, token string, r *http.Request) (string, error) {
	s.lastToken = token
	if s.finishRegFn != nil {
		return s.finishRegFn(ctx, account, token, r)
	}
	return "Passkey 1", nil
}

func (s *stubPasskeys) BeginLogin(ctx context.Context) (passkeys.Options, error) {
	if s.beginLogin != nil {
		return s.beginLogin(ctx)
	}
	return passkeys.Options{PublicKey: map[string]string{"challenge": "abc"}, Token: "login-token"}, nil
}

func (s *stubPasskeys) FinishLogin(ctx context.Context, token string, r *http.Request) (int64, string, error) {
	s.lastToken = token
	if s.finishLogin != nil {
		return s.finishLogin(ctx, token, r)
	}
	return 0, "", errors.New("no stub")
}

func (s *stubPasskeys) BeginSignup(ctx context.Context, code, username string) (passkeys.Options, error) {
	if s.beginSignupFn != nil {
		return s.beginSignupFn(ctx, code, username)
	}
	return passkeys.Options{PublicKey: map[string]string{"challenge": "abc"}, Token: "signup-token"}, nil
}

func (s *stubPasskeys) FinishSignup(ctx context.Context, token string, r *http.Request) (database.User, error) {
	s.lastToken = token
	if s.finishSignupFn != nil {
		return s.finishSignupFn(ctx, token, r)
	}
	return database.User{}, errors.New("no stub")
}

func (s *stubPasskeys) BeginReauth(ctx context.Context, account *database.User) (passkeys.Options, error) {
	if s.beginReauthFn != nil {
		return s.beginReauthFn(ctx, account)
	}
	return passkeys.Options{PublicKey: map[string]string{"challenge": "abc"}, Token: "reauth-token"}, nil
}

func (s *stubPasskeys) FinishReauth(ctx context.Context, account *database.User, token string, r *http.Request) error {
	s.lastToken = token
	if s.finishReauthFn != nil {
		return s.finishReauthFn(ctx, account, token, r)
	}
	return nil
}

func (s *stubPasskeys) ListCredentials(ctx context.Context, userID int64) ([]database.WebauthnCredential, error) {
	if s.listFn != nil {
		return s.listFn(ctx, userID)
	}
	return nil, nil
}

func (s *stubPasskeys) DeleteCredential(ctx context.Context, userID int64, id string) (bool, error) {
	if s.deleteFn != nil {
		return s.deleteFn(ctx, userID, id)
	}
	return false, nil
}

func (s *stubPasskeys) RenameCredential(ctx context.Context, userID int64, id, name string) (bool, error) {
	if s.renameFn != nil {
		return s.renameFn(ctx, userID, id, name)
	}
	return false, nil
}

func newPasskeyTestApp(t *testing.T, stub *stubPasskeys) (http.Handler, *database.Queries, database.User, *http.Cookie) {
	t.Helper()
	app, db, queries, _ := setupTestApp(t)
	t.Cleanup(func() { db.Close() })

	user, err := queries.CreateUser(context.Background(), database.CreateUserParams{
		Username:     "passkey_owner",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cookie := authCookieFor(t, queries, user.ID)
	app.SetPasskeyService(stub)
	return app.Routes(), queries, user, cookie
}

func jsonBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
	return out
}

func webAuthnCookie(rr *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == "disjoint_webauthn" {
			return c
		}
	}
	return nil
}

func TestPasskeyRegisterBeginRequiresPassword(t *testing.T) {
	stub := &stubPasskeys{}
	router, _, _, cookie := newPasskeyTestApp(t, stub)

	// 1. Unauthenticated requests are redirected to the login page by the auth
	// middleware before the handler ever runs.
	req := httptest.NewRequest(http.MethodPost, "/account/passkeys/begin", strings.NewReader(`{"password":"correct horse"}`))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || !strings.HasPrefix(rr.Header().Get("Location"), "/login") {
		t.Fatalf("anonymous begin = %d to %s, want a login redirect", rr.Code, rr.Header().Get("Location"))
	}

	// 2. Wrong password: no ceremony cookie is issued.
	req = httptest.NewRequest(http.MethodPost, "/account/passkeys/begin", strings.NewReader(`{"password":"wrong"}`))
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("wrong password begin = %d, want 403: %s", rr.Code, rr.Body.String())
	}
	if webAuthnCookie(rr) != nil {
		t.Fatalf("a ceremony cookie was issued for a wrong password")
	}
}

func TestPasskeyRegisterFlow(t *testing.T) {
	var seenToken string
	stub := &stubPasskeys{}
	stub.finishRegFn = func(_ context.Context, _ *database.User, token string, _ *http.Request) (string, error) {
		seenToken = token
		return "Passkey 1", nil
	}
	router, queries, user, cookie := newPasskeyTestApp(t, stub)

	hash, err := auth.HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateUserPassword(context.Background(), database.UpdateUserPasswordParams{
		PasswordHash: hash,
		ID:           user.ID,
	}); err != nil {
		t.Fatal(err)
	}

	// Begin with the correct password: options + ceremony cookie.
	req := httptest.NewRequest(http.MethodPost, "/account/passkeys/begin", strings.NewReader(`{"password":"correct horse"}`))
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("begin = %d: %s", rr.Code, rr.Body.String())
	}
	if got := jsonBody(t, rr); got["publicKey"] == nil || got["token"] != "ceremony-token" {
		t.Fatalf("begin payload = %v", got)
	}
	ceremony := webAuthnCookie(rr)
	if ceremony == nil || ceremony.Value != "ceremony-token" || !ceremony.HttpOnly {
		t.Fatalf("ceremony cookie = %+v", ceremony)
	}

	// Finish: the cookie's token reaches the service, and the cookie is cleared.
	req = httptest.NewRequest(http.MethodPost, "/account/passkeys/finish", strings.NewReader(`{"id":"x"}`))
	req.AddCookie(cookie)
	req.AddCookie(ceremony)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("finish = %d: %s", rr.Code, rr.Body.String())
	}
	if seenToken != "ceremony-token" {
		t.Fatalf("service received token %q", seenToken)
	}
	if c := webAuthnCookie(rr); c == nil || c.MaxAge >= 0 {
		t.Fatalf("ceremony cookie was not cleared: %+v", c)
	}
}

func TestPasskeyLoginStartsSession(t *testing.T) {
	stub := &stubPasskeys{}
	router, _, user, _ := newPasskeyTestApp(t, stub)
	stub.finishLogin = func(context.Context, string, *http.Request) (int64, string, error) {
		return user.ID, "cred-id", nil
	}

	// Begin.
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/login/passkey/begin", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("begin = %d: %s", rr.Code, rr.Body.String())
	}
	ceremony := webAuthnCookie(rr)
	if ceremony == nil {
		t.Fatalf("no ceremony cookie on login begin")
	}

	// Finish: a session is established and the response says where to go.
	req := httptest.NewRequest(http.MethodPost, "/login/passkey/finish", strings.NewReader(`{"id":"x"}`))
	req.AddCookie(ceremony)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("finish = %d: %s", rr.Code, rr.Body.String())
	}
	if body := jsonBody(t, rr); body["redirect"] != "/" {
		t.Fatalf("finish payload = %v", body)
	}
	var sessionCookie bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == "disjoint_session" && c.Value != "" {
			sessionCookie = true
		}
	}
	if !sessionCookie {
		t.Fatalf("passkey login did not set a session cookie")
	}
}

func TestPasskeyLoginRefusesDisabledAccount(t *testing.T) {
	stub := &stubPasskeys{}
	router, queries, user, _ := newPasskeyTestApp(t, stub)
	stub.finishLogin = func(context.Context, string, *http.Request) (int64, string, error) {
		return user.ID, "cred-id", nil
	}
	if _, err := queries.DisableUser(context.Background(), user.ID); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/login/passkey/finish", strings.NewReader(`{"id":"x"}`))
	req.AddCookie(&http.Cookie{Name: "disjoint_webauthn", Value: "t"})
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("disabled passkey login = %d, want 401", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "disjoint_session" && c.Value != "" {
			t.Fatalf("disabled account received a session cookie")
		}
	}
}

func TestPasskeyLoginGenericFailure(t *testing.T) {
	stub := &stubPasskeys{}
	router, _, _, _ := newPasskeyTestApp(t, stub)
	stub.finishLogin = func(context.Context, string, *http.Request) (int64, string, error) {
		return 0, "", passkeys.ErrCeremony
	}

	req := httptest.NewRequest(http.MethodPost, "/login/passkey/finish", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("failed login = %d, want 401", rr.Code)
	}
	if msg, _ := jsonBody(t, rr)["error"].(string); msg != "Passkey sign-in failed" {
		t.Fatalf("error = %q, want the generic message", msg)
	}
}

func TestPasskeyDeleteIsScoped(t *testing.T) {
	stub := &stubPasskeys{}
	router, _, user, cookie := newPasskeyTestApp(t, stub)

	stub.deleteFn = func(_ context.Context, userID int64, id string) (bool, error) {
		if userID != user.ID {
			t.Errorf("delete called with user %d, want %d", userID, user.ID)
		}
		return id == "mine", nil
	}

	// Someone else's (or a missing) id is a 404.
	req := httptest.NewRequest(http.MethodPost, "/account/passkeys/other/delete", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("foreign delete = %d, want 404", rr.Code)
	}

	// The user's own id deletes and redirects with a flash.
	req = httptest.NewRequest(http.MethodPost, "/account/passkeys/mine/delete", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/account" {
		t.Fatalf("delete = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if !flashCookieSet(rr, "flash_success", "Passkey deleted") {
		t.Fatalf("missing delete flash")
	}
}

func TestAccountPageListsPasskeys(t *testing.T) {
	stub := &stubPasskeys{}
	router, _, _, cookie := newPasskeyTestApp(t, stub)
	stub.listFn = func(context.Context, int64) ([]database.WebauthnCredential, error) {
		return []database.WebauthnCredential{{ID: "cred-1", Name: "Work laptop"}}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/account", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /account = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Passkeys", "Work laptop", `id="add-passkey-form"`, "/static/passkeys.js", "defer"} {
		if !strings.Contains(body, want) {
			t.Errorf("account page missing %q", want)
		}
	}
	if strings.Contains(body, "No passkeys yet") {
		t.Errorf("account page shows the empty state despite a credential")
	}
}

func TestPasskeyEndpointsDisabledWithoutConfig(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	user, err := queries.CreateUser(context.Background(), database.CreateUserParams{Username: "no_config", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)
	router := app.Routes()

	req := httptest.NewRequest(http.MethodPost, "/login/passkey/begin", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("login begin without config = %d, want 501", rr.Code)
	}

	// The account page simply omits the section.
	req = httptest.NewRequest(http.MethodGet, "/account", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if strings.Contains(rr.Body.String(), `id="passkeys-box"`) {
		t.Fatalf("account page rendered the passkeys box without a service")
	}
}

// Cross-origin posts to the new endpoint are rejected by the existing CSRF
// middleware — assert once so a future route move cannot silently bypass it.
func TestPasskeyBeginRejectsCrossOrigin(t *testing.T) {
	router, _, _, cookie := newPasskeyTestApp(t, &stubPasskeys{})
	req := httptest.NewRequest(http.MethodPost, "/account/passkeys/begin", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin begin = %d, want 403", rr.Code)
	}
}

func TestPasswordLoginRejectsPasskeyOnlyAccount(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	hash, err := auth.HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	user, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "passkey_only", PasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.DB().ExecContext(ctx, `UPDATE users SET password_set = 0 WHERE id = ?`, user.ID); err != nil {
		t.Fatal(err)
	}

	rr := postForm(router, "/login", loginForm("passkey_only", "correct horse"), nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("passkey-only password login = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if !flashCookieSet(rr, "flash_error", "This account signs in with a passkey. Use the passkey button.") {
		t.Fatalf("expected the passkey-only message, got %q", rr.Result().Cookies())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookieName && c.Value != "" {
			t.Fatalf("passkey-only account received a session cookie")
		}
	}
}

func TestPendingAccountCannotLogIn(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	owner, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "pending_owner", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := queries.CreatePendingUser(ctx, database.CreatePendingUserParams{
		Username:     "half_signed",
		PasswordHash: "h",
		PendingUntil: sql.NullTime{Time: time.Now().UTC().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = owner

	// Password login:
	rr := postForm(router, "/login", loginForm("half_signed", "anything"), nil)
	if !flashCookieSet(rr, "flash_error", "Invalid username or password.") {
		t.Fatalf("pending password login did not get the generic message")
	}

	// Passkey login: the service resolves a user, but the handler must refuse.
	app.SetPasskeyService(&stubPasskeys{finishLogin: func(context.Context, string, *http.Request) (int64, string, error) {
		return pending.ID, "cred", nil
	}})
	router = app.Routes()
	req := httptest.NewRequest(http.MethodPost, "/login/passkey/finish", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: "disjoint_webauthn", Value: "t"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("pending passkey login = %d, want 401", rec.Code)
	}
}

func TestPasskeySignupEndpoints(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()

	stub := &stubPasskeys{}
	var seenCode, seenUsername string
	stub.beginSignupFn = func(_ context.Context, code, username string) (passkeys.Options, error) {
		seenCode, seenUsername = code, username
		return passkeys.Options{PublicKey: map[string]string{"challenge": "abc"}, Token: "signup-token"}, nil
	}
	newUser, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "signup_finished", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	stub.finishSignupFn = func(context.Context, string, *http.Request) (database.User, error) {
		return newUser, nil
	}
	app.SetPasskeyService(stub)
	router := app.Routes()

	// Public: no session needed.
	rr := postJSONRecorder(router, "/register/passkey/begin", `{"code":"invite-1","username":"freshuser"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("signup begin = %d: %s", rr.Code, rr.Body.String())
	}
	if seenCode != "invite-1" || seenUsername != "freshuser" {
		t.Fatalf("service received code=%q username=%q", seenCode, seenUsername)
	}
	ceremony := webAuthnCookie(rr)
	if ceremony == nil || ceremony.Value != "signup-token" {
		t.Fatalf("signup ceremony cookie = %+v", ceremony)
	}

	// Invalid username never reaches the service.
	rr = postJSONRecorder(router, "/register/passkey/begin", `{"code":"invite-1","username":"!!"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid username = %d, want 400", rr.Code)
	}

	// Finish starts a session and sends the new user to /account.
	req := httptest.NewRequest(http.MethodPost, "/register/passkey/finish", strings.NewReader(`{"id":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ceremony)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("signup finish = %d: %s", rec.Code, rec.Body.String())
	}
	if body := jsonBody(t, rec); body["redirect"] != "/account" {
		t.Fatalf("signup finish payload = %v", body)
	}
	var session bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName && c.Value != "" {
			session = true
		}
	}
	if !session {
		t.Fatalf("signup did not start a session")
	}

	// Invite errors map to a 400 with the message the UI shows.
	stub.beginSignupFn = func(context.Context, string, string) (passkeys.Options, error) {
		return passkeys.Options{}, passkeys.ErrInviteInvalid
	}
	rr = postJSONRecorder(router, "/register/passkey/begin", `{"code":"gone","username":"freshuser"}`)
	if rr.Code != http.StatusBadRequest || jsonBody(t, rr)["error"] != "This invite is invalid or has already been used." {
		t.Fatalf("invalid invite response = %d %v", rr.Code, jsonBody(t, rr))
	}
}

func postJSONRecorder(router http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestSetPasswordRequiresPasskey(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()

	user, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "no_password", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.DB().ExecContext(ctx, `UPDATE users SET password_set = 0 WHERE id = ?`, user.ID); err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)

	stub := &stubPasskeys{}
	reauthCalls := 0
	stub.finishReauthFn = func(context.Context, *database.User, string, *http.Request) error {
		reauthCalls++
		return nil
	}
	app.SetPasskeyService(stub)
	router := app.Routes()

	// Short password is rejected before any passkey work.
	rr := postJSONRecorderAuth(router, "/account/password/passkey/finish", `{"new_password":"short","id":"x"}`, cookie)
	if rr.Code != http.StatusBadRequest || reauthCalls != 0 {
		t.Fatalf("short password = %d (reauth calls %d)", rr.Code, reauthCalls)
	}

	// A valid request verifies the assertion, then stores the password and
	// flips password_set.
	rr = postJSONRecorderAuth(router, "/account/password/passkey/finish", `{"new_password":"longenough","id":"x","rawId":"y","response":{}}`, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("set password = %d: %s", rr.Code, rr.Body.String())
	}
	if reauthCalls != 1 {
		t.Fatalf("reauth calls = %d, want 1", reauthCalls)
	}
	updated, err := queries.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.PasswordSet != 1 || !auth.CheckPassword(updated.PasswordHash, "longenough") {
		t.Fatalf("password not stored: %+v", updated)
	}

	// A failed assertion must not change the password.
	stub.finishReauthFn = func(context.Context, *database.User, string, *http.Request) error {
		return passkeys.ErrCeremony
	}
	rr = postJSONRecorderAuth(router, "/account/password/passkey/finish", `{"new_password":"anotherpass","id":"x"}`, cookie)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("failed reauth = %d, want 400", rr.Code)
	}
	after, _ := queries.GetUserByID(ctx, user.ID)
	if auth.CheckPassword(after.PasswordHash, "anotherpass") {
		t.Fatalf("password changed despite a failed assertion")
	}
}

func postJSONRecorderAuth(router http.Handler, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// Password-only re-verification is skipped for passkey-only accounts (they
// have no password); a session plus the authenticator's own user verification
// is the proof, and password_set stays 0.
func TestPasskeyRegisterBeginWithoutPassword(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()

	user, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "nopw_add", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.DB().ExecContext(ctx, `UPDATE users SET password_set = 0 WHERE id = ?`, user.ID); err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)
	app.SetPasskeyService(&stubPasskeys{})
	router := app.Routes()

	rr := postJSONRecorderAuth(router, "/account/passkeys/begin", `{}`, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("passkey-only begin = %d: %s", rr.Code, rr.Body.String())
	}
	if webAuthnCookie(rr) == nil {
		t.Fatalf("no ceremony cookie for passkey-only begin")
	}

	// The account page offers "Set a password" and drops the password field
	// from the add-passkey form.
	req := httptest.NewRequest(http.MethodGet, "/account", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `id="set-password-form"`) {
		t.Errorf("passkey-only account page is missing the set-password form")
	}
	if strings.Contains(body, `id="passkey_password"`) {
		t.Errorf("passkey-only account page still asks for a password to add a passkey")
	}
	if !strings.Contains(body, "/static/passkeys.js?v=4") {
		t.Errorf("passkey script missing on the account page")
	}
}

// End-to-end through the real routes and the real WebAuthn service: invite ->
// begin -> virtual authenticator attestation -> finish -> session.
func TestPasskeySignupThroughRoutes(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()

	admin, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "invite_admin", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateInvite(ctx, database.CreateInviteParams{Code: "route-invite", CreatedBy: admin.ID}); err != nil {
		t.Fatal(err)
	}
	svc, err := passkeys.NewService(passkeys.Config{
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		RPDisplayName: "Disjoint",
	}, queries)
	if err != nil {
		t.Fatal(err)
	}
	app.SetPasskeyService(svc)
	router := app.Routes()

	rr := postJSONRecorder(router, "/register/passkey/begin", `{"code":"route-invite","username":"routeuser"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("begin = %d: %s", rr.Code, rr.Body.String())
	}
	ceremony := webAuthnCookie(rr)
	if ceremony == nil {
		t.Fatalf("no ceremony cookie")
	}
	var begin struct {
		PublicKey struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &begin); err != nil {
		t.Fatal(err)
	}
	handle, err := base64.RawURLEncoding.DecodeString(begin.PublicKey.User.ID)
	if err != nil {
		t.Fatalf("decode user handle: %v", err)
	}

	attOptions, err := virtualwebauthn.ParseAttestationOptions(rr.Body.String())
	if err != nil {
		t.Fatalf("parse attestation options: %v", err)
	}
	rp := virtualwebauthn.RelyingParty{ID: "localhost", Name: "Disjoint", Origin: "http://localhost"}
	authenticator := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserHandle: handle})
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	attestation := virtualwebauthn.CreateAttestationResponse(rp, authenticator, cred, *attOptions)

	req := httptest.NewRequest(http.MethodPost, "/register/passkey/finish", strings.NewReader(attestation))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ceremony)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("finish = %d: %s", rec.Code, rec.Body.String())
	}
	var session bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName && c.Value != "" {
			session = true
		}
	}
	if !session {
		t.Fatalf("signup did not start a session")
	}

	account, err := queries.GetUserByUsernameFold(ctx, "routeuser")
	if err != nil {
		t.Fatalf("account missing: %v", err)
	}
	if account.PendingUntil.Valid || account.PasswordSet != 0 {
		t.Fatalf("account state = %+v", account)
	}
	if creds, err := queries.ListWebAuthnCredentialsByUser(ctx, account.ID); err != nil || len(creds) != 1 {
		t.Fatalf("credentials = %d (err=%v)", len(creds), err)
	}
	// The invite is now used, and a second signup with it is refused.
	if _, err := queries.GetInviteByCode(ctx, "route-invite"); err == nil {
		t.Fatalf("invite still redeemable")
	}
}
