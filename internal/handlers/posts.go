package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"disjoint/internal/database"
	"disjoint/internal/middleware"
)

const maxPostURLLength = 2048

// cleanPostURL trims and validates the optional first-class post link.
// Empty is fine (text/attachment post). Otherwise it must be an absolute
// http(s) URL with a host. The scheme check is the security boundary:
// html/template escapes the href value but would happily pass a
// javascript: URL through, so non-http(s) schemes are refused here.
func cleanPostURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if len(s) > maxPostURLLength {
		return "", fmt.Errorf("link is too long (max %d characters)", maxPostURLLength)
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("link must be a valid http(s) URL")
	}
	return s, nil
}

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

	// Save attachments
	uploadedFiles, err := saveUploadedFiles(r, app.uploadDir)
	if err != nil {
		http.Error(w, "failed to upload files: "+err.Error(), http.StatusBadRequest)
		return
	}

	// A post needs a body OR at least one attachment OR a first-class
	// link, so image-only replies (e.g. a screenshot pasted from the
	// clipboard) and link-only replies are both allowed.
	body := strings.TrimSpace(r.FormValue("body"))
	postURL, err := cleanPostURL(r.FormValue("url"))
	if err != nil {
		removeUploadedFiles(app.uploadDir, uploadedFiles)
		http.Error(w, "invalid link: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body == "" && len(uploadedFiles) == 0 && postURL == "" {
		removeUploadedFiles(app.uploadDir, uploadedFiles)
		http.Error(w, "reply needs a body, a link, or at least one attachment", http.StatusBadRequest)
		return
	}

	// Post and attachments commit together so a failed attachment insert
	// does not leave a post referencing missing rows.
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		removeUploadedFiles(app.uploadDir, uploadedFiles)
		http.Error(w, "failed to create post", http.StatusInternalServerError)
		return
	}
	qtx := app.queries.WithTx(tx)

	// 1. Insert post (always attributed to the signed-in user).
	post, err := qtx.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID,
		UserID:   user.ID,
		Body:     body,
		Url:      sql.NullString{String: postURL, Valid: postURL != ""},
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

	// 3. Inbox entries for mentioned users and for the thread's participants.
	if err == nil {
		err = app.notifyPostRecipients(ctx, qtx, thread.ID, post.ID, user.ID, body)
	}

	if err != nil {
		tx.Rollback()
		removeUploadedFiles(app.uploadDir, uploadedFiles)
		http.Error(w, "failed to create post", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		removeUploadedFiles(app.uploadDir, uploadedFiles)
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

		// A freshly created post has no reactions yet, but its bar still
		// carries the full emoji list so its picker works.
		newPostEmojis, err := app.queries.ListAllEmojis(ctx)
		if err != nil {
			http.Error(w, "failed to load emojis", http.StatusInternalServerError)
			return
		}

		viewItem := SinglePostViewItem{
			Post:        postWithAuthor,
			Attachments: attachments,
			Emojis:      newPostEmojis,
			CanEdit:     true, // the viewer just created this post
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

// EditPostPage renders the edit form (body + first-class link) for a post.
// Author only: a non-author — or an unknown/expired post — is a plain 404.
func (app *App) EditPostPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	postID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	post, err := app.queries.GetPostByID(ctx, postID)
	if err != nil || post.UserID != user.ID {
		http.NotFound(w, r)
		return
	}

	thread, err := app.queries.GetThreadByID(ctx, post.ThreadID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	app.render(w, r, "edit_post.html", map[string]any{
		"Post":   post,
		"Thread": thread,
	}, "Edit Post")
}

// UpdatePostSubmit handles the author editing a post's body and first-class
// link. Attachments, expiration, and anonymity are immutable on edit.
// Ownership is enforced in the UPDATE's WHERE clause; a Go-side check right
// after loading the post puts the 404 ahead of any validation error so a
// non-author never learns whether a post exists.
func (app *App) UpdatePostSubmit(w http.ResponseWriter, r *http.Request) {
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

	postID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	post, err := app.queries.GetPostByID(ctx, postID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if post.UserID != user.ID {
		http.NotFound(w, r)
		return
	}

	// Expired threads are not editable (same rule as creation).
	thread, err := app.queries.GetThreadByID(ctx, post.ThreadID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if thread.ExpiresAt.Valid && thread.ExpiresAt.Time.Before(time.Now().UTC()) {
		http.Error(w, "this thread has expired", http.StatusGone)
		return
	}

	body := strings.TrimSpace(r.FormValue("body"))
	postURL, err := cleanPostURL(r.FormValue("url"))
	if err != nil {
		http.Error(w, "invalid link: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Content rule: a body OR a link is required unless the post carries
	// attachments (which are immutable on edit and still satisfy the rule).
	if body == "" && postURL == "" {
		n, countErr := app.queries.CountAttachmentsByPostID(ctx, postID)
		if countErr != nil {
			http.Error(w, "failed to check attachments", http.StatusInternalServerError)
			return
		}
		if n == 0 {
			http.Error(w, "post needs a body, a link, or at least one attachment", http.StatusBadRequest)
			return
		}
	}

	// No-op save: leave updated_at alone so the "(edited)" marker doesn't
	// appear on a resubmitted, unchanged form.
	if body == post.Body && postURL == post.Url {
		setFlash(w, "flash_success", "No changes to save.")
		http.Redirect(w, r, fmt.Sprintf("/threads/%d#post-%d", post.ThreadID, post.ID), http.StatusSeeOther)
		return
	}

	rows, err := app.queries.UpdatePost(ctx, database.UpdatePostParams{
		Body:   body,
		Url:    sql.NullString{String: postURL, Valid: postURL != ""},
		ID:     postID,
		UserID: user.ID,
	})
	if err != nil {
		http.Error(w, "failed to update post", http.StatusInternalServerError)
		return
	}
	if rows == 0 {
		http.NotFound(w, r)
		return
	}

	setFlash(w, "flash_success", "Post updated.")
	http.Redirect(w, r, fmt.Sprintf("/threads/%d#post-%d", post.ThreadID, post.ID), http.StatusSeeOther)
}
