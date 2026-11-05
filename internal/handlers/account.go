package handlers

import (
	"net/http"

	"disjoint/internal/auth"
	"disjoint/internal/database"
	"disjoint/internal/middleware"
)

// AccountPage renders the viewer's account-security page: password change and
// session revocation. Profile editing lives on the public user card.
func (app *App) AccountPage(w http.ResponseWriter, r *http.Request) {
	app.render(w, r, "account.html", map[string]any{}, "Account")
}

// ChangePasswordSubmit verifies the current password, stores the new hash, and
// revokes every session for the user — including the current one. The viewer
// must sign in again with the new password, which is the simplest way to make
// "change password" also mean "sign out everywhere".
func (app *App) ChangePasswordSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if !allowUser(app.passwordLimiter, user.ID) {
		setFlash(w, r, "flash_error", "Too many password attempts. Try again in a few minutes.")
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}

	current := r.FormValue("current_password")
	newPassword := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")

	fail := func(message string) {
		setFlash(w, r, "flash_error", message)
		http.Redirect(w, r, "/account", http.StatusSeeOther)
	}

	switch {
	case current == "" || newPassword == "" || confirm == "":
		fail("All password fields are required.")
		return
	case newPassword != confirm:
		fail("New password and confirmation do not match.")
		return
	case len(newPassword) < minPasswordLength:
		fail("New password must be at least 8 characters.")
		return
	case newPassword == current:
		fail("New password must be different from the current password.")
		return
	}

	if !auth.CheckPassword(user.PasswordHash, current) {
		fail("Current password is incorrect.")
		return
	}

	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		fail("Failed to hash password.")
		return
	}

	if err := app.queries.UpdateUserPassword(ctx, database.UpdateUserPasswordParams{
		PasswordHash: hash,
		ID:           user.ID,
	}); err != nil {
		http.Error(w, "failed to update password", http.StatusInternalServerError)
		return
	}

	audit(r, user.ID, "change_password", user.ID)
	if err := app.queries.DeleteSessionsForUser(ctx, user.ID); err != nil {
		http.Error(w, "password changed, but failed to revoke sessions", http.StatusInternalServerError)
		return
	}
	auth.ClearSessionCookie(w, secureCookie(r))

	setFlash(w, r, "flash_success", "Password changed. Sign in again on every device.")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// SignOutEverywhereSubmit revokes all of the viewer's sessions and clears the
// current cookie. The session row is gone, so even a copied cookie stops
// working immediately.
func (app *App) SignOutEverywhereSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if err := app.queries.DeleteSessionsForUser(ctx, user.ID); err != nil {
		http.Error(w, "failed to revoke sessions", http.StatusInternalServerError)
		return
	}
	audit(r, user.ID, "sign_out_everywhere", user.ID)
	auth.ClearSessionCookie(w, secureCookie(r))

	setFlash(w, r, "flash_success", "Signed out everywhere.")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
