package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"disjoint/internal/auth"
	"disjoint/internal/database"
	"disjoint/internal/middleware"
	"disjoint/internal/passkeys"
)

// webauthnCookieName carries the opaque id of the in-progress ceremony. The
// ceremony itself (challenge, allowed credentials, user handle) lives in the
// database, so the cookie cannot be tampered with into a different challenge.
const webauthnCookieName = "disjoint_webauthn"

const webauthnCookieMaxAge = 300 // seconds

// PasskeyService is the slice of passkeys.Service the HTTP layer uses. Tests
// substitute a stub.
type PasskeyService interface {
	BeginRegistration(ctx context.Context, account *database.User) (passkeys.Options, error)
	FinishRegistration(ctx context.Context, account *database.User, token string, r *http.Request) (string, error)
	BeginLogin(ctx context.Context) (passkeys.Options, error)
	FinishLogin(ctx context.Context, token string, r *http.Request) (int64, string, error)
	BeginSignup(ctx context.Context, code, username string) (passkeys.Options, error)
	FinishSignup(ctx context.Context, token string, r *http.Request) (database.User, error)
	BeginReauth(ctx context.Context, account *database.User) (passkeys.Options, error)
	FinishReauth(ctx context.Context, account *database.User, token string, r *http.Request) error
	ListCredentials(ctx context.Context, userID int64) ([]database.WebauthnCredential, error)
	DeleteCredential(ctx context.Context, userID int64, id string) (bool, error)
	RenameCredential(ctx context.Context, userID int64, id, name string) (bool, error)
}

func (app *App) setWebAuthnCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     webauthnCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   webauthnCookieMaxAge,
		Secure:   auth.SecureCookie(r),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (app *App) clearWebAuthnCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     webauthnCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   auth.SecureCookie(r),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func webAuthnToken(r *http.Request) string {
	cookie, err := r.Cookie(webauthnCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// writeJSON is a tiny helper for the passkey endpoints; the client-side code
// reads data.ok, data.token and data.publicKey.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func jsonError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// PasskeyRegisterBegin starts adding a passkey to the signed-in account. The
// current password must be supplied: a stolen session alone must not be able to
// add a permanent credential.
func (app *App) PasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		jsonError(w, http.StatusUnauthorized, "Sign in first")
		return
	}
	if app.passkeys == nil {
		jsonError(w, http.StatusNotImplemented, "Passkeys are not configured")
		return
	}
	if !allowUser(app.passwordLimiter, user.ID) {
		jsonError(w, http.StatusTooManyRequests, "Too many attempts. Try again in a few minutes.")
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid request")
		return
	}
	// Password accounts re-verify the password. A passkey-only account has no
	// password to check: the session plus the authenticator's own user
	// verification are the proof, and adding a passkey cannot enable password
	// login (password_set stays 0 until the separate "set a password" flow).
	if user.PasswordSet == 1 && !auth.CheckPassword(user.PasswordHash, body.Password) {
		jsonError(w, http.StatusForbidden, "Password is incorrect")
		return
	}

	options, err := app.passkeys.BeginRegistration(ctx, user)
	if err != nil {
		log.Printf("[passkeys] begin registration failed for user %d: %v", user.ID, err)
		if errors.Is(err, passkeys.ErrCredentialLimit) {
			jsonError(w, http.StatusBadRequest, "You already have the maximum number of passkeys")
			return
		}
		http.Error(w, "failed to start passkey registration", http.StatusInternalServerError)
		return
	}
	app.setWebAuthnCookie(w, r, options.Token)
	writeJSON(w, http.StatusOK, map[string]any{"publicKey": options.PublicKey, "token": options.Token})
}

