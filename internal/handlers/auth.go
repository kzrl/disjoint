package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"disjoint/internal/auth"
	"disjoint/internal/database"
)

// usernamePattern restricts new usernames to ASCII alphanumerics so that
// /users/{name} URLs are unambiguous (legacy users keep whatever they have).
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9]{3,32}$`)

// secureCookie reports whether session cookies should carry the Secure
// flag. COOKIE_SECURE forces the decision ("1"/"true" on, "0"/"false" off)
// for deployments behind a TLS-terminating proxy where r.TLS is nil;
// otherwise TLS on the current request is detected automatically.
func secureCookie(r *http.Request) bool {
	switch strings.ToLower(os.Getenv("COOKIE_SECURE")) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return r.TLS != nil
}

// startSession creates a server-side session row for the user and sets the
// cookie holding the (unhashed) token. The table stores only the token's
// SHA-256 hash.
func (app *App) startSession(ctx context.Context, w http.ResponseWriter, r *http.Request, userID int64) error {
	token, err := auth.GenerateSessionToken()
	if err != nil {
		return err
	}
	if err := app.queries.CreateSession(ctx, database.CreateSessionParams{
		TokenHash: auth.TokenHash(token),
		UserID:    userID,
		ExpiresAt: time.Now().Add(auth.SessionDuration).Unix(),
	}); err != nil {
		return err
	}
	auth.SetSessionCookie(w, token, secureCookie(r))
	return nil
}

// SetupPage renders the first-run administrator creation form.
func (app *App) SetupPage(w http.ResponseWriter, r *http.Request) {
	count, err := app.queries.CountUsers(r.Context())
	if err == nil && count > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	app.render(w, r, "setup.html", map[string]any{}, "First-Run Setup")
}

// SetupSubmit creates the initial administrative user.
func (app *App) SetupSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	count, err := app.queries.CountUsers(ctx)
	if err == nil && count > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	if !usernamePattern.MatchString(username) {
		setFlash(w, "flash_error", "Username must be 3-32 letters/digits only.")
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if len(password) < 6 {
		setFlash(w, "flash_error", "Password must be at least 6 characters.")
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		setFlash(w, "flash_error", "Failed to hash password.")
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}

	user, err := app.queries.CreateUser(ctx, database.CreateUserParams{
		Username:     username,
		PasswordHash: hash,
	})
	if err != nil {
		setFlash(w, "flash_error", "Failed to create administrator account.")
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}

	_ = app.queries.SetUserAdmin(ctx, user.ID)

	app.middleware.RefreshUserCount(ctx)
	if err := app.startSession(ctx, w, r, user.ID); err != nil {
		setFlash(w, "flash_error", "Failed to start session.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	setFlash(w, "flash_success", "Welcome to Disjoint! Setup complete.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// LoginPage renders the login form.
func (app *App) LoginPage(w http.ResponseWriter, r *http.Request) {
	app.render(w, r, "login.html", map[string]any{}, "Sign In")
}

// LoginSubmit authenticates the user and creates an HTTP-only session.
func (app *App) LoginSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	if username == "" || password == "" {
		setFlash(w, "flash_error", "Username and password are required.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	user, err := app.queries.GetUserByUsername(ctx, username)
	if err != nil {
		setFlash(w, "flash_error", "Invalid username or password.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if !auth.CheckPassword(user.PasswordHash, password) {
		setFlash(w, "flash_error", "Invalid username or password.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if err := app.startSession(ctx, w, r, user.ID); err != nil {
		setFlash(w, "flash_error", "Failed to start session.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// RegisterPage renders the registration form if a valid invite code is provided.
func (app *App) RegisterPage(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		app.render(w, r, "register_closed.html", map[string]any{}, "Invite Only")
		return
	}

	_, err := app.queries.GetInviteByCode(r.Context(), code)
	if err != nil {
		app.render(w, r, "register_closed.html", map[string]any{}, "Invite Only")
		return
	}

	app.render(w, r, "register.html", map[string]any{"Code": code}, "Register")
}

// RegisterSubmit registers a new user.
func (app *App) RegisterSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	code := r.FormValue("code")

	if code == "" {
		setFlash(w, "flash_error", "Invalid invite link.")
		http.Redirect(w, r, "/register", http.StatusSeeOther)
		return
	}

	_, err := app.queries.GetInviteByCode(ctx, code)
	if err != nil {
		setFlash(w, "flash_error", "Invalid or already used invite code.")
		http.Redirect(w, r, "/register", http.StatusSeeOther)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	redirectUrl := fmt.Sprintf("/register?code=%s", code)

	if !usernamePattern.MatchString(username) {
		setFlash(w, "flash_error", "Username must be 3-32 letters/digits only.")
		http.Redirect(w, r, redirectUrl, http.StatusSeeOther)
		return
	}

	if len(password) < 6 {
		setFlash(w, "flash_error", "Password must be at least 6 characters.")
		http.Redirect(w, r, redirectUrl, http.StatusSeeOther)
		return
	}

	// Check if username already taken
	if _, err := app.queries.GetUserByUsername(ctx, username); err == nil {
		setFlash(w, "flash_error", "Username is already taken.")
		http.Redirect(w, r, redirectUrl, http.StatusSeeOther)
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		setFlash(w, "flash_error", "Failed to hash password.")
		http.Redirect(w, r, redirectUrl, http.StatusSeeOther)
		return
	}

	user, err := app.queries.CreateUser(ctx, database.CreateUserParams{
		Username:     username,
		PasswordHash: hash,
	})
	if err != nil {
		setFlash(w, "flash_error", "Failed to create account.")
		http.Redirect(w, r, redirectUrl, http.StatusSeeOther)
		return
	}

	// ConsumeInvite is atomic (WHERE used_by IS NULL) and reports how many
	// rows it updated. 0 means a concurrent registration took the invite
	// first — roll this account back instead of letting one code create
	// two users.
	rows, err := app.queries.ConsumeInvite(ctx, database.ConsumeInviteParams{UsedBy: sql.NullInt64{Int64: user.ID, Valid: true}, Code: code})
	if err != nil || rows == 0 {
		_ = app.queries.DeleteUser(ctx, user.ID)
		setFlash(w, "flash_error", "Invalid or already used invite code.")
		http.Redirect(w, r, redirectUrl, http.StatusSeeOther)
		return
	}

	if err := app.startSession(ctx, w, r, user.ID); err != nil {
		_ = app.queries.DeleteUser(ctx, user.ID)
		setFlash(w, "flash_error", "Failed to start session.")
		http.Redirect(w, r, redirectUrl, http.StatusSeeOther)
		return
	}
	setFlash(w, "flash_success", "Account created successfully!")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// LogoutSubmit terminates the active session: the row is deleted so the
// token stops working immediately, and the cookie is expired client-side.
func (app *App) LogoutSubmit(w http.ResponseWriter, r *http.Request) {
	if token := auth.GetSessionTokenFromRequest(r); token != "" {
		// Best-effort: the cookie is cleared regardless, and an unknown or
		// already-expired token is harmless.
		_ = app.queries.DeleteSession(r.Context(), auth.TokenHash(token))
	}
	auth.ClearSessionCookie(w, secureCookie(r))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
