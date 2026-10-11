package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"disjoint/internal/database"
	"disjoint/internal/middleware"
	"github.com/google/uuid"
)

const (
	maxBioLength  = 500
	maxAvatarSize = 5 * 1024 * 1024 // 5MB
)

// allowedAvatarMIMETypes maps a sniffed content type to its file extension.
// Only the three avatar formats are accepted; other images (gif, svg, ...)
// and non-images are rejected regardless of the extension the browser claims.
var allowedAvatarMIMETypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
}

// UserCardPage renders a user's profile card: avatar, bio, join date, last
// seen, and attributed post count. Anonymous content is unlinked from its
// author by design, so it never contributes to the post count.
func (app *App) UserCardPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")

	cardUser, err := app.queries.GetUserByUsername(ctx, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var lastSeen *time.Time
	if ts, err := app.queries.GetUserPresence(ctx, cardUser.ID); err == nil {
		tsCopy := ts
		lastSeen = &tsCopy
	}

	postCount, err := app.queries.CountPostsByUserID(ctx, sql.NullInt64{Int64: cardUser.ID, Valid: true})
	if err != nil {
		http.Error(w, "failed to load user card", http.StatusInternalServerError)
		return
	}

	viewer := middleware.UserFromContext(ctx)
	app.render(w, r, "user_card.html", map[string]any{
		"CardUser":  cardUser,
		"LastSeen":  lastSeen,
		"PostCount": postCount,
		"IsSelf":    viewer != nil && viewer.ID == cardUser.ID,
	}, cardUser.Username)
}

// UpdateProfileSubmit saves the viewer's bio (markdown, max 500 chars) and
// optionally replaces or removes their avatar (JPG/PNG/WEBP, max 5MB).
func (app *App) UpdateProfileSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	viewer := middleware.UserFromContext(ctx)

	// Support both urlencoded (bio-only) and multipart (avatar upload) forms.
	if err := r.ParseMultipartForm(10 << 20); err != nil && err != http.ErrNotMultipart {
		setFlash(w, "flash_error", "Failed to parse form: "+err.Error())
		http.Redirect(w, r, "/users/"+url.PathEscape(viewer.Username), http.StatusSeeOther)
		return
	}

	bio := strings.TrimSpace(r.FormValue("bio"))
	if len(bio) > maxBioLength {
		setFlash(w, "flash_error", "Bio must be at most 500 characters.")
		http.Redirect(w, r, "/users/"+url.PathEscape(viewer.Username), http.StatusSeeOther)
		return
	}

	if err := app.processAvatarUpload(ctx, r, viewer); err != nil {
		setFlash(w, "flash_error", err.Error())
		http.Redirect(w, r, "/users/"+url.PathEscape(viewer.Username), http.StatusSeeOther)
		return
	}

	if err := app.queries.UpdateUserBio(ctx, database.UpdateUserBioParams{
		Bio: sql.NullString{String: bio, Valid: true},
		ID:  viewer.ID,
	}); err != nil {
		http.Error(w, "failed to save profile", http.StatusInternalServerError)
		return
	}

	setFlash(w, "flash_success", "Profile saved.")
	http.Redirect(w, r, "/users/"+url.PathEscape(viewer.Username), http.StatusSeeOther)
}

// processAvatarUpload handles the optional "avatar" file and "remove_avatar"
// checkbox from the profile form. The file is stored in the shared uploads
// directory and served via the existing /uploads/{filename} route.
func (app *App) processAvatarUpload(ctx context.Context, r *http.Request, user *database.User) error {
	// Removal takes precedence over a simultaneous (re)upload.
	if r.FormValue("remove_avatar") != "" {
		if err := app.queries.RemoveUserAvatar(ctx, user.ID); err != nil {
			return fmt.Errorf("failed to remove avatar")
		}
		removeAvatarFile(app.uploadDir, user.AvatarPath)
		return nil
	}

	file, header, err := r.FormFile("avatar")
	if err != nil {
		if err == http.ErrMissingFile || header == nil {
			return nil // No avatar field sent — nothing to do.
		}
		return fmt.Errorf("avatar upload failed")
	}
	defer file.Close()

	if header.Size == 0 {
		return nil
	}
	if header.Size > maxAvatarSize {
		return fmt.Errorf("avatar exceeds maximum allowed size of 5MB")
	}

	// Sniff the actual content instead of trusting the browser header,
	// so a renamed .txt/.gif cannot masquerade as an avatar.
	buf := make([]byte, 512)
	n, _ := file.Read(buf)
	contentType := http.DetectContentType(buf[:n])
	// DetectContentType has no WebP signature, so check the RIFF/WEBP
	// magic bytes explicitly.
	if contentType == "application/octet-stream" && isWebP(buf[:n]) {
		contentType = "image/webp"
	}
	ext, ok := allowedAvatarMIMETypes[contentType]
	if !ok {
		return fmt.Errorf("avatar must be a JPG, PNG, or WEBP image")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("avatar upload failed")
	}

	if err := os.MkdirAll(app.uploadDir, 0755); err != nil {
		return fmt.Errorf("failed to create upload directory")
	}

	storageName := fmt.Sprintf("avatar-%s%s", uuid.New().String(), ext)
	targetPath := filepath.Join(app.uploadDir, storageName)
	dst, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("failed to save avatar")
	}
	defer dst.Close()

	if _, err := io.Copy(dst, file); err != nil {
		_ = os.Remove(targetPath)
		return fmt.Errorf("failed to save avatar")
	}

	if err := app.queries.SetUserAvatar(ctx, database.SetUserAvatarParams{
		AvatarPath: sql.NullString{String: storageName, Valid: true},
		ID:         user.ID,
	}); err != nil {
		_ = os.Remove(targetPath)
		return fmt.Errorf("failed to save avatar")
	}

	// Replace: drop the previous avatar file if it differs.
	if user.AvatarPath.Valid && user.AvatarPath.String != storageName {
		removeAvatarFile(app.uploadDir, user.AvatarPath)
	}
	return nil
}

// isWebP checks the RIFF container magic bytes for a WebP payload.
func isWebP(b []byte) bool {
	return len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP"
}

// removeAvatarFile deletes an avatar from disk. The stored path is a bare
// filename, but Base() is applied defensively against anything unexpected.
func removeAvatarFile(uploadDir string, avatarPath sql.NullString) {
	if !avatarPath.Valid || avatarPath.String == "" {
		return
	}
	_ = os.Remove(filepath.Join(uploadDir, filepath.Base(avatarPath.String)))
}
