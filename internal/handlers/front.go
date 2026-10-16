package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"disjoint/internal/database"
)

const frontPagePageSize = 30

// FrontPageThreadItem is one row on the global front page.
type FrontPageThreadItem struct {
	Row        database.ListFrontPageThreadsRow
	Rank       int64 // 1-based position in the current listing
	ReplyCount int64 // posts minus the opening post
	Snippet    string
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
			Snippet:    commentSnippet(row.LastCommentBody),
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
