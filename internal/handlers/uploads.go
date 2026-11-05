package handlers

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const (
	// maxUploadSize caps a single uploaded file at 50MB.
	maxUploadSize = 50 << 20
	// maxUploadFilesPerPost bounds how many attachments one post/thread may
	// carry. Without this, a single request could stream an unlimited number
	// of 50MB files (and exhaust the upload volume). It is above the front
	// page's 8-image strip so the "+N" overflow remains reachable.
	maxUploadFilesPerPost = 16
	// maxUploadTotalSize bounds the combined size of one post's attachments.
	// It is much lower than files × per-file cap so a single post cannot fill
	// a small self-hosted disk with sixteen 50MB videos.
	maxUploadTotalSize = 100 << 20
	// maxUserStorageBytes bounds each user's stored attachments plus custom
	// emoji. It is a policy limit, not a technical one; raise it if the group
	// wants more room.
	maxUserStorageBytes = 2 << 30 // 2 GiB
)

// allowedMIMEExtensions maps a SNIFFED content type to the extension used
// for the stored file. The browser-provided Content-Type header and the
// user-chosen filename extension are both untrusted: HTML/SVG/scripts have
// no magic bytes and sniff as text/plain, so a user-named "evil.html" would
// otherwise be stored and served as a page. Deriving the stored extension
// from the sniffed type (like the avatar path) guarantees the served file
// can never be a type its content isn't.
var allowedMIMEExtensions = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"image/gif":       ".gif",
	"video/mp4":       ".mp4",
	"video/webm":      ".webm",
	"application/pdf": ".pdf",
	"text/plain":      ".txt",
}

// inlineExtensions are served inline (embedded in <img>/<video>); every other
// stored file is force-downloaded as a defense-in-depth measure.
var inlineExtensions = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
	".gif": true, ".mp4": true, ".webm": true,
}

// UploadedFileInfo contains metadata about an uploaded and saved file.
type UploadedFileInfo struct {
	OriginalName string
	StoragePath  string
	MimeType     string
	FileSize     int64
}

// removeUploadedFiles cleans up files written for a post that never made it
// into the database (validation failure, transaction rollback, commit error).
// Best-effort: an orphaned file is preferable to failing the request twice.
func removeUploadedFiles(uploadDir string, files []UploadedFileInfo) {
	for _, f := range files {
		name := filepath.Base(f.StoragePath)
		if name == "." || name == ".." || name == "" {
			continue
		}
		_ = os.Remove(filepath.Join(uploadDir, name))
	}
}

// uploadFieldBytes totals the declared sizes of one multipart file field.
func uploadFieldBytes(r *http.Request, field string) int64 {
	if r.MultipartForm == nil {
		return 0
	}
	var total int64
	for _, header := range r.MultipartForm.File[field] {
		if header.Size > 0 {
			total += header.Size
		}
	}
	return total
}

// checkStorageQuota rejects an upload that would put the user over their
// stored-bytes budget. It runs before files are written; concurrent uploads
// can race past it by one request, which is acceptable for a friend-group
// quota.
func (app *App) checkStorageQuota(ctx context.Context, userID, incoming int64) error {
	if incoming <= 0 {
		return nil
	}
	current, err := app.queries.SumUserStorageBytes(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to check storage quota")
	}
	if current+incoming > maxUserStorageBytes {
		return fmt.Errorf("storage quota exceeded (max %d GB per user)", maxUserStorageBytes>>30)
	}
	return nil
}

