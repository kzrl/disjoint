package handlers

import (
	"database/sql"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"disjoint/internal/auth"
	"disjoint/internal/database"
)

const loginLinkTTL = time.Hour

// loginLinkURL builds the absolute one-time link. In production
// PUBLIC_BASE_URL is set; the request-derived fallback is for local dev and
// behind a proxy that forwards X-Forwarded-Proto.
func (app *App) loginLinkURL(r *http.Request, token string) string {
	base := app.baseURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return strings.TrimRight(base, "/") + "/auth/magic?token=" + url.QueryEscape(token)
}

// CreateLoginLinkSubmit lets an administrator mint a one-time login link for a
// user. The raw token is shown once in the response; only its hash is stored.
func (app *App) CreateLoginLinkSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := requireAdmin(w, r)
	if admin == nil {
		return
	}
	if !allowUser(app.loginLinkLimiter, admin.ID) {
		setFlash(w, r, "flash_error", "Too many login links. Try again in a few minutes.")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	targetID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		setFlash(w, r, "flash_error", "Invalid user ID")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	target, err := app.queries.GetUserByID(ctx, targetID)
	if err != nil {
		setFlash(w, r, "flash_error", "User not found")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	token, err := auth.GenerateToken()
	if err != nil {
		http.Error(w, "failed to generate login link", http.StatusInternalServerError)
		return
	}
	expiresAt := time.Now().Add(loginLinkTTL)

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "failed to create login link", http.StatusInternalServerError)
		return
	}
	qtx := app.queries.WithTx(tx)
	if err := qtx.DeleteLoginTokensForUser(ctx, target.ID); err != nil {
		tx.Rollback()
		http.Error(w, "failed to create login link", http.StatusInternalServerError)
		return
	}
	if err := qtx.CreateLoginToken(ctx, database.CreateLoginTokenParams{
		TokenHash: auth.TokenHash(token),
		UserID:    target.ID,
		CreatedBy: sql.NullInt64{Int64: admin.ID, Valid: true},
		ExpiresAt: expiresAt.Unix(),
	}); err != nil {
		tx.Rollback()
		http.Error(w, "failed to create login link", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "failed to create login link", http.StatusInternalServerError)
		return
	}

	audit(r, admin.ID, "create_login_link", target.ID)
	app.render(w, r, "admin_login_link.html", map[string]any{
		"Target":    target,
		"Link":      app.loginLinkURL(r, token),
		"ExpiresAt": expiresAt,
	}, "Login Link")
}

// MagicLinkPage validates a one-time token and renders the confirmation page.
// It never consumes the token: silently prefetching a link must not use it.
func (app *App) MagicLinkPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	data := map[string]any{"Valid": false}
	if token != "" {
		row, err := app.queries.GetLoginToken(r.Context(), auth.TokenHash(token))
		if err == nil && !row.UsedAt.Valid && row.ExpiresAt > time.Now().Unix() {
			data["Valid"] = true
			data["Token"] = token
			data["Username"] = row.Username
			data["ExpiresAt"] = time.Unix(row.ExpiresAt, 0)
		}
	}
	app.render(w, r, "magic_confirm.html", data, "Sign In")
}

// MagicLinkSubmit atomically consumes the token and creates a normal
// server-side session. The token is a bearer credential: it is never logged.
func (app *App) MagicLinkSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := r.FormValue("token")
	if token == "" {
		app.render(w, r, "magic_confirm.html", map[string]any{"Valid": false}, "Sign In")
		return
	}

	userID, err := app.queries.ConsumeLoginToken(ctx, database.ConsumeLoginTokenParams{
		TokenHash: auth.TokenHash(token),
		Now:       time.Now().Unix(),
	})
	if err != nil {
		app.render(w, r, "magic_confirm.html", map[string]any{"Valid": false}, "Sign In")
		return
	}

	user, err := app.queries.GetUserByID(ctx, userID)
	if err != nil {
		http.Error(w, "login account no longer exists", http.StatusGone)
		return
	}

	// Any other pending links for this user are now redundant.
	_ = app.queries.DeleteLoginTokensForUser(ctx, userID)

	if err := app.startSession(ctx, w, r, user.ID); err != nil {
		http.Error(w, "failed to start session", http.StatusInternalServerError)
		return
	}
	audit(r, user.ID, "magic_login", user.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
