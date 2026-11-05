package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
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

// GenerateToken returns a fresh cryptographically random opaque token
// (64 hex characters). It is used for session cookies and one-time login
// links; the caller decides how to hash/store it.
func GenerateToken() (string, error) {
	b := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GenerateSessionToken returns a fresh session token.
func GenerateSessionToken() (string, error) {
	return GenerateToken()
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
