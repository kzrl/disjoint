package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	// Register PNG and GIF decoders for image.Decode.
	_ "image/gif"
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"disjoint/internal/database"
	"disjoint/internal/markdown"
	"disjoint/internal/middleware"

	"github.com/google/uuid"
)

// Custom emoji limits. Slack allows 128KB/1280px; emoji here render at ~1.25em
// in reaction chips, so we are much stricter: 256KB file, 256px max side.
// Only PNG and GIF: both are reliably sniffed by http.DetectContentType and
// decode with the stdlib (no new deps). WEBP is rejected because the stdlib
// cannot decode it (no image/webp), so we could not verify dimensions.
const (
	maxEmojiSize      = 256 << 10
	maxEmojiDimension = 256
)

// emojiNameRE matches Slack-style :name: tokens: lowercase letters, digits,
// underscores. The charset also guarantees the name is safe inside hrefs and
// HTML attributes without escaping.
var emojiNameRE = regexp.MustCompile(`^[a-z0-9_]{2,64}$`)

// emojiMIMEExtensions maps a SNIFFED content type to the stored extension.
var emojiMIMEExtensions = map[string]string{
	"image/png": ".png",
	"image/gif": ".gif",
}

// EmojiPage renders the community emoji page: every available emoji plus the
// upload form. Any signed-in user may browse and upload.
func (app *App) EmojiPage(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	emojis, err := app.queries.ListAllEmojis(r.Context())
	if err != nil {
		setFlash(w, r, "flash_error", "Failed to load emojis")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	app.render(w, r, "emojis.html", map[string]any{
		"Emojis": emojis,
	}, "Emoji")
}

// refreshEmojiCache publishes the current emoji set to the markdown renderer so
// :name: tokens in post bodies resolve immediately after an upload or delete.
// Best-effort: a failed load leaves the previous snapshot in place (the next
// write or process start heals it), and it must never fail the emoji write
// that triggered it.
func (app *App) refreshEmojiCache(ctx context.Context) {
	rows, err := app.queries.ListAllEmojis(ctx)
	if err != nil {
		return
	}
	out := make([]markdown.Emoji, 0, len(rows))
	for _, r := range rows {
		out = append(out, markdown.Emoji{
			Name:        r.Name,
			Glyph:       r.Glyph,
			StoragePath: r.StoragePath,
			Custom:      r.Source == "custom",
		})
	}
	markdown.SetEmojis(out)
}

// CreateEmojiSubmit handles uploading a new custom emoji.
func (app *App) CreateEmojiSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	fail := func(msg string) {
		setFlash(w, r, "flash_error", msg)
		http.Redirect(w, r, "/emojis", http.StatusSeeOther)
	}
	if !allowUser(app.uploadLimiter, user.ID) {
		fail("You are uploading too fast. Try again in a few minutes.")
		return
	}

	if err := r.ParseMultipartForm(maxEmojiSize); err != nil {
		fail("Failed to parse upload: " + err.Error())
		return
	}

	name := strings.ToLower(strings.TrimSpace(r.FormValue("name")))
	if !emojiNameRE.MatchString(name) {
		fail("Emoji name must be 2-64 lowercase letters, digits, or underscores (e.g. mario_fire).")
		return
	}

	switch _, err := app.queries.GetEmojiByName(ctx, name); {
	case err == nil:
		fail(fmt.Sprintf("An emoji named :%s: already exists.", name))
		return
	case !errors.Is(err, sql.ErrNoRows):
		fail("Failed to check emoji name.")
		return
	}

	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		fail("No image uploaded.")
		return
	}
	if err := app.checkStorageQuota(ctx, user.ID, files[0].Size); err != nil {
		fail(err.Error())
		return
	}
	info, err := saveEmojiFile(files[0], app.uploadDir)
	if err != nil {
		fail("Invalid emoji image: " + err.Error())
		return
	}

	created, err := app.queries.CreateEmoji(ctx, database.CreateEmojiParams{
		Name:        name,
		Source:      "custom",
		Glyph:       sql.NullString{},
		StoragePath: sql.NullString{String: info.StoragePath, Valid: true},
		MimeType:    sql.NullString{String: info.MimeType, Valid: true},
		FileSize:    sql.NullInt64{Int64: info.FileSize, Valid: true},
		CreatedBy:   sql.NullInt64{Int64: user.ID, Valid: true},
	})
	if err != nil {
		_ = os.Remove(filepath.Join(app.uploadDir, info.StoragePath))
		fail("Failed to save emoji.")
		return
	}

	app.refreshEmojiCache(ctx)
	audit(r, user.ID, "create_emoji", created.ID)

	setFlash(w, r, "flash_success", fmt.Sprintf("Emoji :%s: added.", created.Name))
	http.Redirect(w, r, "/emojis", http.StatusSeeOther)
}

