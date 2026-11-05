package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"disjoint/internal/database"
	"disjoint/internal/middleware"
	"disjoint/internal/thumbs"
)

// removeAttachmentFiles deletes attachment files (and their cached
// thumbnails) after their rows are gone. Best-effort, matching the expiry
// worker: a leftover file is preferable to failing an already-committed
// deletion.
func (app *App) removeAttachmentFiles(attachments []database.Attachment) {
	for _, att := range attachments {
		if att.StoragePath == "" {
			continue
		}
		fullPath := filepath.Join(app.uploadDir, filepath.Clean(att.StoragePath))
		if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[handlers] Warning: could not delete attachment file %s: %v", fullPath, err)
		}
		thumbs.Remove(app.uploadDir, att.StoragePath)
	}
}

// deleteThreadAndFiles removes a thread row (cascading posts, attachments,
// reactions, reads, and notifications) and then its files on disk.
func (app *App) deleteThreadAndFiles(ctx context.Context, threadID int64) error {
	attachments, err := app.queries.ListAttachmentsByThreadID(ctx, threadID)
	if err != nil {
		return err
	}
	if err := app.queries.DeleteThread(ctx, threadID); err != nil {
		return err
	}
	app.removeAttachmentFiles(attachments)
	return nil
}

// DeleteThreadSubmit removes a thread and everything under it. Allowed for the
// thread's author and for administrators; everyone else gets a 404.
func (app *App) DeleteThreadSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	threadID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	thread, err := app.queries.GetThreadByID(ctx, threadID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if thread.UserID != user.ID && user.IsAdmin != 1 {
		http.NotFound(w, r)
		return
	}

	if err := app.deleteThreadAndFiles(ctx, thread.ID); err != nil {
		http.Error(w, "failed to delete thread", http.StatusInternalServerError)
		return
	}

	setFlash(w, "flash_success", "Thread deleted.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// DeletePostSubmit removes a reply, or the whole thread when the post is the
// opening post (a thread cannot exist without it). Allowed for the post's
// author and for administrators; everyone else gets a 404.
func (app *App) DeletePostSubmit(w http.ResponseWriter, r *http.Request) {
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
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if post.UserID != user.ID && user.IsAdmin != 1 {
		http.NotFound(w, r)
		return
	}

	firstPostID, err := app.queries.GetFirstPostIDByThreadID(ctx, post.ThreadID)
	if err != nil {
		http.Error(w, "failed to load thread", http.StatusInternalServerError)
		return
	}

	if post.ID == firstPostID {
		thread, err := app.queries.GetThreadByID(ctx, post.ThreadID)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if thread.UserID != user.ID && user.IsAdmin != 1 {
			http.NotFound(w, r)
			return
		}
		if err := app.deleteThreadAndFiles(ctx, thread.ID); err != nil {
			http.Error(w, "failed to delete thread", http.StatusInternalServerError)
			return
		}
		setFlash(w, "flash_success", "Thread deleted.")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	attachments, err := app.queries.ListAttachmentsByPostID(ctx, post.ID)
	if err != nil {
		http.Error(w, "failed to load attachments", http.StatusInternalServerError)
		return
	}
	if err := app.queries.DeletePost(ctx, post.ID); err != nil {
		http.Error(w, "failed to delete post", http.StatusInternalServerError)
		return
	}
	app.removeAttachmentFiles(attachments)

	setFlash(w, "flash_success", "Post deleted.")
	http.Redirect(w, r, fmt.Sprintf("/threads/%d", post.ThreadID), http.StatusSeeOther)
}
