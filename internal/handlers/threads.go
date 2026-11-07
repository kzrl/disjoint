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

// maxThreadTitleLength bounds the title input. The front-page rows are
// single-line, so unbounded titles would wreck the layout. Applies to new
// values only; pre-existing long titles remain viewable.
const maxThreadTitleLength = 200

// expirationToTTL maps the thread form's lifespan choice to its stored
// duration. "never" (and any unknown value) reports ok=false, which the
// callers store as NULL ttl/deadline. Shared by thread creation and the
// author's post-creation lifespan change so the choice set lives in one
// place.
func expirationToTTL(choice string) (time.Duration, bool) {
	switch choice {
	case "24h":
		return 24 * time.Hour, true
	case "3d":
		return 3 * 24 * time.Hour, true
	case "7d":
		return 7 * 24 * time.Hour, true
	case "30d":
		return 30 * 24 * time.Hour, true
	}
	return 0, false
}

// PostViewItem holds post details and its associated attachments and reactions.
type PostViewItem struct {
	Post        database.ListPostsByThreadPageRow
	Attachments []database.Attachment
	Reactions   []ReactionGroup
	// AttachmentReactions holds per-image reaction groups keyed by attachment
	// id; the post card renders a bar under each image attachment.
	AttachmentReactions map[int64][]ReactionGroup
	Emojis              []database.ListAllEmojisRow // full list, rendered into the picker
	// CanEdit is true when the viewing user is the post's author; the post
	// card renders the Edit button from it. Always false for the HTMX
	// single-post partial (a freshly created post is rendered for its
	// author, who sees the button on the next full page load).
	CanEdit bool
	// CanDelete is true for the post's author and for administrators.
	CanDelete bool
}

