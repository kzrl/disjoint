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
	// SessionCookieNameSecure is the cookie name used whenever the Secure flag
	// is set. The __Host- prefix makes browsers reject a same-named cookie set
	// by a sibling subdomain, without Secure, or with a Path other than "/".
	// The plain name is kept for local HTTP development, where browsers would
	// reject a Secure cookie.
	SessionCookieNameSecure = "__Host-disjoint_session"
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
// request cookie. Both the secure (__Host-) and local-development names are
// accepted so a deployment can move between HTTP and HTTPS without logging
// everyone out. Returns "" if no cookie is present.
func GetSessionTokenFromRequest(r *http.Request) string {
	for _, name := range []string{SessionCookieNameSecure, SessionCookieName} {
		if cookie, err := r.Cookie(name); err == nil && cookie.Value != "" {
			return cookie.Value
		}
	}
	return ""
}

// sessionCookieName returns the name to set for the given secure flag.
func sessionCookieName(secure bool) string {
	if secure {
		return SessionCookieNameSecure
	}
	return SessionCookieName
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
		Name:     sessionCookieName(secure),
		Value:    token,
		Path:     "/",
		MaxAge:   int(SessionDuration.Seconds()),
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie expires the session cookie on the response. Both names
// are expired: a deployment that switched HTTP/HTTPS may still have the other
// name in a browser.
func ClearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(secure),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	if secure {
		// Also clear the plain name, which is what a previous HTTP-only
		// session (or local development) would have set.
		http.SetCookie(w, &http.Cookie{
			Name:     SessionCookieName,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}
}
