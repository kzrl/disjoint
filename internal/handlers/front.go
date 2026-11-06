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

const frontPagePageSize = 30

// frontPageImageCap caps the thumbnails shown per row on the front page;
// the overflow renders as a "+N" chip. Keeps page weight bounded (30 rows ×
// 8 lazy-loaded images at most).
const frontPageImageCap = 8

// maxSearchQueryLength bounds the front-page search term supplied as ?q= (the
// page has no search box, so the URL is the only entry point). Longer input is
// truncated rather than rejected.
const maxSearchQueryLength = 100

// expiryBadgeWindow is how close to expiration a thread must be for its TTL
// badge to appear on the front page. Threads with more time left show no
// badge at all, keeping rows uncluttered; the thread page itself still shows
// the deadline regardless.
const expiryBadgeWindow = 48 * time.Hour

// liveThreadWindow marks a thread as "live" on the front page (pulsing dot
// next to the title) when it received a comment this recently.
const liveThreadWindow = time.Hour

// FrontPageImage is one thumbnail on a front-page row's image strip.
type FrontPageImage struct {
	StoragePath string
	Filename    string
}

// FrontPageThreadItem is one row on the global front page.
type FrontPageThreadItem struct {
	Row          database.ListFrontPageThreadsRow
	ReplyCount   int64 // posts minus the opening post
	ExpiryUrgent bool  // true when ExpiresAt is within expiryBadgeWindow
	IsLive       bool  // replied to within liveThreadWindow
	Unread       bool  // has posts the viewer has not seen yet
	Snippet      string
	Reactions    []ReactionGroup             // the thread's opening-post reactions
	Emojis       []database.ListAllEmojisRow // full list, rendered into the picker
	Images       []FrontPageImage            // opening-post image attachments, capped
	MoreImages   int                         // image count beyond frontPageImageCap
}

