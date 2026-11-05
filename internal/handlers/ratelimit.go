package handlers

import (
	"fmt"
	"net/http"

	"disjoint/internal/middleware"
)

// allowUser keys a limiter on the authenticated user, not the IP, so a shared
// household cannot be throttled by one member and a spammer cannot rotate IPs
// to reset their budget.
func allowUser(limiter *middleware.RateLimiter, userID int64) bool {
	return limiter.Allow(fmt.Sprintf("user:%d", userID))
}

// allowContent allows 30 posts/threads per user per 5 minutes. Normal
// conversation never approaches it, while scripted spam and accidental
// double-submits are bounded.
func (app *App) allowContent(userID int64) bool {
	return allowUser(app.contentLimiter, userID)
}

// rejectRateLimit sends the viewer back with a visible flash. For HTMX
// requests, HX-Redirect is what makes the limit visible at all: a bare 4xx is
// not swapped, so without it the click would appear to do nothing.
func rejectRateLimit(w http.ResponseWriter, r *http.Request, redirect, message string) {
	setFlash(w, r, "flash_error", message)
	w.Header().Set("Retry-After", "300")

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", redirect)
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (app *App) rejectContentRateLimit(w http.ResponseWriter, r *http.Request, redirect string) {
	rejectRateLimit(w, r, redirect, "You are posting too fast. Try again in a few minutes.")
}
