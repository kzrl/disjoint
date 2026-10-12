package handlers

import (
	"database/sql"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"disjoint/internal/database"
	"disjoint/internal/markdown"
	"disjoint/internal/middleware"
	"disjoint/web"
)

// App manages the application HTTP routes and dependencies.
type App struct {
	db              *sql.DB
	queries         *database.Queries
	uploadDir       string
	sessionSecret   string
	staticFS        fs.FS
	templates       map[string]*template.Template
	postPartialTmpl *template.Template
	middleware      *middleware.Middleware
}

// NewApp instantiates an App and compiles HTML templates.
func NewApp(db *sql.DB, queries *database.Queries, uploadDir, sessionSecret string) (*App, error) {
	app := &App{
		db:            db,
		queries:       queries,
		uploadDir:     uploadDir,
		sessionSecret: sessionSecret,
		templates:     make(map[string]*template.Template),
		middleware:    middleware.New(queries, sessionSecret),
	}

	staticFS, err := fs.Sub(web.Files, "static")
	if err != nil {
		return nil, fmt.Errorf("failed to open embedded static assets: %w", err)
	}
	app.staticFS = staticFS

	funcMap := template.FuncMap{
		"formatTime": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Format("Jan 02, 2006 15:04")
		},
		"formatRelative": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			d := time.Since(t)
			if d < time.Minute {
				return "just now"
			} else if d < time.Hour {
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			} else if d < 24*time.Hour {
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			} else if d < 7*24*time.Hour {
				return fmt.Sprintf("%dd ago", int(d.Hours()/24))
			}
			return t.Format("Jan 02, 2006")
		},
		"formatFileSize": func(b int64) string {
			const unit = 1024
			if b < unit {
				return fmt.Sprintf("%d B", b)
			}
			div, exp := int64(unit), 0
			for n := b / unit; n >= unit; n /= unit {
				div *= unit
				exp++
			}
			return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
		},
		"isImage": func(mimeType string) bool {
			return strings.HasPrefix(mimeType, "image/")
		},
		"isVideo": func(mimeType string) bool {
			return strings.HasPrefix(mimeType, "video/")
		},
		"formatExpiry": func(nt sql.NullTime) string {
			if !nt.Valid {
				return "Never"
			}
			d := time.Until(nt.Time)
			if d <= 0 {
				return "Expired"
			}
			if d < time.Hour {
				return fmt.Sprintf("in %dm", int(d.Minutes()))
			}
			if d < 24*time.Hour {
				return fmt.Sprintf("in %dh", int(d.Hours()))
			}
			return fmt.Sprintf("in %dd", int(d.Hours()/24))
		},
		"renderMarkdown": func(body string) template.HTML {
			return markdown.Render(body)
		},
		// userLink renders a username as a link to its user card
		// (empty name renders nothing). Every post and thread is
		// attributed to a real user (posts.user_id is NOT NULL), so there
		// is no anonymous case.
		"userLink": func(name string) template.HTML {
			if name == "" {
				return ""
			}
			return template.HTML(`<a href="/users/` + template.HTMLEscapeString(url.PathEscape(name)) + `">` + template.HTMLEscapeString(name) + `</a>`)
		},
		// authorAvatar renders a small round avatar image before an author
		// name, or nothing when the user has no avatar.
		"authorAvatar": func(path string) template.HTML {
			if path == "" {
				return ""
			}
			return template.HTML(`<img class="avatar avatar-sm" src="/uploads/` + template.HTMLEscapeString(url.PathEscape(path)) + `" alt="" loading="lazy">`)
		},
	}

	// Parse individual page templates combined with layout.html
	pages := []string{
		"index.html",
		"board.html",
		"thread.html",
		"new_thread.html",
		"login.html",
		"register.html",
		"register_closed.html",
		"invites.html",
		"users.html",
		"setup.html",
		"new_board.html",
		"user_card.html",
	}

	for _, page := range pages {
		tmpl, err := template.New("layout.html").Funcs(funcMap).ParseFS(web.Files, "templates/layout.html", "templates/"+page)
		if err != nil {
			return nil, fmt.Errorf("failed to parse template %s: %w", page, err)
		}
		app.templates[page] = tmpl
	}

	// Parse post partial for HTMX reply responses
	postPartialTmpl, err := template.New("post_partial.html").Funcs(funcMap).ParseFS(web.Files, "templates/post_partial.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse post_partial template: %w", err)
	}
	app.postPartialTmpl = postPartialTmpl

	return app, nil
}