// DeleteEmojiSubmit deletes a custom emoji (its uploader or an admin),
// removing its file and, via the FK cascade, every reaction that used it.
func (app *App) DeleteEmojiSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	fail := func(msg string) {
		setFlash(w, r, "flash_error", msg)
		http.Redirect(w, r, "/emojis", http.StatusSeeOther)
	}

	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		fail("Invalid emoji ID.")
		return
	}

	emoji, err := app.queries.GetEmojiByID(ctx, id)
	if err != nil {
		fail("Emoji not found.")
		return
	}
	if emoji.Source == "unicode" {
		fail("Built-in emoji cannot be deleted.")
		return
	}

	isOwner := emoji.CreatedBy.Valid && emoji.CreatedBy.Int64 == user.ID
	if user.IsAdmin != 1 && !isOwner {
		fail("You can only delete emoji you uploaded.")
		return
	}

	if emoji.StoragePath.Valid && emoji.StoragePath.String != "" {
		// Base() keeps a malicious stored path from escaping the upload dir.
		_ = os.Remove(filepath.Join(app.uploadDir, filepath.Base(emoji.StoragePath.String)))
	}

	if err := app.queries.DeleteEmoji(ctx, id); err != nil {
		fail("Failed to delete emoji.")
		return
	}
	app.refreshEmojiCache(ctx)
	audit(r, user.ID, "delete_emoji", id)

	setFlash(w, r, "flash_success", fmt.Sprintf("Emoji :%s: deleted.", emoji.Name))
	http.Redirect(w, r, "/emojis", http.StatusSeeOther)
}

// saveEmojiFile validates and saves one uploaded emoji image. The file is
// read fully into memory (capped at maxEmojiSize), its content sniffed, then
// decoded: the sniffed type must be on the allowlist, the image must actually
// decode, and its dimensions must fit the chip size.
func saveEmojiFile(header *multipart.FileHeader, uploadDir string) (UploadedFileInfo, error) {
	if header.Size > maxEmojiSize {
		return UploadedFileInfo{}, fmt.Errorf("exceeds maximum allowed size of %dKB", maxEmojiSize>>10)
	}

	src, err := header.Open()
	if err != nil {
		return UploadedFileInfo{}, err
	}
	defer src.Close()

	data, err := io.ReadAll(io.LimitReader(src, maxEmojiSize+1))
	if err != nil {
		return UploadedFileInfo{}, err
	}
	if len(data) == 0 {
		return UploadedFileInfo{}, errors.New("empty file")
	}

	mimeType := http.DetectContentType(data)
	if idx := strings.Index(mimeType, ";"); idx != -1 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}
	storedExt, ok := emojiMIMEExtensions[mimeType]
	if !ok {
		return UploadedFileInfo{}, fmt.Errorf("unsupported type %s (PNG or GIF only)", mimeType)
	}

	// Check the header before decoding pixels: a tiny compressed PNG/GIF can
	// declare enormous dimensions and otherwise allocate them in image.Decode.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return UploadedFileInfo{}, fmt.Errorf("not a decodable image: %v", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return UploadedFileInfo{}, errors.New("image has invalid dimensions")
	}
	if cfg.Width > maxEmojiDimension || cfg.Height > maxEmojiDimension {
		return UploadedFileInfo{}, fmt.Errorf("dimensions %dx%d exceed %dx%d", cfg.Width, cfg.Height, maxEmojiDimension, maxEmojiDimension)
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return UploadedFileInfo{}, fmt.Errorf("not a decodable image: %v", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() > maxEmojiDimension || bounds.Dy() > maxEmojiDimension {
		return UploadedFileInfo{}, fmt.Errorf("dimensions %dx%d exceed %dx%d", bounds.Dx(), bounds.Dy(), maxEmojiDimension, maxEmojiDimension)
	}

	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		return UploadedFileInfo{}, fmt.Errorf("failed to create upload directory: %w", err)
	}

	uniqueName := uuid.New().String() + storedExt
	if err := os.WriteFile(filepath.Join(uploadDir, uniqueName), data, 0o600); err != nil {
		return UploadedFileInfo{}, err
	}

	return UploadedFileInfo{
		OriginalName: header.Filename,
		StoragePath:  uniqueName,
		MimeType:     mimeType,
		FileSize:     int64(len(data)),
	}, nil
}
