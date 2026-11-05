package middleware

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	queries  *database.Queries
	hasUsers atomic.Bool
}

// New creates a new Middleware instance.
func New(queries *database.Queries) *Middleware {
	m := &Middleware{queries: queries}
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

// SecurityHeaders sets baseline defensive headers on every response:
//   - X-Frame-Options / CSP frame-ancestors: the app must never be framed
//     (clickjacking — it has state-changing forms).
//   - X-Content-Type-Options: no MIME sniffing (defense-in-depth for the
//     upload handler's forced Content-Disposition).
//   - Referrer-Policy: don't leak URLs (invite codes, user names) to
//     third-party sites.
//
// A full CSP is intentionally NOT set: HTMX's hx-on::* attributes are
// inline handlers that script-src 'self' would break.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// CSRFProtect is a defense-in-depth same-origin check for state-changing
// requests. Session cookies are already SameSite=Lax and every mutation is a
// POST, but an Origin/Sec-Fetch-Site check stops forged form posts from
// same-site sibling origins and from browsers that ignore SameSite.
//
// Scheme is deliberately ignored when comparing Origin with Host: TLS is
// commonly terminated by a reverse proxy, so the browser sees https while the
// Go process sees http. The hostname is the security-relevant part.
func CSRFProtect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
			next.ServeHTTP(w, r)
			return
		}

		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Hostname() == "" || !strings.EqualFold(u.Hostname(), requestHostname(r.Host)) {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
		}

		if site := r.Header.Get("Sec-Fetch-Site"); site != "" &&
			site != "same-origin" && site != "none" {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// requestHostname strips a port and IPv6 brackets so it can be compared with
// url.URL.Hostname from an Origin header.
func requestHostname(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(host)
	}
	return strings.ToLower(strings.Trim(hostport, "[]"))
}

// MaxRequestBodyBytes bounds any single HTTP request body. It is a coarse
// safety net (the per-file and per-post upload limits are enforced by the
// upload handler): without it, a client could stream unbounded data through
// multipart parsing, which spills to temporary files on disk.
const MaxRequestBodyBytes int64 = 128 << 20

// MaxBody rejects requests whose declared Content-Length exceeds the global
// request cap and wraps the body so a chunked/lying client is stopped at the
// same limit. Handlers still see read errors and report them as bad input.
func MaxBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > MaxRequestBodyBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// RateLimiter is a fixed-window per-IP request limiter, intended for
// authentication endpoints (brute-force bounding). It is in-memory, which
// is correct for a single-process deployment; with multiple instances it
// would need a shared store.
//
// The key is r.RemoteAddr (port stripped): behind a reverse proxy that is
// the proxy's address, so deploy single-proxy or terminate auth elsewhere.
type RateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	// hits maps client address to its current window. Windows are rotated
	// lazily and stale entries are swept when the map grows, so it stays
	// bounded by recent activity, not by history.
	hits map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

// NewRateLimiter allows at most limit requests per window per IP.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{limit: limit, window: window, hits: make(map[string]*rateWindow)}
}

// Allow reports whether a request from ip may proceed within the window.
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	if w, ok := rl.hits[ip]; ok && now.Sub(w.start) < rl.window {
		w.count++
		return w.count <= rl.limit
	}
	if len(rl.hits) > 1024 {
		for k, w := range rl.hits {
			if now.Sub(w.start) >= rl.window {
				delete(rl.hits, k)
			}
		}
	}
	rl.hits[ip] = &rateWindow{start: now, count: 1}
	return true
}

// Handler wraps next with the limiter, answering 429 when the client's
// window budget is exhausted.
func (rl *RateLimiter) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow(clientIP(r)) {
			w.Header().Set("Retry-After", "900")
			http.Error(w, "too many requests, slow down", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP extracts the client address for rate limiting. Directly it is
// RemoteAddr without the port. When the request arrives from loopback —
// i.e. via a local reverse proxy such as Caddy — the X-Real-IP header the
// proxy sets (Caddy sets it by default on proxied requests) is used
// instead. The header is only honored from loopback so a directly exposed
// app cannot be made to rate-limit on attacker-chosen keys.
func clientIP(r *http.Request) string {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	parsed, err := netip.ParseAddr(ip)
	if err == nil && parsed.IsLoopback() {
		if xri := r.Header.Get("X-Real-IP"); xri != "" {
			if real, err := netip.ParseAddr(xri); err == nil {
				return real.String()
			}
		}
	}
	return ip
}

// FirstRunAndAuth is a combined middleware that handles first-run setup check and authentication.
func (m *Middleware) FirstRunAndAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Static assets are always allowed. /healthz must also bypass auth
		// and the first-run redirect so container probes and external
		// monitors can check liveness before an admin account exists.
		if strings.HasPrefix(path, "/static/") || path == "/favicon.ico" || path == "/healthz" {
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

		// 2. Identify user from the session cookie. The cookie holds a
		// random token; its hash is looked up in the sessions table (the
		// expiry check happens in SQL). A missing or expired session just
		// means the request proceeds anonymously.
		var currentUser *database.User
		if token := auth.GetSessionTokenFromRequest(r); token != "" {
			if userID, err := m.queries.GetSessionUserID(ctx, database.GetSessionUserIDParams{
				TokenHash: auth.TokenHash(token),
				ExpiresAt: time.Now().Unix(),
			}); err == nil {
				if user, err := m.queries.GetUserByID(ctx, userID); err == nil {
					currentUser = &user
					r = r.WithContext(ContextWithUser(r.Context(), currentUser))
				}
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
