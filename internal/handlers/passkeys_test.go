package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"disjoint/internal/auth"
	"disjoint/internal/database"
	"disjoint/internal/passkeys"
)

// stubPasskeys implements handlers.PasskeyService without any WebAuthn crypto,
// so the HTTP contract (auth, password confirmation, cookies, session start,
// scoping) can be tested deterministically. The real ceremony is covered by
// internal/passkeys' virtual-authenticator tests.
type stubPasskeys struct {
	beginRegFn  func(ctx context.Context, account *database.User) (passkeys.Options, error)
	finishRegFn func(ctx context.Context, account *database.User, token string, r *http.Request) (string, error)
	beginLogin  func(ctx context.Context) (passkeys.Options, error)
	finishLogin func(ctx context.Context, token string, r *http.Request) (int64, string, error)
	listFn      func(ctx context.Context, userID int64) ([]database.WebauthnCredential, error)
	deleteFn    func(ctx context.Context, userID int64, id string) (bool, error)
	renameFn    func(ctx context.Context, userID int64, id, name string) (bool, error)

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
