package handlers

import (
	"fmt"
	"net/http"
)

// contentLimiter allows 30 posts/threads per user per 5 minutes. The window
// is intentionally generous: normal conversation never approaches it, while
// scripted spam and accidental double-submits are bounded. Reads are not
// limited here; the auth endpoints have their own per-IP limiter.
func (app *App) allowContent(userID int64) bool {
	return app.contentLimiter.Allow(fmt.Sprintf("user:%d", userID))
}

// rejectContentRateLimit sends the viewer back with a visible flash. For HTMX
// replies, HX-Redirect is what makes the limit visible at all: a bare 4xx is
// not swapped, so without it the click would appear to do nothing.
func (app *App) rejectContentRateLimit(w http.ResponseWriter, r *http.Request, redirect string) {
	setFlash(w, "flash_error", "You are posting too fast. Try again in a few minutes.")
	w.Header().Set("Retry-After", "300")

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", redirect)
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}
