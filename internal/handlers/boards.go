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

// BoardViewItem represents a board on the index page with its metrics and latest thread.
type BoardViewItem struct {
	Board        database.Board
	ThreadCount  int64
	PostCount    int64
	LatestThread *database.ListLatestThreadsByBoardRow
	IsUnread     bool
}

// IndexPage renders the main forum index with boards and latest thread summaries.
func (app *App) IndexPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)

	boardDetails, err := app.queries.ListBoardsWithDetails(ctx)
	if err != nil {
		http.Error(w, "failed to load boards", http.StatusInternalServerError)
		return
	}

	// Fetch user board read timestamps
	readMap := make(map[int64]time.Time)
	if user != nil {
		reads, err := app.queries.ListUserBoardReads(ctx, user.ID)
		if err == nil {
			for _, rd := range reads {
				if rd.LastReadAt.Valid {
					readMap[rd.BoardID] = rd.LastReadAt.Time
				}
			}
		}
	}

	// "On the air": users seen within the last 5 minutes (includes the viewer)
	online, err := app.queries.ListOnlineUsers(ctx)
	if err != nil {
		http.Error(w, "failed to load online users", http.StatusInternalServerError)
		return
	}

	// Recent activity ticker: newest posts across all boards
	activity, err := app.queries.ListRecentActivity(ctx, 10)
	if err != nil {
		http.Error(w, "failed to load recent activity", http.StatusInternalServerError)
		return
	}

	// Latest thread preview per board (one query for all boards)
	latestRows, err := app.queries.ListLatestThreadsByBoard(ctx)
	if err != nil {
		http.Error(w, "failed to load latest threads", http.StatusInternalServerError)
		return
	}
	latestMap := make(map[int64]*database.ListLatestThreadsByBoardRow, len(latestRows))
	for i := range latestRows {
		latestMap[latestRows[i].BoardID] = &latestRows[i]
	}

	// Build the flat board list (already ordered by position, id)
	boards := make([]BoardViewItem, 0, len(boardDetails))
	for _, bd := range boardDetails {
		// Check unread status
		isUnread := false
		if user != nil {
			if latestTime, ok := bd.LatestUpdatedTime(); ok {
				lastRead, hasRead := readMap[bd.ID]
				if !hasRead || latestTime.After(lastRead) {
					isUnread = true
				}
			}
		}

		boards = append(boards, BoardViewItem{
			Board: database.Board{
				ID:          bd.ID,
				Name:        bd.Name,
				Description: bd.Description,
				Position:    bd.Position,
			},
			ThreadCount:  bd.ThreadCount,
			PostCount:    bd.PostCount,
			LatestThread: latestMap[bd.ID],
			IsUnread:     isUnread,
		})
	}

	app.render(w, r, "index.html", map[string]any{
		"Boards":   boards,
		"Online":   online,
		"Activity": activity,
	}, "Board Index")
}

// ThreadListItem represents a thread on a board page.
type ThreadListItem struct {
	Thread     database.ListThreadsByBoardRow
	ReplyCount int64
	IsUnread   bool
}

// BoardPage renders a specific board and its threads.
func (app *App) BoardPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)

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

	// Read last read timestamp before updating
	var lastReadTime time.Time
	var hasLastRead bool
	if user != nil {
		rd, err := app.queries.GetUserBoardRead(ctx, database.GetUserBoardReadParams{
			UserID:  user.ID,
			BoardID: board.ID,
		})
		if err == nil && rd.Valid {
			lastReadTime = rd.Time
			hasLastRead = true
		}
	}

	threads, err := app.queries.ListThreadsByBoard(ctx, board.ID)
	if err != nil {
		http.Error(w, "failed to load threads", http.StatusInternalServerError)
		return
	}

	var threadItems []ThreadListItem
	for _, t := range threads {
		replyCount := t.PostCount - 1
		if replyCount < 0 {
			replyCount = 0
		}

		isUnread := false
		if user != nil && t.UpdatedAt.Valid {
			if !hasLastRead || t.UpdatedAt.Time.After(lastReadTime) {
				isUnread = true
			}
		}

		threadItems = append(threadItems, ThreadListItem{
			Thread:     t,
			ReplyCount: replyCount,
			IsUnread:   isUnread,
		})
	}

	// Update user board read timestamp
	if user != nil {
		_ = app.queries.UpsertUserBoardRead(ctx, database.UpsertUserBoardReadParams{
			UserID:  user.ID,
			BoardID: board.ID,
		})
	}

	app.render(w, r, "board.html", map[string]any{
		"Board":   board,
		"Threads": threadItems,
	}, board.Name)
}

// NewBoardPage renders the form to create a new board (admin only).
func (app *App) NewBoardPage(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}
	app.render(w, r, "new_board.html", map[string]any{}, "Create Board")
}

// CreateBoardSubmit handles creating a new board (admin only).
func (app *App) CreateBoardSubmit(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}

	ctx := r.Context()
	name := strings.TrimSpace(r.FormValue("name"))
	description := strings.TrimSpace(r.FormValue("description"))
	if name == "" {
		setFlash(w, "flash_error", "Board name is required.")
		http.Redirect(w, r, "/boards/new", http.StatusSeeOther)
		return
	}

	board, err := app.queries.CreateBoard(ctx, database.CreateBoardParams{
		Name:        name,
		Description: sql.NullString{String: description, Valid: description != ""},
		Position:    0,
	})
	if err != nil {
		setFlash(w, "flash_error", "Failed to create board.")
		http.Redirect(w, r, "/boards/new", http.StatusSeeOther)
		return
	}

	setFlash(w, "flash_success", "Board created successfully!")
	http.Redirect(w, r, "/boards/"+strconv.FormatInt(board.ID, 10), http.StatusSeeOther)
}
