package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	SessionCookieName = "disjoint_session"
	// SessionDuration is the absolute maximum lifetime of a login.
	SessionDuration = 30 * 24 * time.Hour
	// SessionIdleTimeout is how long a session may go unused before it stops
	// authenticating. The row remains until the absolute expiry, but the
	// middleware rejects it after this cutoff.
	SessionIdleTimeout = 7 * 24 * time.Hour
	// sessionTokenBytes is the entropy of a session token (256 bits).
	sessionTokenBytes = 32
)

// HashPassword hashes the password using bcrypt.
func HashPassword(password string) (string, error) {
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hashed), nil
}

// CheckPassword checks if the password matches the hash.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// GenerateSessionToken returns a fresh cryptographically random session
// token (64 hex characters). It is placed in the session cookie.
func GenerateSessionToken() (string, error) {
	b := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// TokenHash is the value stored in the sessions table for a token. Storing
// the hash instead of the token means a leaked database does not leak live
// sessions.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// GetSessionTokenFromRequest extracts the raw session token from the
// request cookie. Returns "" if no cookie is present.
func GetSessionTokenFromRequest(r *http.Request) string {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return ""
	}
	return cookie.Value
}

// SecureCookie reports whether the session and flash cookies should carry the
// Secure flag. COOKIE_SECURE forces the decision ("1"/"true" on, "0"/"false"
// off) for deployments behind a TLS-terminating proxy where r.TLS is nil;
// otherwise TLS on the current request is detected automatically. Shared by the
// handlers and the auth middleware so a disabled-session cookie is cleared with
// the same attributes it was set with.
func SecureCookie(r *http.Request) bool {
	switch strings.ToLower(os.Getenv("COOKIE_SECURE")) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return r.TLS != nil
}

// SetSessionCookie sets the session cookie on the response. secure marks
// it HTTPS-only (correct when TLS is terminated at this process or a proxy
// in front of it).
func SetSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(SessionDuration.Seconds()),
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie expires the session cookie on the response.
func ClearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
