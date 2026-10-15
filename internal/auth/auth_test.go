package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSetSessionCookieFlags(t *testing.T) {
	for _, secure := range []bool{false, true} {
		rec := httptest.NewRecorder()
		SetSessionCookie(rec, "token", secure)

		var cookie *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == SessionCookieName {
				cookie = c
				break
			}
		}
		if cookie == nil {
			t.Fatalf("secure=%v: no session cookie set", secure)
		}
		if cookie.Secure != secure {
			t.Errorf("secure=%v: cookie.Secure = %v", secure, cookie.Secure)
		}
		if !cookie.HttpOnly {
			t.Error("cookie.HttpOnly = false, want true")
		}
		if cookie.SameSite != http.SameSiteLaxMode {
			t.Errorf("cookie.SameSite = %v, want %v", cookie.SameSite, http.SameSiteLaxMode)
		}
		if cookie.MaxAge <= 0 {
			t.Errorf("cookie.MaxAge = %d, want > 0", cookie.MaxAge)
		}
	}
}

func TestSessionTokenEntropy(t *testing.T) {
	tok, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken: %v", err)
	}
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(tok))
	}
	// Two generated tokens must differ (256 bits of randomness).
	other, _ := GenerateSessionToken()
	if tok == other {
		t.Fatal("two generated tokens are identical")
	}
}
