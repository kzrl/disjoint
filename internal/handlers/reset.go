package handlers

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"disjoint/internal/auth"
	"disjoint/internal/database"
)

// passwordResetTTL is how long an admin-issued reset link stays valid. Short on
// purpose: the admin hands it over in chat, and the holder sets a password
// immediately.
const passwordResetTTL = time.Hour

// resetLinkURL builds the absolute link shown to the admin. PUBLIC_BASE_URL is
// set in production; the request-derived fallback is for local dev and for a
// proxy that forwards X-Forwarded-Proto.
func (app *App) resetLinkURL(r *http.Request, token string) string {
	base := app.baseURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return strings.TrimRight(base, "/") + "/reset?token=" + url.QueryEscape(token)
}

// CreateResetLinkSubmit mints a single-use password reset link for a user. Only
// the token hash is stored; the raw link is shown once in the flash message
// because this instance has no mail delivery.
func (app *App) CreateResetLinkSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := requireAdmin(w, r)
	if admin == nil {
		return
	}

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		setFlash(w, r, "flash_error", "Invalid user ID")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	if id == admin.ID {
		setFlash(w, r, "flash_error", "Use the Change password form for your own account")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	target, err := app.queries.GetUserByID(ctx, id)
	if err != nil || target.PendingUntil.Valid {
		setFlash(w, r, "flash_error", "User not found")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		setFlash(w, r, "flash_error", "Failed to create reset link")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	token := hex.EncodeToString(raw)

	// Issuing a new link invalidates any earlier one for the same account.
	if err := app.queries.DeletePasswordResetTokensForUser(ctx, target.ID); err != nil {
		setFlash(w, r, "flash_error", "Failed to create reset link")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	if err := app.queries.CreatePasswordResetToken(ctx, database.CreatePasswordResetTokenParams{
		TokenHash: auth.TokenHash(token),
		UserID:    target.ID,
		CreatedBy: sql.NullInt64{Int64: admin.ID, Valid: true},
		ExpiresAt: time.Now().UTC().Add(passwordResetTTL).Round(0),
	}); err != nil {
		setFlash(w, r, "flash_error", "Failed to create reset link")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	audit(r, admin.ID, "create_reset_link", target.ID)
	setFlash(w, r, "flash_success", "Reset link for "+target.Username+" (valid for 1 hour): "+app.resetLinkURL(r, token))
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ResetPage renders the set-a-new-password form for a valid, unused token.
func (app *App) ResetPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		app.render(w, r, "reset.html", map[string]any{"Invalid": true}, "Reset password")
		return
	}

	userID, err := app.queries.GetPasswordResetToken(r.Context(), auth.TokenHash(token))
	if err != nil {
		app.render(w, r, "reset.html", map[string]any{"Invalid": true}, "Reset password")
		return
	}
	account, err := app.queries.GetUserByID(r.Context(), userID)
	if err != nil || account.PendingUntil.Valid {
		app.render(w, r, "reset.html", map[string]any{"Invalid": true}, "Reset password")
		return
	}

	app.render(w, r, "reset.html", map[string]any{
		"Token":    token,
		"Username": account.Username,
	}, "Reset password")
}

// ResetSubmit consumes the token and stores the new password. It never signs the
// holder in; it revokes every existing session and sends them to /login, so a
// stolen reset link cannot silently become a live session.
func (app *App) ResetSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := strings.TrimSpace(r.FormValue("token"))
	newPassword := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")

	fail := func(message string) {
		setFlash(w, r, "flash_error", message)
		http.Redirect(w, r, "/reset?token="+url.QueryEscape(token), http.StatusSeeOther)
	}
	invalid := func() {
		setFlash(w, r, "flash_error", "That reset link is invalid or has expired. Ask an administrator for a new one.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}

	if token == "" {
		invalid()
		return
	}
	switch {
	case newPassword == "" || confirm == "":
		fail("All password fields are required.")
		return
	case newPassword != confirm:
		fail("New password and confirmation do not match.")
		return
	case len(newPassword) < minPasswordLength:
		fail("Password must be at least 8 characters.")
		return
	}

	userID, err := app.queries.ConsumePasswordResetToken(ctx, auth.TokenHash(token))
	if errors.Is(err, sql.ErrNoRows) {
		invalid()
		return
	}
	if err != nil {
		http.Error(w, "failed to reset password", http.StatusInternalServerError)
		return
	}

	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		fail("Failed to hash password.")
		return
	}
	if err := app.queries.SetUserPassword(ctx, database.SetUserPasswordParams{
		PasswordHash: hash,
		ID:           userID,
	}); err != nil {
		http.Error(w, "failed to reset password", http.StatusInternalServerError)
		return
	}
	// A reset is a recovery action: every old session and any other pending
	// link for the account is revoked.
	if err := app.queries.DeleteSessionsForUser(ctx, userID); err != nil {
		http.Error(w, "password reset, but failed to revoke sessions", http.StatusInternalServerError)
		return
	}
	_ = app.queries.DeletePasswordResetTokensForUser(ctx, userID)
	audit(r, 0, "reset_password", userID)

	setFlash(w, r, "flash_success", "Password updated. Sign in with your new password.")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ResetTokenUser is a small helper for tests and diagnostics: it reports the
// account a reset token belongs to without consuming it.
func (app *App) ResetTokenUser(r *http.Request, token string) (int64, error) {
	return app.queries.GetPasswordResetToken(r.Context(), auth.TokenHash(token))
}
