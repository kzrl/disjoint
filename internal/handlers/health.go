package handlers

import (
	"context"
	"io"
	"net/http"
	"time"
)

// Healthz is the public liveness/readiness probe. It bypasses auth (see
// middleware.FirstRunAndAuth) so Docker HEALTHCHECK and external monitors can
// reach it before an admin account exists. The database ping is the only
// dependency worth reporting: templates/assets are embedded and the worker is
// best-effort by design.
func (app *App) Healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := app.db.PingContext(ctx); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// Favicon is the public /favicon.ico endpoint. It serves the embedded 32x32
// PNG (browsers accept an image/png payload for this path) and never falls
// through to the authenticated front page, which would leak the whole index to
// unauthenticated visitors. The richer icon set lives in the <head> links
// (/static/favicon.svg, apple-touch-icon.png) and /static/site.webmanifest.
func (app *App) Favicon(w http.ResponseWriter, r *http.Request) {
	f, err := app.staticFS.Open("favicon-32.png")
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = io.Copy(w, f)
}