// SinglePostViewItem holds post details and its associated attachments and reactions
// for a single post view (the HTMX reply response).
type SinglePostViewItem struct {
	Post        database.GetPostByIDRow
	Attachments []database.Attachment
	Reactions   []ReactionGroup
	// A freshly created post's attachments cannot have reactions yet, but the
	// field exists so the post card template has one shape.
	AttachmentReactions map[int64][]ReactionGroup
	Emojis              []database.ListAllEmojisRow
	// CanEdit: the only viewer of this partial is the post's own author
	// (they just created it), so it is always true when rendered.
	CanEdit bool
	// CanDelete: the viewer just created the post, so it is always true.
	CanDelete bool
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
	if !app.allowContent(user.ID) {
		app.rejectContentRateLimit(w, r, "/threads/new")
		return
	}

	// Parse multipart form (maxUploadSize of in-memory form data)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		setFlash(w, r, "flash_error", "Failed to parse upload form: "+err.Error())
		http.Redirect(w, r, "/threads/new", http.StatusSeeOther)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	body := strings.TrimSpace(r.FormValue("body"))
	expiration := r.FormValue("expiration")
	postURL, urlErr := cleanPostURL(r.FormValue("url"))
	bodyTooLong := len(body) > maxPostBodyBytes
	if err := app.checkStorageQuota(ctx, user.ID, uploadFieldBytes(r, "files")); err != nil {
		setFlash(w, r, "flash_error", err.Error())
		http.Redirect(w, r, "/threads/new", http.StatusSeeOther)
		return
	}

	// Save any uploaded files to disk
	uploadedFiles, err := saveUploadedFiles(r, app.uploadDir)
	if err != nil {
		setFlash(w, r, "flash_error", "Error uploading files: "+err.Error())
		http.Redirect(w, r, "/threads/new", http.StatusSeeOther)
		return
	}

	// A thread needs a title, and a first post with a body OR at least
	// one attachment OR a first-class link (image-only and link-only
	// posts are both allowed).
	if bodyTooLong || urlErr != nil || title == "" || (body == "" && postURL == "" && len(uploadedFiles) == 0) {
		removeUploadedFiles(app.uploadDir, uploadedFiles)
		if bodyTooLong {
			setFlash(w, r, "flash_error", "Post body is too long (max 256 KB).")
		} else if urlErr != nil {
			setFlash(w, r, "flash_error", "Invalid link: "+urlErr.Error())
		} else {
			setFlash(w, r, "flash_error", "Title and post content (text, a link, or an attachment) are required.")
		}
		http.Redirect(w, r, "/threads/new", http.StatusSeeOther)
		return
	}

	// Calculate the stored TTL and the live expiry deadline. The deadline is
	// last-comment + ttl; at creation the first post is the last comment.
	// "never" stores NULL in both so the worker never touches the thread.
	var ttlSeconds sql.NullInt64
	var expiresAt sql.NullTime
	if duration, ok := expirationToTTL(expiration); ok {
		ttlSeconds = sql.NullInt64{Int64: int64(duration.Seconds()), Valid: true}
		expiresAt = sql.NullTime{Time: time.Now().UTC().Add(duration).Round(0), Valid: true}
	}

	// Thread, initial post, and attachments commit together: a failed
	// step must not leave a title-only thread behind.
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		removeUploadedFiles(app.uploadDir, uploadedFiles)
		setFlash(w, r, "flash_error", "Failed to create thread.")
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

	// 2. Create initial post. Note the `=` (not `:=`): declaring `post` here
	// and assigning to the outer `err` keeps a failed insert inside the same
	// transaction's error path instead of shadowing it in this block.
	var post database.Post
	if err == nil {
		post, err = qtx.CreatePost(ctx, database.CreatePostParams{
			ThreadID: thread.ID,
			UserID:   user.ID,
			Body:     body,
			Url:      sql.NullString{String: postURL, Valid: postURL != ""},
		})
	}

	// 3. Save attachments
	if err == nil {
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

	// 4. Inbox entries for anyone mentioned in the opening post. The opening
	// post is the only participation at this point, so the participant pass
	// resolves to the author and is dropped as the actor; no special case.
	if err == nil {
		err = app.notifyPostRecipients(ctx, qtx, thread.ID, post.ID, user.ID, body)
	}

	if err != nil {
		tx.Rollback()
		removeUploadedFiles(app.uploadDir, uploadedFiles)
		setFlash(w, r, "flash_error", "Failed to create thread.")
		http.Redirect(w, r, "/threads/new", http.StatusSeeOther)
		return
	}
	if err := tx.Commit(); err != nil {
		removeUploadedFiles(app.uploadDir, uploadedFiles)
		setFlash(w, r, "flash_error", "Failed to create thread.")
		http.Redirect(w, r, "/threads/new", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/threads/"+strconv.FormatInt(thread.ID, 10), http.StatusSeeOther)
}

// UpdateThreadTitleSubmit handles the author renaming their thread.
// Ownership is enforced in the UPDATE's WHERE clause; a Go-side check right
// after loading puts the 404 ahead of any validation error so a non-author
// never learns whether a thread exists.
func (app *App) UpdateThreadTitleSubmit(w http.ResponseWriter, r *http.Request) {
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

	threadID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	thread, err := app.queries.GetThreadByID(ctx, threadID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if thread.UserID != user.ID {
		http.NotFound(w, r)
		return
	}
	if thread.ExpiresAt.Valid && thread.ExpiresAt.Time.Before(time.Now().UTC()) {
		http.Error(w, "this thread has expired", http.StatusGone)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		setFlash(w, r, "flash_error", "Thread title is required.")
		http.Redirect(w, r, threadPageURL(r, thread.ID), http.StatusSeeOther)
		return
	}
	if len(title) > maxThreadTitleLength {
		setFlash(w, r, "flash_error", fmt.Sprintf("Title is too long (max %d characters).", maxThreadTitleLength))
		http.Redirect(w, r, threadPageURL(r, thread.ID), http.StatusSeeOther)
		return
	}

	if title == thread.Title {
		setFlash(w, r, "flash_success", "No changes to save.")
		http.Redirect(w, r, threadPageURL(r, thread.ID), http.StatusSeeOther)
		return
	}

	rows, err := app.queries.UpdateThreadTitle(ctx, database.UpdateThreadTitleParams{
		Title:  title,
		ID:     thread.ID,
		UserID: user.ID,
	})
	if err != nil {
		http.Error(w, "failed to update thread title", http.StatusInternalServerError)
		return
	}
	if rows == 0 {
		http.NotFound(w, r)
		return
	}

	setFlash(w, r, "flash_success", "Title updated.")
	http.Redirect(w, r, threadPageURL(r, thread.ID), http.StatusSeeOther)
}

// UpdateThreadExpirationSubmit handles the author changing their thread's
// lifespan. The deadline resets to now + new ttl (or NULL for Never) — the
// same "ttl of silence from now" rule every reply applies. Ownership is
// enforced in the UPDATE's WHERE clause; a Go-side check right after
// loading puts the 404 ahead of any validation error so a non-author never
// learns whether a thread exists.
func (app *App) UpdateThreadExpirationSubmit(w http.ResponseWriter, r *http.Request) {
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

	threadID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	thread, err := app.queries.GetThreadByID(ctx, threadID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if thread.UserID != user.ID {
		http.NotFound(w, r)
		return
	}
	// Expired threads must not be revived: apart from violating the TTL
	// contract, a concurrent worker purge may already be deleting its files.
	if thread.ExpiresAt.Valid && thread.ExpiresAt.Time.Before(time.Now().UTC()) {
		http.Error(w, "this thread has expired", http.StatusGone)
		return
	}

	choice := r.FormValue("expiration")
	duration, hasTTL := expirationToTTL(choice) // "never" and unknown both report false
	if choice != "never" && !hasTTL {
		setFlash(w, r, "flash_error", "Unknown lifespan.")
		http.Redirect(w, r, threadPageURL(r, thread.ID), http.StatusSeeOther)
		return
	}

	// No-op detection: resubmitting the current choice must leave expires_at
	// byte-identical (the reset would otherwise quietly shorten the deadline).
	currentIsNever := !thread.TtlSeconds.Valid
	if (currentIsNever && choice == "never") ||
		(!currentIsNever && hasTTL && int64(duration.Seconds()) == thread.TtlSeconds.Int64) {
		setFlash(w, r, "flash_success", "No changes to save.")
		http.Redirect(w, r, threadPageURL(r, thread.ID), http.StatusSeeOther)
		return
	}

	var ttlSeconds sql.NullInt64
	var expiresAt sql.NullTime
	if hasTTL {
		ttlSeconds = sql.NullInt64{Int64: int64(duration.Seconds()), Valid: true}
		expiresAt = sql.NullTime{Time: time.Now().UTC().Add(duration).Round(0), Valid: true}
	}

	rows, err := app.queries.SetThreadExpiration(ctx, database.SetThreadExpirationParams{
		TtlSeconds: ttlSeconds,
		ExpiresAt:  expiresAt,
		ID:         thread.ID,
		UserID:     user.ID,
	})
	if err != nil {
		http.Error(w, "failed to update thread expiration", http.StatusInternalServerError)
		return
	}
	if rows == 0 {
		http.NotFound(w, r)
		return
	}

	setFlash(w, r, "flash_success", "Expiration updated.")
	http.Redirect(w, r, threadPageURL(r, thread.ID), http.StatusSeeOther)
}

// expiryPercent reports how much of a thread's current lifespan is left, as
// an integer percentage, for the header's TTL bar. Threads that never expire
// (or whose deadline is in the past) report 0, which hides the bar.
func expiryPercent(ttl sql.NullInt64, expiresAt sql.NullTime) int {
	if !ttl.Valid || ttl.Int64 <= 0 || !expiresAt.Valid {
		return 0
	}
	remaining := time.Until(expiresAt.Time)
	if remaining <= 0 {
		return 0
	}
	pct := int(float64(remaining) / float64(time.Duration(ttl.Int64)*time.Second) * 100)
	if pct > 100 {
		return 100
	}
	return pct
}

// threadPageSize is the number of posts per thread page. Page 1 includes the
// opening post; the last page carries the composer and the live-update pill.
const threadPageSize = 20

// threadPageURL returns the canonical thread URL for the page submitted with a
// title/expiration form, so an edit made on page 3 does not bounce the user
// back to page 1.
func threadPageURL(r *http.Request, threadID int64) string {
	if page, err := strconv.Atoi(r.FormValue("page")); err == nil && page > 1 {
		return fmt.Sprintf("/threads/%d?page=%d", threadID, page)
	}
	return fmt.Sprintf("/threads/%d", threadID)
}

// ThreadPage renders one page of a thread. Pagination is chronological:
// page 1 starts with the opening post, and the last page holds the newest
// replies plus the composer. ?from=<post-id> redirects to the page containing
// that post, so front-page/inbox/quote permalinks work across pages.
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

	totalPosts, err := app.queries.CountPostsByThreadID(ctx, thread.ID)
	if err != nil {
		http.Error(w, "failed to count posts", http.StatusInternalServerError)
		return
	}
	totalPages := int((totalPosts + threadPageSize - 1) / threadPageSize)
	if totalPages < 1 {
		totalPages = 1
	}

	// ?from=<post-id> is used by permalinks (front-page last comment, inbox,
	// quotes, the new-replies pill). Resolve it to the page that contains the
	// post, then redirect to a canonical page URL with the anchor intact.
	if fromRaw := r.URL.Query().Get("from"); fromRaw != "" {
		fromID, parseErr := strconv.ParseInt(fromRaw, 10, 64)
		if parseErr == nil {
			post, postErr := app.queries.GetPostByID(ctx, fromID)
			if postErr == nil && post.ThreadID == thread.ID {
				position, countErr := app.queries.CountPostsUpToInThread(ctx, database.CountPostsUpToInThreadParams{
					ThreadID: thread.ID,
					ID:       fromID,
				})
				if countErr == nil && position > 0 {
					targetPage := int((position-1)/threadPageSize) + 1
					http.Redirect(w, r, fmt.Sprintf("/threads/%d?page=%d#post-%d", thread.ID, targetPage, fromID), http.StatusSeeOther)
					return
				}
			}
		}
		// Unknown/stale/foreign post id: land on page 1 rather than looping
		// back through ?from= forever.
		http.Redirect(w, r, fmt.Sprintf("/threads/%d?page=1", thread.ID), http.StatusSeeOther)
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	isLastPage := page == totalPages

	viewerID := int64(0)
	isAdmin := false
	if u := middleware.UserFromContext(ctx); u != nil {
		viewerID = u.ID
		isAdmin = u.IsAdmin == 1
	}

	// Reading a thread marks it read for the front page's unread dots.
	// Best-effort: a failed marker must not break the page itself.
	if viewerID != 0 {
		_ = app.queries.MarkThreadRead(ctx, database.MarkThreadReadParams{
			UserID:   viewerID,
			ThreadID: thread.ID,
		})
		// Opening the conversation counts as dealing with its inbox entry, so
		// the row clears (and the navbar badge drops) without opening /inbox.
		_ = app.queries.MarkThreadNotificationRead(ctx, database.MarkThreadNotificationReadParams{
			UserID:   viewerID,
			ThreadID: thread.ID,
		})
	}

	posts, err := app.queries.ListPostsByThreadPage(ctx, database.ListPostsByThreadPageParams{
		ThreadID: thread.ID,
		Limit:    threadPageSize,
		Offset:   int64((page - 1) * threadPageSize),
	})
	if err != nil {
		http.Error(w, "failed to load posts", http.StatusInternalServerError)
		return
	}

	postIDs := make([]int64, 0, len(posts))
	for _, p := range posts {
		postIDs = append(postIDs, p.ID)
	}

	var pageAttachments []database.Attachment
	if len(postIDs) > 0 {
		pageAttachments, err = app.queries.ListAttachmentsByPostIDs(ctx, postIDs)
		if err != nil {
			http.Error(w, "failed to load attachments", http.StatusInternalServerError)
			return
		}
	}

	// Group attachments by post ID.
	attMap := make(map[int64][]database.Attachment)
	for _, att := range pageAttachments {
		attMap[att.PostID] = append(attMap[att.PostID], att)
	}

	// Per-image reactions for attachments shown on this page (the same
	// attachment_reactions rows the gallery uses, so both views agree).
	attachmentReactionGroups := make(map[int64][]ReactionGroup)
	var attachmentIDs []int64
	for _, att := range pageAttachments {
		if strings.HasPrefix(att.MimeType, "image/") {
			attachmentIDs = append(attachmentIDs, att.ID)
		}
	}
	if len(attachmentIDs) > 0 {
		attReactionRows, err := app.queries.ListReactionsByAttachmentIDs(ctx, attachmentIDs)
		if err != nil {
			http.Error(w, "failed to load image reactions", http.StatusInternalServerError)
			return
		}
		attRows := make([]reactionRow, 0, len(attReactionRows))
		for _, r := range attReactionRows {
			attRows = append(attRows, reactionRow{
				targetID:    r.AttachmentID,
				userID:      r.UserID,
				emojiID:     r.EmojiID,
				name:        r.EmojiName,
				source:      r.EmojiSource,
				glyph:       r.EmojiGlyph,
				storagePath: r.EmojiStoragePath,
				username:    r.Username,
			})
		}
		attachmentReactionGroups = groupReactions(attRows, viewerID)
	}

	// Reactions for this page only. A guest has no user ID, so no chip ever
	// shows the active state for them.
	var rRows []reactionRow
	if len(postIDs) > 0 {
		reactionRows, err := app.queries.ListReactionsByPostIDs(ctx, postIDs)
		if err != nil {
			http.Error(w, "failed to load reactions", http.StatusInternalServerError)
			return
		}
		for _, r := range reactionRows {
			rRows = append(rRows, reactionRow{
				targetID:    r.PostID,
				userID:      r.UserID,
				emojiID:     r.EmojiID,
				name:        r.EmojiName,
				source:      r.EmojiSource,
				glyph:       r.EmojiGlyph,
				storagePath: r.EmojiStoragePath,
				username:    r.Username,
			})
		}
	}
	reactionGroups := groupReactions(rRows, viewerID)

	// Every bar carries the full emoji list for its picker.
	emojis, err := app.queries.ListAllEmojis(ctx)
	if err != nil {
		http.Error(w, "failed to load emojis", http.StatusInternalServerError)
		return
	}

	firstPostID, err := app.queries.GetFirstPostIDByThreadID(ctx, thread.ID)
	if err != nil {
		http.Error(w, "failed to load thread", http.StatusInternalServerError)
		return
	}

	// CanEdit is author-only (no admin override, per tasks/plan-editing.md);
	// guests have no user so it is always false for them. The opening post is
	// deleted through the thread header, never through the post card.
	var postViews []PostViewItem
	for _, p := range posts {
		postViews = append(postViews, PostViewItem{
			Post:                p,
			Attachments:         attMap[p.ID],
			Reactions:           reactionGroups[p.ID],
			AttachmentReactions: attachmentReactionGroups,
			Emojis:              emojis,
			CanEdit:             p.UserID == viewerID,
			CanDelete:           (p.UserID == viewerID || isAdmin) && p.ID != firstPostID,
		})
	}

	replyCount := totalPosts - 1 // the opening post is not a reply
	if replyCount < 0 {
		replyCount = 0
	}

	// The "N new replies" pill polls for posts newer than the newest one
	// rendered here. It is only meaningful on the last page; earlier pages
	// would immediately offer already-existing later pages as "new".
	lastPostID := int64(0)
	if len(posts) > 0 {
		lastPostID = posts[len(posts)-1].ID
	}
	newPosts := NewPostsView{}
	if isLastPage {
		newPosts = newPostsView(thread.ID, lastPostID, 0, 0)
	}

	app.render(w, r, "thread.html", map[string]any{
		"Thread":          thread,
		"Posts":           postViews,
		"ReplyCount":      replyCount,
		"ExpiryPercent":   expiryPercent(thread.TtlSeconds, thread.ExpiresAt),
		"NewPosts":        newPosts,
		"CanDeleteThread": thread.UserID == viewerID || isAdmin,
		"Page":            page,
		"TotalPages":      totalPages,
		"IsLastPage":      isLastPage,
	}, thread.Title)
}

// NewPostsView drives the thread page's "N new replies" pill. The container
// polls itself (UpdatesURL); when other users have posted since the page
// rendered, it offers a reload link anchored at the first unseen post. A reload
// — rather than an HTMX append — keeps chronological order intact when the
// viewer's own HTMX replies are interleaved with other users' posts, and it
// refreshes the unread/notification markers for free.
type NewPostsView struct {
	Count      int64
	UpdatesURL string
	ReloadURL  string
	Poll       bool
}

func newPostsView(threadID, after, count, firstNewID int64) NewPostsView {
	v := NewPostsView{
		Count:      count,
		UpdatesURL: fmt.Sprintf("/threads/%d/updates?after=%d", threadID, after),
		Poll:       true,
	}
	if firstNewID != 0 {
		// ?from=… only exists to make the URL different from the current one:
		// navigating to the same path with only a fragment change would not
		// reload the page, so the new posts would never render.
		v.ReloadURL = fmt.Sprintf("/threads/%d?from=%d#post-%d", threadID, firstNewID, firstNewID)
	}
	return v
}

// ThreadUpdates re-renders the thread page's new-posts pill. It always answers
// 200 with a container: an empty response swapped into the container would
// delete the polling element, and a 4xx is not swapped by HTMX, so the client
// would keep polling a dead URL. A missing or expired thread answers with a
// container that has no hx-trigger, which ends the loop quietly.
func (app *App) ThreadUpdates(w http.ResponseWriter, r *http.Request) {
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

	// Poll fragments must never be served from a cache: the whole point is to
	// observe that something changed since the last tick.
	noStore := func() {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	stop := func() {
		noStore()
		if err := app.newPostsTmpl.ExecuteTemplate(w, "new_posts", NewPostsView{}); err != nil {
			http.Error(w, "failed to render new posts", http.StatusInternalServerError)
		}
	}

	threadID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		stop()
		return
	}
	thread, err := app.queries.GetThreadByID(ctx, threadID)
	if err != nil {
		stop()
		return
	}
	if thread.ExpiresAt.Valid && thread.ExpiresAt.Time.Before(time.Now().UTC()) {
		stop()
		return
	}

	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil || after < 0 {
		after = 0
	}

	row, err := app.queries.NewPostsAfter(ctx, database.NewPostsAfterParams{
		ThreadID: thread.ID,
		After:    after,
		ViewerID: user.ID,
	})
	if err != nil {
		http.Error(w, "failed to count new posts", http.StatusInternalServerError)
		return
	}

	noStore()
	if err := app.newPostsTmpl.ExecuteTemplate(w, "new_posts", newPostsView(thread.ID, after, row.NewCount, row.FirstNewID)); err != nil {
		http.Error(w, "failed to render new posts", http.StatusInternalServerError)
	}
}