// PasskeyRegisterFinish verifies the attestation and stores the credential.
func (app *App) PasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		jsonError(w, http.StatusUnauthorized, "Sign in first")
		return
	}
	if app.passkeys == nil {
		jsonError(w, http.StatusNotImplemented, "Passkeys are not configured")
		return
	}
	token := webAuthnToken(r)
	app.clearWebAuthnCookie(w, r)

	name, err := app.passkeys.FinishRegistration(ctx, user, token, r)
	if err != nil {
		// Logged server-side (never sent to the client) so a failing browser
		// can be diagnosed from the journal.
		log.Printf("[passkeys] finish registration failed for user %d: %v", user.ID, err)
		if errors.Is(err, passkeys.ErrCeremony) || errors.Is(err, passkeys.ErrCredentialLimit) {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Verification failures are all the same to the client: no details
		// about why an assertion or attestation was rejected.
		jsonError(w, http.StatusBadRequest, "Passkey registration failed")
		return
	}
	audit(r, user.ID, "add_passkey", user.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

// PasskeyLoginBegin returns discoverable-credential options for an anonymous
// visitor.
func (app *App) PasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if app.passkeys == nil {
		jsonError(w, http.StatusNotImplemented, "Passkeys are not configured")
		return
	}
	options, err := app.passkeys.BeginLogin(r.Context())
	if err != nil {
		log.Printf("[passkeys] begin login failed: %v", err)
		http.Error(w, "failed to start passkey sign-in", http.StatusInternalServerError)
		return
	}
	app.setWebAuthnCookie(w, r, options.Token)
	writeJSON(w, http.StatusOK, map[string]any{"publicKey": options.PublicKey, "token": options.Token})
}

// PasskeyLoginFinish verifies an assertion, starts a session and returns the
// redirect target. Errors are deliberately generic: this endpoint is
// anonymous, so it must not reveal whether a credential or account exists.
func (app *App) PasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if app.passkeys == nil {
		jsonError(w, http.StatusNotImplemented, "Passkeys are not configured")
		return
	}
	token := webAuthnToken(r)
	app.clearWebAuthnCookie(w, r)

	userID, _, err := app.passkeys.FinishLogin(ctx, token, r)
	if err != nil {
		log.Printf("[passkeys] finish login failed: %v", err)
		if errors.Is(err, passkeys.ErrSignCountRegression) {
			audit(r, 0, "passkey_sign_count_regression", 0)
		}
		jsonError(w, http.StatusUnauthorized, "Passkey sign-in failed")
		return
	}

	account, err := app.queries.GetUserByID(ctx, userID)
	if err != nil {
		log.Printf("[passkeys] login succeeded for unknown user %d: %v", userID, err)
		jsonError(w, http.StatusUnauthorized, "Passkey sign-in failed")
		return
	}
	// Same rules as password login, but without telling the client why. A
	// pending signup is not an account yet.
	if account.IsDisabled == 1 || account.PendingUntil.Valid {
		jsonError(w, http.StatusUnauthorized, "Passkey sign-in failed")
		return
	}

	if err := app.startSession(ctx, w, r, account.ID); err != nil {
		jsonError(w, http.StatusInternalServerError, "Failed to start session")
		return
	}
	audit(r, account.ID, "passkey_login", account.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redirect": "/"})
}

// PasskeyDelete removes one of the viewer's passkeys.
func (app *App) PasskeyDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if app.passkeys == nil {
		setFlash(w, r, "flash_error", "Passkeys are not configured")
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}

	deleted, err := app.passkeys.DeleteCredential(ctx, user.ID, r.PathValue("id"))
	if err != nil {
		http.Error(w, "failed to delete passkey", http.StatusInternalServerError)
		return
	}
	if !deleted {
		http.NotFound(w, r)
		return
	}
	audit(r, user.ID, "delete_passkey", user.ID)
	setFlash(w, r, "flash_success", "Passkey deleted")
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// PasskeyRename relabels one of the viewer's passkeys.
func (app *App) PasskeyRename(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if app.passkeys == nil {
		setFlash(w, r, "flash_error", "Passkeys are not configured")
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	renamed, err := app.passkeys.RenameCredential(ctx, user.ID, r.PathValue("id"), name)
	if err != nil {
		http.Error(w, "failed to rename passkey", http.StatusInternalServerError)
		return
	}
	if !renamed {
		http.NotFound(w, r)
		return
	}
	audit(r, user.ID, "rename_passkey", user.ID)
	setFlash(w, r, "flash_success", "Passkey renamed")
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// PasskeySignupBegin starts a passkey-only account creation from an invite.
// Public (no session): the invite code is the authorisation.
func (app *App) PasskeySignupBegin(w http.ResponseWriter, r *http.Request) {
	if app.passkeys == nil {
		jsonError(w, http.StatusNotImplemented, "Passkeys are not configured")
		return
	}
	var body struct {
		Code     string `json:"code"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid request")
		return
	}
	username := strings.TrimSpace(body.Username)
	if !usernamePattern.MatchString(username) {
		jsonError(w, http.StatusBadRequest, "Username must be 3-32 letters/digits only.")
		return
	}
	if body.Code == "" {
		jsonError(w, http.StatusBadRequest, "Invalid invite link.")
		return
	}

	options, err := app.passkeys.BeginSignup(r.Context(), body.Code, username)
	switch {
	case errors.Is(err, passkeys.ErrInviteInvalid):
		jsonError(w, http.StatusBadRequest, "This invite is invalid or has already been used.")
		return
	case errors.Is(err, passkeys.ErrUsernameTaken):
		jsonError(w, http.StatusConflict, "Username is already taken.")
		return
	case err != nil:
		log.Printf("[passkeys] begin signup failed for %q: %v", username, err)
		http.Error(w, "failed to start passkey signup", http.StatusInternalServerError)
		return
	}
	app.setWebAuthnCookie(w, r, options.Token)
	writeJSON(w, http.StatusOK, map[string]any{"publicKey": options.PublicKey, "token": options.Token})
}

// PasskeySignupFinish verifies the attestation, activates the account, consumes
// the invite and starts a session.
func (app *App) PasskeySignupFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if app.passkeys == nil {
		jsonError(w, http.StatusNotImplemented, "Passkeys are not configured")
		return
	}
	token := webAuthnToken(r)
	app.clearWebAuthnCookie(w, r)

	account, err := app.passkeys.FinishSignup(ctx, token, r)
	if err != nil {
		log.Printf("[passkeys] finish signup failed: %v", err)
		switch {
		case errors.Is(err, passkeys.ErrInviteInvalid):
			jsonError(w, http.StatusBadRequest, "This invite is invalid or has already been used.")
		case errors.Is(err, passkeys.ErrUsernameTaken):
			jsonError(w, http.StatusConflict, "Username is already taken.")
		case errors.Is(err, passkeys.ErrPendingAccount):
			jsonError(w, http.StatusBadRequest, "That signup expired. Start again from your invite link.")
		default:
			jsonError(w, http.StatusBadRequest, "Passkey signup failed. Try again from your invite link.")
		}
		return
	}

	if err := app.startSession(ctx, w, r, account.ID); err != nil {
		jsonError(w, http.StatusInternalServerError, "Failed to start session")
		return
	}
	audit(r, account.ID, "passkey_signup", account.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redirect": "/account"})
}

// PasskeySetPasswordBegin starts the re-authentication ceremony a passkey-only
// account must pass before setting a password.
func (app *App) PasskeySetPasswordBegin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		jsonError(w, http.StatusUnauthorized, "Sign in first")
		return
	}
	if app.passkeys == nil {
		jsonError(w, http.StatusNotImplemented, "Passkeys are not configured")
		return
	}
	options, err := app.passkeys.BeginReauth(ctx, user)
	if err != nil {
		if errors.Is(err, passkeys.ErrNoCredentials) {
			jsonError(w, http.StatusBadRequest, "No passkey is registered on this account")
			return
		}
		log.Printf("[passkeys] begin reauth failed for user %d: %v", user.ID, err)
		http.Error(w, "failed to start passkey verification", http.StatusInternalServerError)
		return
	}
	app.setWebAuthnCookie(w, r, options.Token)
	writeJSON(w, http.StatusOK, map[string]any{"publicKey": options.PublicKey, "token": options.Token})
}

// PasskeySetPasswordFinish verifies the assertion and, in the same request,
// stores the new password. A session alone can never mint a password.
func (app *App) PasskeySetPasswordFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		jsonError(w, http.StatusUnauthorized, "Sign in first")
		return
	}
	if app.passkeys == nil {
		jsonError(w, http.StatusNotImplemented, "Passkeys are not configured")
		return
	}

	// Read the body once: the password is validated here, and the same bytes
	// are handed to the library (it ignores the extra field).
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid request")
		return
	}
	var body struct {
		NewPassword string `json:"new_password"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid request")
		return
	}
	if len(body.NewPassword) < minPasswordLength {
		jsonError(w, http.StatusBadRequest, "Password must be at least 8 characters.")
		return
	}

	token := webAuthnToken(r)
	app.clearWebAuthnCookie(w, r)

	innerReq := r.Clone(ctx)
	innerReq.Body = io.NopCloser(bytes.NewReader(raw))
	if err := app.passkeys.FinishReauth(ctx, user, token, innerReq); err != nil {
		log.Printf("[passkeys] reauth failed for user %d: %v", user.ID, err)
		jsonError(w, http.StatusBadRequest, "Passkey verification failed")
		return
	}

	hash, err := auth.HashPassword(body.NewPassword)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "Failed to hash password")
		return
	}
	if err := app.queries.SetUserPassword(ctx, database.SetUserPasswordParams{
		PasswordHash: hash,
		ID:           user.ID,
	}); err != nil {
		http.Error(w, "failed to set password", http.StatusInternalServerError)
		return
	}
	audit(r, user.ID, "set_password", user.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redirect": "/account"})
}
