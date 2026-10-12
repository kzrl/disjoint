package handlers

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// maxUploadSize caps a single uploaded file (and the in-memory multipart
// budget) at 50MB.
const maxUploadSize = 50 << 20

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

// saveUploadedFiles validates and saves uploaded files into uploadDir.
func saveUploadedFiles(r *http.Request, uploadDir string) ([]UploadedFileInfo, error) {
	if r.MultipartForm == nil || r.MultipartForm.File == nil {
		return nil, nil
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		return nil, nil
	}

	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create upload directory: %w", err)
	}

	var saved []UploadedFileInfo

	for _, fileHeader := range files {
		if fileHeader.Size == 0 {
			continue
		}
		if fileHeader.Size > maxUploadSize {
			return nil, fmt.Errorf("file %q exceeds maximum allowed size of 50MB", fileHeader.Filename)
		}

		info, err := saveSingleFile(fileHeader, uploadDir)
		if err != nil {
			// Clean up previously saved files on error
			for _, s := range saved {
				_ = os.Remove(filepath.Join(uploadDir, s.StoragePath))
			}
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

	dst, err := os.Create(targetPath)
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

	// Prevent directory traversal
	cleanName := filepath.Base(filepath.Clean(filename))
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
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, cleanName, stat.ModTime(), file)
}
