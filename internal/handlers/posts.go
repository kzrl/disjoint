package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"disjoint/internal/database"
	"disjoint/internal/middleware"
)

// CreatePostSubmit handles creating a reply post, returning an HTML partial for HTMX or redirecting.
func (app *App) CreatePostSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	threadIDStr := r.PathValue("id")
	threadID, err := strconv.ParseInt(threadIDStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	thread, err := app.queries.GetThreadByID(ctx, threadID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Verify thread has not expired
	if thread.ExpiresAt.Valid && thread.ExpiresAt.Time.Before(time.Now().UTC()) {
		http.Error(w, "this thread has expired", http.StatusGone)
		return
	}

	// Parse multipart form up to 50MB
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		http.Error(w, "failed to parse multipart form: "+err.Error(), http.StatusBadRequest)
		return
	}

	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		http.Error(w, "reply body cannot be empty", http.StatusBadRequest)
		return
	}
	// Save attachments
	uploadedFiles, err := saveUploadedFiles(r, app.uploadDir)
	if err != nil {
		http.Error(w, "failed to upload files: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 1. Insert post (always attributed to the signed-in user).
	post, err := app.queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID,
		UserID:   user.ID,
		Body:     body,
	})
	if err != nil {
		http.Error(w, "failed to create post", http.StatusInternalServerError)
		return
	}

	// 2. Insert attachments
	var attachments []database.Attachment
	for _, f := range uploadedFiles {
		att, err := app.queries.CreateAttachment(ctx, database.CreateAttachmentParams{
			PostID:      post.ID,
			Filename:    f.OriginalName,
			StoragePath: f.StoragePath,
			MimeType:    f.MimeType,
			FileSize:    f.FileSize,
		})
		if err == nil {
			attachments = append(attachments, att)
		}
	}

	// 3. Touch thread to update updated_at timestamp
	_ = app.queries.TouchThread(ctx, thread.ID)

	// 4. Update user board read timestamp
	_ = app.queries.UpsertUserBoardRead(ctx, database.UpsertUserBoardReadParams{
		UserID:  user.ID,
		BoardID: thread.BoardID,
	})

	// If HTMX request, render and return the single post partial
	if r.Header.Get("HX-Request") == "true" {
		postWithAuthor, err := app.queries.GetPostByID(ctx, post.ID)
		if err != nil {
			http.Error(w, "failed to load created post", http.StatusInternalServerError)
			return
		}

		viewItem := SinglePostViewItem{
			Post:        postWithAuthor,
			Attachments: attachments,
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := app.postPartialTmpl.Execute(w, viewItem); err != nil {
			http.Error(w, fmt.Sprintf("template execution error: %v", err), http.StatusInternalServerError)
		}
		return
	}

	// Standard fallback: redirect to thread anchoring to newly created post
	http.Redirect(w, r, fmt.Sprintf("/threads/%d#post-%d", thread.ID, post.ID), http.StatusSeeOther)
}
