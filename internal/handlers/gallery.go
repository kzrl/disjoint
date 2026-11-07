package handlers

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"disjoint/internal/database"
	"disjoint/internal/middleware"
)

const galleryPageSize = 48

// GalleryImage is one tile on the /gallery wall. The wall shows pixels, not
// text, so there is no snippet or markdown payload here.
type GalleryImage struct {
	AttachmentID int64
	StoragePath  string
	Filename     string
	ThreadID     int64
	PostID       int64
	ThreadTitle  string
	Time         time.Time
	// Reactions and Emojis feed the same reaction_bar partial used by the
	// front page and thread cards; gallery reactions are reactions on the post
	// that carries the image, so they show up everywhere that post does.
	Reactions []ReactionGroup
	Emojis    []database.ListAllEmojisRow
}

// GalleryPage renders every image attachment on a live thread, newest upload
// first. Reply images are included on purpose: the front-page strip only
// previews opening posts, while this is the browsing surface.
func (app *App) GalleryPage(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	rows, err := app.queries.ListGalleryImages(r.Context(), database.ListGalleryImagesParams{
		Limit:  galleryPageSize,
		Offset: int64((page - 1) * galleryPageSize),
	})
	if err != nil {
		http.Error(w, "failed to load gallery", http.StatusInternalServerError)
		return
	}

	items := make([]GalleryImage, 0, len(rows))
	for _, row := range rows {
		t := time.Time{}
		if row.CreatedAt.Valid {
			t = row.CreatedAt.Time
		}
		items = append(items, GalleryImage{
			AttachmentID: row.AttachmentID,
			StoragePath:  row.StoragePath,
			Filename:     row.Filename,
			ThreadID:     row.ThreadID,
			PostID:       row.PostID,
			ThreadTitle:  row.ThreadTitle,
			Time:         t,
		})
	}

	// Per-image reactions for each tile: one query for the whole page, grouped
	// with the same implementation the post bars use.
	if err := app.loadGalleryReactions(r.Context(), items); err != nil {
		http.Error(w, "failed to load gallery reactions", http.StatusInternalServerError)
		return
	}

	// The gallery shares the front page's presence strip, so the online list
	// is loaded here with the same semantics as IndexPage.
	online, err := app.queries.ListOnlineUsers(r.Context())
	if err != nil {
		http.Error(w, "failed to load online users", http.StatusInternalServerError)
		return
	}

	app.render(w, r, "gallery.html", map[string]any{
		"Items":   items,
		"Online":  online,
		"Page":    page,
		"HasNext": len(rows) == galleryPageSize,
		"View":    "gallery",
	}, "Gallery")
}

// loadGalleryReactions fills each item's per-image reaction groups and the
// shared emoji list for its picker.
func (app *App) loadGalleryReactions(ctx context.Context, items []GalleryImage) error {
	if len(items) == 0 {
		return nil
	}
	attachmentIDs := make([]int64, 0, len(items))
	for _, item := range items {
		attachmentIDs = append(attachmentIDs, item.AttachmentID)
	}

	rows, err := app.queries.ListReactionsByAttachmentIDs(ctx, attachmentIDs)
	if err != nil {
		return err
	}
	rRows := make([]reactionRow, 0, len(rows))
	for _, r := range rows {
		rRows = append(rRows, reactionRow{
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

	currentUserID := int64(0)
	if u := middleware.UserFromContext(ctx); u != nil {
		currentUserID = u.ID
	}
	groups := groupReactions(rRows, currentUserID)

	emojis, err := app.queries.ListAllEmojis(ctx)
	if err != nil {
		return err
	}
	for i := range items {
		items[i].Reactions = groups[items[i].AttachmentID]
		items[i].Emojis = emojis
	}
	return nil
}
