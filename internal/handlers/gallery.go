package handlers

import (
	"net/http"
	"strconv"
	"time"

	"disjoint/internal/database"
)

const galleryPageSize = 48

// GalleryImage is one tile on the /gallery wall. The wall shows pixels, not
// text, so there is no snippet or markdown payload here.
type GalleryImage struct {
	StoragePath string
	Filename    string
	ThreadID    int64
	PostID      int64
	ThreadTitle string
	Time        time.Time
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
			StoragePath: row.StoragePath,
			Filename:    row.Filename,
			ThreadID:    row.ThreadID,
			PostID:      row.PostID,
			ThreadTitle: row.ThreadTitle,
			Time:        t,
		})
	}

	app.render(w, r, "gallery.html", map[string]any{
		"Items":   items,
		"Page":    page,
		"HasNext": len(rows) == galleryPageSize,
	}, "Gallery")
}
