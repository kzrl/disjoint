package handlers

import (
	"net/http"
	"strconv"
	"time"

	"disjoint/internal/database"
	"disjoint/internal/middleware"
)

// inboxPageSize mirrors the front page: 30 conversations per page.
const inboxPageSize = 30

// InboxItem is one conversation row in /inbox. It flattens the joined
// notification/thread/post/user row so the template needs no conversions.
type InboxItem struct {
	ThreadID         int64
	PostID           int64
	Reason           string // "mention" | "reply"
	Title            string
	Snippet          string
	URL              string // link-only posts have no body to preview
	ActorUsername    string
	ActorAvatar      string
	ActorAvatarNonce int64
	Time             time.Time
	Unread           bool
}

// InboxPage renders the viewer's mentions & replies inbox. It deliberately
// does not mark anything read: an entry clears when the viewer opens the
// thread (ThreadPage's read-through) or presses "Mark all read" (see
// MarkInboxReadSubmit). The badge is a claim about what has been dealt with,
// not about which page happened to load.
func (app *App) InboxPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 1 {
		page = p
	}

	rows, err := app.queries.ListInbox(ctx, database.ListInboxParams{
		UserID: user.ID,
		Limit:  inboxPageSize,
		Offset: int64((page - 1) * inboxPageSize),
	})
	if err != nil {
		http.Error(w, "failed to load inbox", http.StatusInternalServerError)
		return
	}

	items := make([]InboxItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, InboxItem{
			ThreadID:         row.ThreadID,
			PostID:           row.PostID,
			Reason:           row.Reason,
			Title:            row.Title,
			Snippet:          plainSnippet(row.Body),
			URL:              row.Url,
			ActorUsername:    row.ActorUsername,
			ActorAvatar:      row.ActorAvatar,
			ActorAvatarNonce: row.ActorAvatarNonce,
			Time:             row.LastEventAt,
			Unread:           !row.ReadAt.Valid,
		})
	}

	app.render(w, r, "inbox.html", map[string]any{
		"Items":   items,
		"Page":    page,
		"HasNext": len(rows) == inboxPageSize,
	}, "Inbox")
}

// MarkInboxReadSubmit clears every unread inbox entry for the viewer (the
// "Mark all read" button). Plain form + redirect: re-rendering the whole page
// is what clears the row highlights and the navbar badge together, which an
// HTMX partial swap of the list alone could not do.
func (app *App) MarkInboxReadSubmit(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if err := app.queries.MarkAllNotificationsRead(r.Context(), user.ID); err != nil {
		setFlash(w, "flash_error", "Could not mark the inbox read.")
		http.Redirect(w, r, "/inbox", http.StatusSeeOther)
		return
	}

	setFlash(w, "flash_success", "Inbox cleared.")
	http.Redirect(w, r, "/inbox", http.StatusSeeOther)
}

// InboxCount re-renders only the navbar bell for the inbox poll
// (hx-get="/inbox/count"). Returning the anchor itself — not a bare badge — is
// what keeps the hx-trigger alive across the outerHTML swap.
func (app *App) InboxCount(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	count, err := app.queries.CountUnreadNotifications(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "failed to count unread notifications", http.StatusInternalServerError)
		return
	}

	// The fragment is a live unread count: never let a browser or proxy reuse
	// a previous poll's answer.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := app.inboxBellTmpl.ExecuteTemplate(w, "inbox_bell", map[string]any{
		"UnreadNotifications": count,
	}); err != nil {
		http.Error(w, "failed to render inbox bell", http.StatusInternalServerError)
	}
}
