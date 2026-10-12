package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"

	"disjoint/internal/database"
	"disjoint/internal/middleware"
)

func requireAdmin(w http.ResponseWriter, r *http.Request) *database.User {
	user := middleware.UserFromContext(r.Context())
	if user == nil || user.IsAdmin != 1 {
		setFlash(w, "flash_error", "Unauthorized")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return nil
	}
	return user
}

func (app *App) InvitesPage(w http.ResponseWriter, r *http.Request) {
	user := requireAdmin(w, r)
	if user == nil {
		return
	}

	invites, err := app.queries.ListInvites(r.Context())
	if err != nil {
		setFlash(w, "flash_error", "Failed to load invites")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	app.render(w, r, "invites.html", map[string]any{
		"Invites": invites,
	}, "Invites")
}

func (app *App) CreateInviteSubmit(w http.ResponseWriter, r *http.Request) {
	user := requireAdmin(w, r)
	if user == nil {
		return
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		setFlash(w, "flash_error", "Failed to create invite")
		http.Redirect(w, r, "/invites", http.StatusSeeOther)
		return
	}
	code := hex.EncodeToString(b)

	_, err := app.queries.CreateInvite(r.Context(), database.CreateInviteParams{
		Code:      code,
		CreatedBy: user.ID,
	})
	if err != nil {
		setFlash(w, "flash_error", "Failed to create invite")
		http.Redirect(w, r, "/invites", http.StatusSeeOther)
		return
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	url := fmt.Sprintf("%s://%s/register?code=%s", scheme, r.Host, code)
	setFlash(w, "flash_success", fmt.Sprintf("Invite created: %s", url))

	http.Redirect(w, r, "/invites", http.StatusSeeOther)
}

func (app *App) DeleteInviteSubmit(w http.ResponseWriter, r *http.Request) {
	user := requireAdmin(w, r)
	if user == nil {
		return
	}

	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		setFlash(w, "flash_error", "Invalid invite ID")
		http.Redirect(w, r, "/invites", http.StatusSeeOther)
		return
	}

	err = app.queries.DeleteUnusedInvite(r.Context(), id)
	if err != nil {
		setFlash(w, "flash_error", "Failed to delete invite")
	} else {
		setFlash(w, "flash_success", "Invite deleted successfully")
	}

	http.Redirect(w, r, "/invites", http.StatusSeeOther)
}
