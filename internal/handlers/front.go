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

// expiryBadgeWindow is how close to expiration a thread must be for its TTL
// badge to appear on the front page. Threads with more time left show no
// badge at all, keeping rows uncluttered; the thread page itself still shows
// the deadline regardless.
const expiryBadgeWindow = 48 * time.Hour

// FrontPageThreadItem is one row on the global front page.
type FrontPageThreadItem struct {
	Row          database.ListFrontPageThreadsRow
	Rank         int64 // 1-based position in the current listing
	ReplyCount   int64 // posts minus the opening post
	ExpiryUrgent bool  // true when ExpiresAt is within expiryBadgeWindow
	Snippet      string
	Reactions    []ReactionGroup             // the thread's opening-post reactions
	Emojis       []database.ListAllEmojisRow // full list, rendered into the picker
}

// IndexPage renders the global front page: all non-expired threads sorted
// by created date (newest first), each with a preview of its most recent
// comment. Replaces the old per-board index.
func (app *App) IndexPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	rows, err := app.queries.ListFrontPageThreads(ctx, database.ListFrontPageThreadsParams{
		Limit:  frontPagePageSize,
		Offset: int64((page - 1) * frontPagePageSize),
	})
	if err != nil {
		http.Error(w, "failed to load threads", http.StatusInternalServerError)
		return
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
			})
		}
	}
	currentUserID := int64(0)
	if u := middleware.UserFromContext(ctx); u != nil {
		currentUserID = u.ID
	}
	reactionGroups := groupReactions(rRows, currentUserID)

	// Every row's bar carries the full emoji list for its picker.
	emojis, err := app.queries.ListAllEmojis(ctx)
	if err != nil {
		http.Error(w, "failed to load emojis", http.StatusInternalServerError)
		return
	}

	items := make([]FrontPageThreadItem, 0, len(rows))
	for i, row := range rows {
		replyCount := row.PostCount - 1 // the opening post is not a reply
		if replyCount < 0 {
			replyCount = 0
		}
		items = append(items, FrontPageThreadItem{
			Row:        row,
			Rank:       int64((page-1)*frontPagePageSize + i + 1),
			ReplyCount: replyCount,
			ExpiryUrgent: row.ExpiresAt.Valid &&
				time.Until(row.ExpiresAt.Time) <= expiryBadgeWindow,
			Snippet:   commentSnippet(row.LastCommentBody),
			Reactions: reactionGroups[row.FirstPostID],
			Emojis:    emojis,
		})
	}

	app.render(w, r, "index.html", map[string]any{
		"Threads": items,
		"Online":  online,
		"Page":    page,
		"HasNext": len(rows) == frontPagePageSize,
	}, "Front Page")
}

// commentSnippet flattens a raw markdown body into a plain-text preview
// (whitespace folded, capped at ~120 chars). It is rendered escaped by
// the template and never re-parsed as markdown, so half-open markup from
// the cap cannot inject HTML.
func commentSnippet(body sql.NullString) string {
	if !body.Valid {
		return ""
	}
	s := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, body.String)
	s = strings.Join(strings.Fields(s), " ")
	const maxLen = 120
	if len(s) > maxLen {
		s = strings.TrimRight(s[:maxLen], " ") + "…"
	}
	return s
}
