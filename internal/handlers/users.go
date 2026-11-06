package handlers

import (
	"database/sql"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"disjoint/internal/database"
	"disjoint/internal/thumbs"
)

// deleteUserConfirmPhrase is the literal text an admin must type to delete an
// account (checked server-side; the form's pattern= is only a convenience).
const deleteUserConfirmPhrase = "DELETE USER"

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

// DeleteUserSubmit permanently removes an account: the user row goes, which
// cascades their threads (and those threads' posts), their replies, reactions,
// reads, notifications and sessions. Their uploads — attachments, cached
// thumbnails, avatar, custom emoji — are unlinked from disk afterwards.
//
// Destructive and irreversible, so the form must echo the literal phrase
// "DELETE USER"; that check is enforced here, not just in the browser.
func (app *App) DeleteUserSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := requireAdmin(w, r)
	if user == nil {
		return
	}

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		setFlash(w, r, "flash_error", "Invalid user ID")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	if id == user.ID {
		setFlash(w, r, "flash_error", "You cannot delete your own account")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	if strings.TrimSpace(r.FormValue("confirm")) != deleteUserConfirmPhrase {
		setFlash(w, r, "flash_error", "Type DELETE USER to confirm deleting an account")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	target, err := app.queries.GetUserByID(ctx, id)
	if err != nil {
		setFlash(w, r, "flash_error", "User not found")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	if target.IsAdmin == 1 {
		setFlash(w, r, "flash_error", "Admins cannot be deleted. Revoke admin first")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	// Collect the file paths before the rows vanish. Attachments covers the
	// user's own posts and every post in threads they started, because those
	// threads (and all their replies) are deleted with the account.
	attachments, err := app.queries.ListAttachmentsByUser(ctx, database.ListAttachmentsByUserParams{
		UserID:   id,
		UserID_2: id,
	})
	if err != nil {
		setFlash(w, r, "flash_error", "Failed to load the account's files")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	emojis, err := app.queries.ListCustomEmojisByCreator(ctx, sql.NullInt64{Int64: id, Valid: true})
	if err != nil {
		setFlash(w, r, "flash_error", "Failed to load the account's emoji")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	// emojis.created_by is ON DELETE SET NULL, so the rows must go explicitly
	// or they would survive as orphaned custom emoji with no owner.
	if err := app.queries.DeleteCustomEmojisByCreator(ctx, sql.NullInt64{Int64: id, Valid: true}); err != nil {
		setFlash(w, r, "flash_error", "Failed to delete the account's emoji")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	if err := app.queries.DeleteUser(ctx, id); err != nil {
		setFlash(w, r, "flash_error", "Failed to delete user")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	app.removeAttachmentFiles(attachments)
	removeAvatarFile(app.uploadDir, target.AvatarPath)
	for _, e := range emojis {
		if !e.StoragePath.Valid || e.StoragePath.String == "" {
			continue
		}
		name := filepath.Base(filepath.Clean(e.StoragePath.String))
		if name == "." || name == ".." || name == "" {
			continue
		}
		fullPath := filepath.Join(app.uploadDir, name)
		if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[handlers] Warning: could not delete emoji file %s: %v", fullPath, err)
		}
		thumbs.Remove(app.uploadDir, e.StoragePath.String)
	}

	audit(r, user.ID, "delete_user", id)
	setFlash(w, r, "flash_success", "User deleted")
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}
