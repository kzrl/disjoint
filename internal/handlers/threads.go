package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"disjoint/internal/database"
	"disjoint/internal/middleware"
)

// PostViewItem holds post details and its associated attachments.
type PostViewItem struct {
	Post        database.ListPostsByThreadRow
	Attachments []database.Attachment
}

// SinglePostViewItem holds post details and its associated attachments for a single post view.
type SinglePostViewItem struct {
	Post        database.GetPostByIDRow
	Attachments []database.Attachment
}

// NewThreadPage renders the global thread creation form.
func (app *App) NewThreadPage(w http.ResponseWriter, r *http.Request) {
	app.render(w, r, "new_thread.html", map[string]any{}, "New Thread")
}

// CreateThreadSubmit handles creating a new thread with its initial post and optional attachments.
func (app *App) CreateThreadSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Parse multipart form (maxUploadSize of in-memory form data)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		setFlash(w, "flash_error", "Failed to parse upload form: "+err.Error())
		http.Redirect(w, r, "/threads/new", http.StatusSeeOther)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	body := strings.TrimSpace(r.FormValue("body"))
	expiration := r.FormValue("expiration")

	if title == "" || body == "" {
		setFlash(w, "flash_error", "Title and post content are required.")
		http.Redirect(w, r, "/threads/new", http.StatusSeeOther)
		return
	}

	// Calculate the stored TTL and the live expiry deadline. The deadline is
	// last-comment + ttl; at creation the first post is the last comment.
	// "never" stores NULL in both so the worker never touches the thread.
	var ttlSeconds sql.NullInt64
	var expiresAt sql.NullTime
	if duration, ok := map[string]time.Duration{
		"24h": 24 * time.Hour,
		"3d":  3 * 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour,
	}[expiration]; ok {
		ttlSeconds = sql.NullInt64{Int64: int64(duration.Seconds()), Valid: true}
		expiresAt = sql.NullTime{Time: time.Now().UTC().Add(duration).Round(0), Valid: true}
	}

	// Save any uploaded files to disk
	uploadedFiles, err := saveUploadedFiles(r, app.uploadDir)
	if err != nil {
		setFlash(w, "flash_error", "Error uploading files: "+err.Error())
		http.Redirect(w, r, "/threads/new", http.StatusSeeOther)
		return
	}

	// Thread, initial post, and attachments commit together: a failed
	// step must not leave a title-only thread behind.
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		setFlash(w, "flash_error", "Failed to create thread.")
		http.Redirect(w, r, "/threads/new", http.StatusSeeOther)
		return
	}
	qtx := app.queries.WithTx(tx)

	// 1. Create thread (always attributed to the signed-in user).
	thread, err := qtx.CreateThread(ctx, database.CreateThreadParams{
		UserID:     user.ID,
		Title:      title,
		ExpiresAt:  expiresAt,
		TtlSeconds: ttlSeconds,
	})
	if err == nil {
		// 2. Create initial post
		post, err := qtx.CreatePost(ctx, database.CreatePostParams{
			ThreadID: thread.ID,
			UserID:   user.ID,
			Body:     body,
		})
		if err == nil {
			// 3. Save attachments
			for _, f := range uploadedFiles {
				if _, err = qtx.CreateAttachment(ctx, database.CreateAttachmentParams{
					PostID:      post.ID,
					Filename:    f.OriginalName,
					StoragePath: f.StoragePath,
					MimeType:    f.MimeType,
					FileSize:    f.FileSize,
				}); err != nil {
					break
				}
			}
		}
	}

	if err != nil {
		tx.Rollback()
		setFlash(w, "flash_error", "Failed to create thread.")
		http.Redirect(w, r, "/threads/new", http.StatusSeeOther)
		return
	}
	if err := tx.Commit(); err != nil {
		setFlash(w, "flash_error", "Failed to create thread.")
		http.Redirect(w, r, "/threads/new", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/threads/"+strconv.FormatInt(thread.ID, 10), http.StatusSeeOther)
}

// ThreadPage renders the thread detail view with all posts and attachments.
func (app *App) ThreadPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

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

	// Double-check expiration
	if thread.ExpiresAt.Valid && thread.ExpiresAt.Time.Before(time.Now().UTC()) {
		http.NotFound(w, r)
		return
	}

	posts, err := app.queries.ListPostsByThread(ctx, thread.ID)
	if err != nil {
		http.Error(w, "failed to load posts", http.StatusInternalServerError)
		return
	}

	allAttachments, err := app.queries.ListAttachmentsByThreadID(ctx, thread.ID)
	if err != nil {
		http.Error(w, "failed to load attachments", http.StatusInternalServerError)
		return
	}

	// Group attachments by post ID
	attMap := make(map[int64][]database.Attachment)
	for _, att := range allAttachments {
		attMap[att.PostID] = append(attMap[att.PostID], att)
	}

	var postViews []PostViewItem
	for _, p := range posts {
		postViews = append(postViews, PostViewItem{
			Post:        p,
			Attachments: attMap[p.ID],
		})
	}

	app.render(w, r, "thread.html", map[string]any{
		"Thread": thread,
		"Posts":  postViews,
	}, thread.Title)
}
