package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	SessionCookieName = "disjoint_session"
	SessionDuration   = 30 * 24 * time.Hour
)

// HashPassword hashes a plaintext password using bcrypt.
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

// CheckPassword verifies a plaintext password against a bcrypt hash.
func CheckPassword(hash, password string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// CreateSessionToken generates an HMAC-signed session string for a user.
func CreateSessionToken(userID int64, secret string) string {
	expiresAt := time.Now().Add(SessionDuration).Unix()
	payload := fmt.Sprintf("%d:%d", userID, expiresAt)

	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(payload))
	signature := hex.EncodeToString(h.Sum(nil))

	return fmt.Sprintf("%s:%s", payload, signature)
}

// VerifySessionToken verifies the session token and returns the userID if valid.
func VerifySessionToken(token string, secret string) (int64, error) {
	parts := strings.Split(token, ":")
	if len(parts) != 3 {
		return 0, errors.New("invalid session token format")
	}

	userIDStr := parts[0]
	expiresAtStr := parts[1]
	signature := parts[2]

	payload := fmt.Sprintf("%s:%s", userIDStr, expiresAtStr)
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(payload))
	expectedSig := hex.EncodeToString(h.Sum(nil))

	if !hmac.Equal([]byte(signature), []byte(expectedSig)) {
		return 0, errors.New("invalid session signature")
	}

	expiresAt, err := strconv.ParseInt(expiresAtStr, 10, 64)
	if err != nil || time.Now().Unix() > expiresAt {
		return 0, errors.New("session token expired")
	}

	userID, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil {
		return 0, errors.New("invalid user id in session token")
	}

	return userID, nil
}

// SetSessionCookie sets the HTTP-only session cookie.
func SetSessionCookie(w http.ResponseWriter, userID int64, secret string) {
	token := CreateSessionToken(userID, secret)
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionDuration.Seconds()),
	})
}

// ClearSessionCookie invalidates the session cookie.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

// GetUserIDFromRequest extracts and verifies the user ID from the request cookie.
func GetUserIDFromRequest(r *http.Request, secret string) (int64, error) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		return 0, err
	}
	return VerifySessionToken(cookie.Value, secret)
}
