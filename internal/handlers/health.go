package handlers

import (
	"context"
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
