package middleware

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"

	"disjoint/internal/auth"
	"disjoint/internal/database"
)

type contextKey string

const (
	UserKey contextKey = "current_user"
)

// ContextWithUser returns a new context with the given user.
func ContextWithUser(ctx context.Context, user *database.User) context.Context {
	return context.WithValue(ctx, UserKey, user)
}

// UserFromContext retrieves the user from context if available.
func UserFromContext(ctx context.Context) *database.User {
	if u, ok := ctx.Value(UserKey).(*database.User); ok {
		return u
	}
	return nil
}

// Middleware holds dependencies for HTTP middleware.
type Middleware struct {
	queries       *database.Queries
	sessionSecret string
	hasUsers      atomic.Bool
}

// New creates a new Middleware instance.
func New(queries *database.Queries, sessionSecret string) *Middleware {
	m := &Middleware{
		queries:       queries,
		sessionSecret: sessionSecret,
	}
	// Initial check if users exist
	if count, err := queries.CountUsers(context.Background()); err == nil && count > 0 {
		m.hasUsers.Store(true)
	}
	return m
}

// RefreshUserCount updates the internal flag indicating users exist.
func (m *Middleware) RefreshUserCount(ctx context.Context) {
	if count, err := m.queries.CountUsers(ctx); err == nil && count > 0 {
		m.hasUsers.Store(true)
	} else {
		m.hasUsers.Store(false)
	}
}

// FirstRunAndAuth is a combined middleware that handles first-run setup check and authentication.
func (m *Middleware) FirstRunAndAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Static assets are always allowed
		if strings.HasPrefix(path, "/static/") || path == "/favicon.ico" {
			next.ServeHTTP(w, r)
			return
		}

		ctx := r.Context()

		// 1. First-Run check
		if !m.hasUsers.Load() {
			count, err := m.queries.CountUsers(ctx)
			if err == nil && count > 0 {
				m.hasUsers.Store(true)
			} else {
				if path != "/setup" {
					http.Redirect(w, r, "/setup", http.StatusSeeOther)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
		}

		// If users already exist and request is /setup, redirect to login
		if path == "/setup" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// 2. Identify user from session cookie
		var currentUser *database.User
		userID, err := auth.GetUserIDFromRequest(r, m.sessionSecret)
		if err == nil {
			if user, err := m.queries.GetUserByID(ctx, userID); err == nil {
				currentUser = &user
				r = r.WithContext(ContextWithUser(r.Context(), currentUser))
			}
		}

		// Presence: best-effort "last seen" update. The upsert is throttled
		// in SQL (at most one write per 30s), and presence must never block
		// or fail the request, so errors are dropped.
		if currentUser != nil {
			_ = m.queries.UpsertUserPresence(ctx, currentUser.ID)
		}

		// 3. Access Control
		isPublicRoute := path == "/login" || path == "/register"

		if currentUser == nil {
			if isPublicRoute {
				next.ServeHTTP(w, r)
				return
			}

			// HTMX request: instruct HTMX to redirect to /login
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// If logged in and visiting login/register, redirect to home
		if isPublicRoute {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		next.ServeHTTP(w, r)
	})
}