// render renders a named page template.
func (app *App) render(w http.ResponseWriter, r *http.Request, page string, data any, pageTitle string) {
	tmpl, ok := app.templates[page]
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}

	user := middleware.UserFromContext(r.Context())

	// Read flash cookies if present
	var flashErr, flashSucc string
	if c, err := r.Cookie("flash_error"); err == nil && c.Value != "" {
		flashErr = c.Value
		http.SetCookie(w, &http.Cookie{Name: "flash_error", Value: "", Path: "/", MaxAge: -1})
	}
	if c, err := r.Cookie("flash_success"); err == nil && c.Value != "" {
		flashSucc = c.Value
		http.SetCookie(w, &http.Cookie{Name: "flash_success", Value: "", Path: "/", MaxAge: -1})
	}

	// If data is a map, inject CurrentUser and PageTitle into it for template access
	if m, ok := data.(map[string]any); ok {
		m["CurrentUser"] = user
		m["PageTitle"] = pageTitle
		m["FlashError"] = flashErr
		m["FlashSuccess"] = flashSucc
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "layout.html", data); err != nil {
		http.Error(w, fmt.Sprintf("template execution error: %v", err), http.StatusInternalServerError)
	}
}

// setFlash sets a session-lifetime flash cookie; render() clears it on the
// next page load, and browsers drop it on close.
func setFlash(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
	})
}

// Routes constructs the HTTP handler mux with all routes and middleware applied.
func (app *App) Routes() http.Handler {
	mux := http.NewServeMux()

	// Embedded static assets (/static/*)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(app.staticFS))))

	// Uploaded media files (/uploads/{filename})
	mux.HandleFunc("GET /uploads/{filename}", app.ServeUpload)

	// Auth & First-Run Setup
	mux.HandleFunc("GET /setup", app.SetupPage)
	mux.HandleFunc("POST /setup", app.SetupSubmit)
	mux.HandleFunc("GET /login", app.LoginPage)
	mux.HandleFunc("POST /login", app.LoginSubmit)
	mux.HandleFunc("GET /register", app.RegisterPage)
	mux.HandleFunc("POST /register", app.RegisterSubmit)
	mux.HandleFunc("POST /logout", app.LogoutSubmit)

	// Admin Invite Management
	mux.HandleFunc("GET /invites", app.InvitesPage)
	mux.HandleFunc("POST /invites", app.CreateInviteSubmit)
	mux.HandleFunc("POST /invites/{id}/delete", app.DeleteInviteSubmit)

	// Admin User Administration
	mux.HandleFunc("GET /admin/users", app.UsersPage)
	mux.HandleFunc("POST /admin/users/{id}/promote", app.PromoteUserSubmit)
	mux.HandleFunc("POST /admin/users/{id}/demote", app.DemoteUserSubmit)

	// Forum boards & categories
	mux.HandleFunc("GET /", app.IndexPage)
	mux.HandleFunc("GET /boards/new", app.NewBoardPage)
	mux.HandleFunc("POST /boards/new", app.CreateBoardSubmit)
	mux.HandleFunc("POST /categories/new", app.CreateCategorySubmit)
	mux.HandleFunc("GET /boards/{id}", app.BoardPage)

	// Threads & posts
	mux.HandleFunc("GET /boards/{id}/threads/new", app.NewThreadPage)
	mux.HandleFunc("POST /boards/{id}/threads/new", app.CreateThreadSubmit)
	mux.HandleFunc("GET /threads/{id}", app.ThreadPage)
	mux.HandleFunc("POST /threads/{id}/posts", app.CreatePostSubmit)

	// User cards & profiles
	mux.HandleFunc("GET /users/{name}", app.UserCardPage)
	mux.HandleFunc("POST /users/me/profile", app.UpdateProfileSubmit)

	return app.middleware.FirstRunAndAuth(mux)
}
