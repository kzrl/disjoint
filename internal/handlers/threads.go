package handlers

import (
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"disjoint/internal/database"
	"disjoint/internal/middleware"
)

// PostViewItem holds post details and its associated attachments and reactions.
type PostViewItem struct {
	Post        database.ListPostsByThreadRow
	Attachments []database.Attachment
	Reactions   []ReactionGroup
	Emojis      []database.ListAllEmojisRow // full list, rendered into the picker
}

// SinglePostViewItem holds post details and its associated attachments and reactions
// for a single post view (the HTMX reply response).
type SinglePostViewItem struct {
	Post        database.GetPostByIDRow
	Attachments []database.Attachment
	Reactions   []ReactionGroup
	Emojis      []database.ListAllEmojisRow
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
	postURL, urlErr := cleanPostURL(r.FormValue("url"))

	// Save any uploaded files to disk
	uploadedFiles, err := saveUploadedFiles(r, app.uploadDir)
	if err != nil {
		setFlash(w, "flash_error", "Error uploading files: "+err.Error())
		http.Redirect(w, r, "/threads/new", http.StatusSeeOther)
		return
	}

	// A thread needs a title, and a first post with a body OR at least
	// one attachment OR a first-class link (image-only and link-only
	// posts are both allowed).
	if urlErr != nil || title == "" || (body == "" && postURL == "" && len(uploadedFiles) == 0) {
		for _, f := range uploadedFiles {
			_ = os.Remove(filepath.Join(app.uploadDir, f.StoragePath))
		}
		if urlErr != nil {
			setFlash(w, "flash_error", "Invalid link: "+urlErr.Error())
		} else {
			setFlash(w, "flash_error", "Title and post content (text, a link, or an attachment) are required.")
		}
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
			Url:      sql.NullString{String: postURL, Valid: postURL != ""},
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

	// Reactions: one query for the whole thread, grouped per post in Go. A
	// guest has no user ID, so no chip ever shows the active state for them.
	reactionRows, err := app.queries.ListReactionsByThreadID(ctx, thread.ID)
	if err != nil {
		http.Error(w, "failed to load reactions", http.StatusInternalServerError)
		return
	}
	var rRows []reactionRow
	for _, r := range reactionRows {
		rRows = append(rRows, reactionRow{
			postID:      r.PostID,
			userID:      r.UserID,
			emojiID:     r.EmojiID,
			name:        r.EmojiName,
			source:      r.EmojiSource,
			glyph:       r.EmojiGlyph,
			storagePath: r.EmojiStoragePath,
		})
	}
	currentUserID := int64(0)
	if u := middleware.UserFromContext(ctx); u != nil {
		currentUserID = u.ID
	}
	reactionGroups := groupReactions(rRows, currentUserID)

	// Every bar carries the full emoji list for its picker.
	emojis, err := app.queries.ListAllEmojis(ctx)
	if err != nil {
		http.Error(w, "failed to load emojis", http.StatusInternalServerError)
		return
	}

	var postViews []PostViewItem
	for _, p := range posts {
		postViews = append(postViews, PostViewItem{
			Post:        p,
			Attachments: attMap[p.ID],
			Reactions:   reactionGroups[p.ID],
			Emojis:      emojis,
		})
	}

	app.render(w, r, "thread.html", map[string]any{
		"Thread": thread,
		"Posts":  postViews,
	}, thread.Title)
}