// IndexPage renders the global front page: all non-expired threads sorted
// by created date (newest first), each with a preview of its most recent
// comment. Replaces the old per-board index.
func (app *App) IndexPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Defense-in-depth: the middleware normally guarantees this, but the
	// index must never render without a viewer even if a public path is
	// accidentally routed here.
	if middleware.UserFromContext(ctx) == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	// Front-page search: the term filters threads by title or any post body.
	// LIKE's % and _ act as wildcards here, which is acceptable for a simple
	// self-hosted search; the pattern is still length-capped.
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if runes := []rune(query); len(runes) > maxSearchQueryLength {
		query = string(runes[:maxSearchQueryLength])
	}
	if query != "" {
		if user := middleware.UserFromContext(ctx); user != nil && !allowUser(app.apiLimiter, user.ID) {
			w.Header().Set("Retry-After", "300")
			http.Error(w, "too many searches; slow down", http.StatusTooManyRequests)
			return
		}
	}

	limit := int64(frontPagePageSize)
	offset := int64((page - 1) * frontPagePageSize)
	var rows []database.ListFrontPageThreadsRow
	if query != "" {
		pattern := "%" + query + "%"
		searchRows, err := app.queries.SearchFrontPageThreads(ctx, database.SearchFrontPageThreadsParams{
			Title:  pattern,
			Body:   pattern,
			Limit:  limit,
			Offset: offset,
		})
		if err != nil {
			http.Error(w, "failed to search threads", http.StatusInternalServerError)
			return
		}
		rows = make([]database.ListFrontPageThreadsRow, 0, len(searchRows))
		for _, sr := range searchRows {
			rows = append(rows, database.ListFrontPageThreadsRow{
				ID:                sr.ID,
				Title:             sr.Title,
				CreatedAt:         sr.CreatedAt,
				ExpiresAt:         sr.ExpiresAt,
				TtlSeconds:        sr.TtlSeconds,
				AuthorUsername:    sr.AuthorUsername,
				PostCount:         sr.PostCount,
				FirstPostID:       sr.FirstPostID,
				LastCommentID:     sr.LastCommentID,
				LastCommentBody:   sr.LastCommentBody,
				LastCommentAt:     sr.LastCommentAt,
				LastCommentAuthor: sr.LastCommentAuthor,
			})
		}
	} else {
		var err error
		rows, err = app.queries.ListFrontPageThreads(ctx, database.ListFrontPageThreadsParams{
			Limit:  limit,
			Offset: offset,
		})
		if err != nil {
			http.Error(w, "failed to load threads", http.StatusInternalServerError)
			return
		}
	}

	// "On the air": users seen within the last 5 minutes (includes the viewer)
	online, err := app.queries.ListOnlineUsers(ctx)
	if err != nil {
		http.Error(w, "failed to load online users", http.StatusInternalServerError)
		return
	}

	// Reactions on each thread's opening post: one query for the whole
	// page (skipped when the page is empty — an empty IN list is a syntax
	// error). A FirstPostID of 0 means the thread somehow has no posts; the
	// reaction lookup simply never matches it.
	var postIDs []int64
	for _, row := range rows {
		if row.FirstPostID > 0 {
			postIDs = append(postIDs, row.FirstPostID)
		}
	}
	var rRows []reactionRow
	if len(postIDs) > 0 {
		reactionRows, err := app.queries.ListReactionsByPostIDs(ctx, postIDs)
		if err != nil {
			http.Error(w, "failed to load reactions", http.StatusInternalServerError)
			return
		}
		for _, r := range reactionRows {
			rRows = append(rRows, reactionRow{
				postID:      r.PostID,
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
	currentUserID := int64(0)
	var currentUser *database.User
	if u := middleware.UserFromContext(ctx); u != nil {
		currentUser = u
		currentUserID = u.ID
	}

	// Unread threads for the viewer: one lightweight query returning thread
	// IDs, then a set lookup while building the rows. Guests get no dots.
	unread := make(map[int64]bool)
	if currentUser != nil {
		accountCreatedAt := sql.NullTime{Time: time.Now().UTC(), Valid: true}
		if currentUser.CreatedAt.Valid {
			accountCreatedAt = currentUser.CreatedAt
		}
		unreadIDs, err := app.queries.ListUnreadThreadIDs(ctx, database.ListUnreadThreadIDsParams{
			ViewerID:         currentUser.ID,
			AccountCreatedAt: accountCreatedAt,
		})
		if err != nil {
			http.Error(w, "failed to load unread threads", http.StatusInternalServerError)
			return
		}
		for _, id := range unreadIDs {
			unread[id] = true
		}
	}
	reactionGroups := groupReactions(rRows, currentUserID)

	// Opening-post image attachments for the image strips: one query for the
	// whole page (skipped when the page is empty — an empty IN list is a
	// syntax error), grouped by thread in attachment order.
	var imagePathsByThread map[int64][]FrontPageImage
	if len(postIDs) > 0 {
		imageRows, err := app.queries.ListFrontPageThreadImages(ctx, postIDs)
		if err != nil {
			http.Error(w, "failed to load thread images", http.StatusInternalServerError)
			return
		}
		imagePathsByThread = make(map[int64][]FrontPageImage, len(imageRows))
		for _, r := range imageRows {
			imagePathsByThread[r.ThreadID] = append(imagePathsByThread[r.ThreadID], FrontPageImage{
				StoragePath: r.StoragePath,
				Filename:    r.Filename,
			})
		}
	}

	// Every row's bar carries the full emoji list for its picker.
	emojis, err := app.queries.ListAllEmojis(ctx)
	if err != nil {
		http.Error(w, "failed to load emojis", http.StatusInternalServerError)
		return
	}

	items := make([]FrontPageThreadItem, 0, len(rows))
	for _, row := range rows {
		replyCount := row.PostCount - 1 // the opening post is not a reply
		if replyCount < 0 {
			replyCount = 0
		}
		images := imagePathsByThread[row.ID]
		moreImages := len(images) - frontPageImageCap
		if moreImages < 0 {
			moreImages = 0
		} else {
			images = images[:frontPageImageCap]
		}
		items = append(items, FrontPageThreadItem{
			Row:        row,
			ReplyCount: replyCount,
			ExpiryUrgent: row.ExpiresAt.Valid &&
				time.Until(row.ExpiresAt.Time) <= expiryBadgeWindow,
			IsLive: row.LastCommentAt.Valid &&
				time.Since(row.LastCommentAt.Time) < liveThreadWindow,
			Unread:     unread[row.ID],
			Snippet:    commentSnippet(row.LastCommentBody),
			Reactions:  reactionGroups[row.FirstPostID],
			Emojis:     emojis,
			Images:     images,
			MoreImages: moreImages,
		})
	}

	app.render(w, r, "index.html", map[string]any{
		"Threads": items,
		"Online":  online,
		"Page":    page,
		"HasNext": len(rows) == frontPagePageSize,
		"Query":   query,
	}, "Front Page")
}

// commentSnippet flattens a raw markdown body into a plain-text preview.
// It is rendered escaped by the template and never re-parsed as markdown, so
// half-open markup from the cap cannot inject HTML.
func commentSnippet(body sql.NullString) string {
	if !body.Valid {
		return ""
	}
	return plainSnippet(body.String)
}

// plainSnippet is commentSnippet for a non-null body (the inbox's post body).
// Whitespace is folded and the result is capped at ~120 chars.
func plainSnippet(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	const maxLen = 120
	if len(s) > maxLen {
		s = strings.TrimRight(s[:maxLen], " ") + "…"
	}
	return s
}
