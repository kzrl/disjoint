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

// NewThreadPage renders the thread creation form.
func (app *App) NewThreadPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	boardIDStr := r.PathValue("id")
	boardID, err := strconv.ParseInt(boardIDStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	board, err := app.queries.GetBoardByID(ctx, boardID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	app.render(w, r, "new_thread.html", map[string]any{
		"Board": board,
	}, "New Thread - "+board.Name)
}

// CreateThreadSubmit handles creating a new thread with its initial post and optional attachments.
func (app *App) CreateThreadSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	boardIDStr := r.PathValue("id")
	boardID, err := strconv.ParseInt(boardIDStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Parse multipart form up to 50MB
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		setFlash(w, "flash_error", "Failed to parse upload form: "+err.Error())
		http.Redirect(w, r, "/boards/"+boardIDStr+"/threads/new", http.StatusSeeOther)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	body := strings.TrimSpace(r.FormValue("body"))
	expiration := r.FormValue("expiration")
	var isAnonymous int64
	if r.FormValue("anonymous") != "" {
		isAnonymous = 1
	}

	if title == "" || body == "" {
		setFlash(w, "flash_error", "Title and post content are required.")
		http.Redirect(w, r, "/boards/"+boardIDStr+"/threads/new", http.StatusSeeOther)
		return
	}

	// Calculate expiration timestamp
	var expiresAt sql.NullTime
	switch expiration {
	case "24h":
		expiresAt = sql.NullTime{Time: time.Now().UTC().Add(24 * time.Hour).Round(0), Valid: true}
	case "3d":
		expiresAt = sql.NullTime{Time: time.Now().UTC().Add(3 * 24 * time.Hour).Round(0), Valid: true}
	case "7d":
		expiresAt = sql.NullTime{Time: time.Now().UTC().Add(7 * 24 * time.Hour).Round(0), Valid: true}
	case "30d":
		expiresAt = sql.NullTime{Time: time.Now().UTC().Add(30 * 24 * time.Hour).Round(0), Valid: true}
	default: // "never" or empty
		expiresAt = sql.NullTime{Valid: false}
	}

	// Save any uploaded files to disk
	uploadedFiles, err := saveUploadedFiles(r, app.uploadDir)
	if err != nil {
		setFlash(w, "flash_error", "Error uploading files: "+err.Error())
		http.Redirect(w, r, "/boards/"+boardIDStr+"/threads/new", http.StatusSeeOther)
		return
	}

	// 1. Create thread.
	// Anonymous threads are stored with user_id = NULL so the author
	// cannot be identified by anyone, including admins.
	authorID := sql.NullInt64{Int64: user.ID, Valid: isAnonymous == 0}
	thread, err := app.queries.CreateThread(ctx, database.CreateThreadParams{
		BoardID:   boardID,
		UserID:    authorID,
		Title:     title,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		setFlash(w, "flash_error", "Failed to create thread.")
		http.Redirect(w, r, "/boards/"+boardIDStr+"/threads/new", http.StatusSeeOther)
		return
	}

	// 2. Create initial post
	post, err := app.queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID:    thread.ID,
		UserID:      authorID,
		Body:        body,
		IsAnonymous: isAnonymous,
	})
	if err != nil {
		setFlash(w, "flash_error", "Failed to create initial post.")
		http.Redirect(w, r, "/boards/"+boardIDStr, http.StatusSeeOther)
		return
	}

	// 3. Save attachments
	for _, f := range uploadedFiles {
		_, _ = app.queries.CreateAttachment(ctx, database.CreateAttachmentParams{
			PostID:      post.ID,
			Filename:    f.OriginalName,
			StoragePath: f.StoragePath,
			MimeType:    f.MimeType,
			FileSize:    f.FileSize,
		})
	}

	// Update user read state for board
	_ = app.queries.UpsertUserBoardRead(ctx, database.UpsertUserBoardReadParams{
		UserID:  user.ID,
		BoardID: boardID,
	})

	http.Redirect(w, r, "/threads/"+strconv.FormatInt(thread.ID, 10), http.StatusSeeOther)
}

// ThreadPage renders the thread detail view with all posts and attachments.
func (app *App) ThreadPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)

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

	// Mark board as read for user
	if user != nil {
		_ = app.queries.UpsertUserBoardRead(ctx, database.UpsertUserBoardReadParams{
			UserID:  user.ID,
			BoardID: thread.BoardID,
		})
	}

	app.render(w, r, "thread.html", map[string]any{
		"Thread": thread,
		"Posts":  postViews,
	}, thread.Title)
}