// saveUploadedFiles validates and saves uploaded files into uploadDir.
func saveUploadedFiles(r *http.Request, uploadDir string) ([]UploadedFileInfo, error) {
	if r.MultipartForm == nil || r.MultipartForm.File == nil {
		return nil, nil
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		return nil, nil
	}

	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create upload directory: %w", err)
	}

	var saved []UploadedFileInfo
	var totalSize int64

	for _, fileHeader := range files {
		if fileHeader.Size <= 0 {
			continue
		}
		if len(saved) >= maxUploadFilesPerPost {
			removeUploadedFiles(uploadDir, saved)
			return nil, fmt.Errorf("a post can include at most %d attachments", maxUploadFilesPerPost)
		}
		if fileHeader.Size > maxUploadSize {
			removeUploadedFiles(uploadDir, saved)
			return nil, fmt.Errorf("file %q exceeds maximum allowed size of 50MB", fileHeader.Filename)
		}
		totalSize += fileHeader.Size
		if totalSize > maxUploadTotalSize {
			removeUploadedFiles(uploadDir, saved)
			return nil, fmt.Errorf("attachments exceed the maximum combined size of 100MB per post")
		}

		info, err := saveSingleFile(fileHeader, uploadDir)
		if err != nil {
			// Clean up previously saved files on error
			removeUploadedFiles(uploadDir, saved)
			return nil, err
		}
		saved = append(saved, info)
	}

	return saved, nil
}

func saveSingleFile(header *multipart.FileHeader, uploadDir string) (UploadedFileInfo, error) {
	src, err := header.Open()
	if err != nil {
		return UploadedFileInfo{}, err
	}
	defer src.Close()

	// Sniff the actual content; the browser-provided Content-Type is
	// untrusted. The sniffed type must be on the allowlist.
	buf := make([]byte, 512)
	n, err := src.Read(buf)
	if err != nil && err != io.EOF {
		return UploadedFileInfo{}, fmt.Errorf("failed to read %q: %w", header.Filename, err)
	}
	if n == 0 {
		return UploadedFileInfo{}, fmt.Errorf("cannot determine content type of empty file %q", header.Filename)
	}
	mimeType := http.DetectContentType(buf[:n])
	if idx := strings.Index(mimeType, ";"); idx != -1 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}
	// DetectContentType only recognizes WebP when a VP* chunk leads the
	// file; an ICCP-led WebP sniffs as octet-stream, so check the RIFF/WEBP
	// magic bytes explicitly - the same fallback the avatar upload path
	// applies.
	if mimeType == "application/octet-stream" && isWebP(buf[:n]) {
		mimeType = "image/webp"
	}
	storedExt, ok := allowedMIMEExtensions[mimeType]
	if !ok {
		return UploadedFileInfo{}, fmt.Errorf("file %q: unsupported type %s", header.Filename, mimeType)
	}

	// Rewind reader
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return UploadedFileInfo{}, err
	}

	uniqueName := fmt.Sprintf("%s%s", uuid.New().String(), storedExt)
	targetPath := filepath.Join(uploadDir, uniqueName)

	dst, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return UploadedFileInfo{}, err
	}
	defer dst.Close()

	written, err := io.Copy(dst, src)
	if err != nil {
		_ = os.Remove(targetPath)
		return UploadedFileInfo{}, err
	}

	return UploadedFileInfo{
		OriginalName: header.Filename,
		StoragePath:  uniqueName,
		MimeType:     mimeType,
		FileSize:     written,
	}, nil
}

// ServeUpload serves uploaded media files with proper headers and cache settings.
func (app *App) ServeUpload(w http.ResponseWriter, r *http.Request) {
	filename := r.PathValue("filename")
	if filename == "" {
		http.NotFound(w, r)
		return
	}

	// Prevent directory traversal and require the route value to be exactly
	// one file name (the same identity check ServeThumb uses).
	cleanName := filepath.Base(filepath.Clean(filename))
	if cleanName == "" || cleanName == "." || cleanName == ".." || cleanName != filename {
		http.NotFound(w, r)
		return
	}
	filePath := filepath.Join(app.uploadDir, cleanName)

	stat, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if stat.IsDir() {
		http.NotFound(w, r)
		return
	}

	file, err := os.Open(filePath)
	if err != nil {
		http.Error(w, "cannot read file", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	// Media embeds inline in <img>/<video>; anything else is force-
	// downloaded so a non-media file can never render as a page.
	if inlineExtensions[strings.ToLower(filepath.Ext(cleanName))] {
		w.Header().Set("Content-Disposition", "inline")
	} else {
		w.Header().Set("Content-Disposition", "attachment")
	}

	// Cache control for static media
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, cleanName, stat.ModTime(), file)
}
