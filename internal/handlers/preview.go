package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"disjoint/internal/middleware"
)

// maxPreviewRequestBytes bounds the urlencoded preview request. A 256 KB body
// can grow to roughly 3x under percent-encoding; 1 MiB leaves headroom without
// accepting an unbounded text payload.
const maxPreviewRequestBytes int64 = 1 << 20

// PreviewView is the data for the composer preview fragment. It deliberately
// mirrors the fields post_card.html reads from a post: Body, the optional
// first-class Url, and an inline Error for user-input problems.
type PreviewView struct {
	Body  string
	Url   string
	Error string
}

// PreviewSubmit renders a composer's current Markdown body and link card
// without persisting anything. It is a read-only endpoint: no database reads
// or writes, no attachment parsing, and no link unfurling.
//
// User-input problems (an invalid link, an oversized body, a malformed
// request) are returned as a 200 fragment with an inline message. HTMX does
// not swap 4xx responses, so a 400 here would leave the preview panel showing
// stale content.
func (app *App) PreviewSubmit(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if !allowUser(app.previewLimiter, user.ID) {
		w.Header().Set("Retry-After", "300")
		app.renderPreview(w, PreviewView{
			Error: "Preview is temporarily rate-limited. Try again in a few minutes.",
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPreviewRequestBytes)
	if err := r.ParseForm(); err != nil {
		app.renderPreview(w, PreviewView{
			Error: "Preview request is too large or malformed.",
		})
		return
	}

	view := PreviewView{}
	var problems []string

	body := strings.TrimSpace(r.FormValue("body"))
	if len(body) > maxPostBodyBytes {
		problems = append(problems, fmt.Sprintf("Post body is too long (max %d KB).", maxPostBodyBytes/1024))
	} else {
		view.Body = body
	}

	postURL, err := cleanPostURL(r.FormValue("url"))
	if err != nil {
		problems = append(problems, err.Error())
	} else {
		view.Url = postURL
	}

	view.Error = strings.Join(problems, " ")
	app.renderPreview(w, view)
}

func (app *App) renderPreview(w http.ResponseWriter, view PreviewView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := app.previewTmpl.ExecuteTemplate(w, "preview", view); err != nil {
		http.Error(w, "failed to render preview", http.StatusInternalServerError)
	}
}
