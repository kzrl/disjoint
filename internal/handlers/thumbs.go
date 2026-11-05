package handlers

import (
	"net/http"
	"path/filepath"
	"strconv"

	"disjoint/internal/middleware"
	"disjoint/internal/thumbs"
)

// ServeThumb serves a cached, downscaled derivative of an image attachment,
// generating it on first request.
//
// ?w= is a whitelist (320/640/1280, default 640): every width is a decode job
// and a cache key, so an unknown value is a 404 rather than a silent clamp.
// Anything that cannot be thumbnailed — GIFs (animation is preserved),
// sources already narrower than the request, undecodable or oversized files —
// is served as the original by thumbs.Ensure: a heavy image beats a broken one.
func (app *App) ServeThumb(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user != nil && !allowUser(app.thumbLimiter, user.ID) {
		w.Header().Set("Retry-After", "300")
		http.Error(w, "too many thumbnail requests", http.StatusTooManyRequests)
		return
	}

	raw := r.PathValue("filename")
	name := filepath.Base(filepath.Clean(raw))
	if name == "" || name == "." || name == ".." || name != raw {
		http.NotFound(w, r)
		return
	}
	if !thumbs.IsImage(name) {
		http.NotFound(w, r)
		return
	}

	width := thumbs.DefaultWidth
	if q := r.URL.Query().Get("w"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || !thumbs.AllowedWidth(n) {
			http.NotFound(w, r)
			return
		}
		width = n
	}

	path, derivative := thumbs.Ensure(app.uploadDir, name, width)
	if derivative {
		// The cache key includes the source UUID, the width, and the renderer
		// version, so a derivative's bytes can never change.
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=86400")
	}
	http.ServeFile(w, r, path)
}
