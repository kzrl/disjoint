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
		setFlash(w, r, "flash_error", "Failed to load users")
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
		setFlash(w, r, "flash_error", "Invalid user ID")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	target, err := app.queries.GetUserByID(r.Context(), id)
	if err != nil {
		setFlash(w, r, "flash_error", "User not found")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	if target.IsDisabled == 1 {
		setFlash(w, r, "flash_error", "Enable the account before promoting it to admin")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	err = app.queries.SetUserAdmin(r.Context(), id)
	if err != nil {
		setFlash(w, r, "flash_error", "Failed to promote user")
	} else {
		audit(r, user.ID, "promote_user", id)
		setFlash(w, r, "flash_success", "User promoted to admin")
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
		setFlash(w, r, "flash_error", "Invalid user ID")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	if id == user.ID {
		setFlash(w, r, "flash_error", "You cannot revoke your own admin privileges")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	err = app.queries.RemoveUserAdmin(r.Context(), id)
	if err != nil {
		setFlash(w, r, "flash_error", "Failed to revoke admin")
	} else {
		audit(r, user.ID, "demote_user", id)
		setFlash(w, r, "flash_success", "Admin privileges revoked")
	}

	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// DisableUserSubmit suspends a non-admin account: it can no longer sign in and
// its live sessions are deleted. Content is untouched — this is suspension,
// not deletion.
func (app *App) DisableUserSubmit(w http.ResponseWriter, r *http.Request) {
	user := requireAdmin(w, r)
	if user == nil {
		return
	}

	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		setFlash(w, r, "flash_error", "Invalid user ID")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	if id == user.ID {
		setFlash(w, r, "flash_error", "You cannot disable your own account")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	// DisableUser is guarded with "is_admin = 0", so 0 rows means the target
	// is an administrator (demote first) or no longer exists.
	rows, err := app.queries.DisableUser(r.Context(), id)
	if err != nil {
		setFlash(w, r, "flash_error", "Failed to disable user")
	} else if rows == 0 {
		setFlash(w, r, "flash_error", "Only non-admin users can be disabled. Demote them first")
	} else {
		// Kill existing logins immediately: the middleware also rejects and
		// purges any session that slips through, but deleting them is what
		// makes the suspension take effect on the next request.
		_ = app.queries.DeleteSessionsForUser(r.Context(), id)
		audit(r, user.ID, "disable_user", id)
		setFlash(w, r, "flash_success", "User disabled")
	}

	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// EnableUserSubmit lifts a suspension. Enabling an already-enabled account is
// a harmless no-op.
func (app *App) EnableUserSubmit(w http.ResponseWriter, r *http.Request) {
	user := requireAdmin(w, r)
	if user == nil {
		return
	}

	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		setFlash(w, r, "flash_error", "Invalid user ID")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	err = app.queries.EnableUser(r.Context(), id)
	if err != nil {
		setFlash(w, r, "flash_error", "Failed to enable user")
	} else {
		audit(r, user.ID, "enable_user", id)
		setFlash(w, r, "flash_success", "User enabled")
	}

	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}
