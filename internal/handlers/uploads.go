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

// Allowed MIME type prefixes or specific types
var allowedMIMETypes = map[string]bool{
	"image/jpeg":      true,
	"image/png":       true,
	"image/webp":      true,
	"image/gif":       true,
	"video/mp4":       true,
	"video/webm":      true,
	"application/pdf": true,
	"text/plain":      true,
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
		// Max file size: 50MB
		if fileHeader.Size > 50*1024*1024 {
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

	// Sniff MIME type
	buf := make([]byte, 512)
	n, _ := src.Read(buf)
	mimeType := http.DetectContentType(buf[:n])
	if headerMime := header.Header.Get("Content-Type"); headerMime != "" && headerMime != "application/octet-stream" {
		mimeType = headerMime
	}
	// Normalize mime type
	if idx := strings.Index(mimeType, ";"); idx != -1 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}

	// Rewind reader
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return UploadedFileInfo{}, err
	}

	ext := filepath.Ext(header.Filename)
	uniqueName := fmt.Sprintf("%s%s", uuid.New().String(), ext)
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

	// Cache control for static media
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, cleanName, stat.ModTime(), file)
}
