package handlers

import (
	"net/http"
	"strconv"
)

func (app *App) UsersPage(w http.ResponseWriter, r *http.Request) {
	user := requireAdmin(w, r)
	if user == nil {
		return
	}

	users, err := app.queries.ListUsers(r.Context())
	if err != nil {
		setFlash(w, "flash_error", "Failed to load users")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	app.render(w, r, "users.html", map[string]any{
		"Users": users,
	}, "Users")
}

func (app *App) PromoteUserSubmit(w http.ResponseWriter, r *http.Request) {
	user := requireAdmin(w, r)
	if user == nil {
		return
	}

	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		setFlash(w, "flash_error", "Invalid user ID")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	err = app.queries.SetUserAdmin(r.Context(), id)
	if err != nil {
		setFlash(w, "flash_error", "Failed to promote user")
	} else {
		setFlash(w, "flash_success", "User promoted to admin")
	}

	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func (app *App) DemoteUserSubmit(w http.ResponseWriter, r *http.Request) {
	user := requireAdmin(w, r)
	if user == nil {
		return
	}

	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		setFlash(w, "flash_error", "Invalid user ID")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	if id == user.ID {
		setFlash(w, "flash_error", "You cannot revoke your own admin privileges")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	err = app.queries.RemoveUserAdmin(r.Context(), id)
	if err != nil {
		setFlash(w, "flash_error", "Failed to revoke admin")
	} else {
		setFlash(w, "flash_success", "Admin privileges revoked")
	}

	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}
