package handlers

import (
	"database/sql"
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

	// Parse multipart form (maxUploadSize of in-memory form data)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
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

	// Post and attachments commit together so a failed attachment insert
	// does not leave a post referencing missing rows.
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "failed to create post", http.StatusInternalServerError)
		return
	}
	qtx := app.queries.WithTx(tx)

	// 1. Insert post (always attributed to the signed-in user).
	post, err := qtx.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID,
		UserID:   user.ID,
		Body:     body,
	})

	// 2. Insert attachments
	var attachments []database.Attachment
	if err == nil {
		for _, f := range uploadedFiles {
			att, attErr := qtx.CreateAttachment(ctx, database.CreateAttachmentParams{
				PostID:      post.ID,
				Filename:    f.OriginalName,
				StoragePath: f.StoragePath,
				MimeType:    f.MimeType,
				FileSize:    f.FileSize,
			})
			if attErr != nil {
				err = attErr
				break
			}
			attachments = append(attachments, att)
		}
	}

	if err != nil {
		tx.Rollback()
		http.Error(w, "failed to create post", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "failed to create post", http.StatusInternalServerError)
		return
	}

	// 3. Bump the thread: advance updated_at and, for TTL threads, push the
	// expiry deadline out to last-comment + ttl (never-threads stay NULL).
	var newExpiry sql.NullTime
	if thread.TtlSeconds.Valid {
		newExpiry = sql.NullTime{
			Time:  time.Now().UTC().Add(time.Duration(thread.TtlSeconds.Int64) * time.Second),
			Valid: true,
		}
	}
	_ = app.queries.BumpThreadExpiration(ctx, database.BumpThreadExpirationParams{
		ExpiresAt: newExpiry,
		ID:        thread.ID,
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
