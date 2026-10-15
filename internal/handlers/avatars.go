package handlers

import (
	"net/http"

	"disjoint/internal/avatars"
)

// ServeAvatar renders a user's default pixel-art avatar: GET /avatars/{name}.
//
// The route is the single source of truth for a user's displayed avatar:
// when the user has uploaded one it 302s to the /uploads URL, so a link to
// this route keeps showing the current avatar without template branching.
// Otherwise the avatar is generated on the fly from the username (microsecond
// cost, no cache needed) and served with a 1-day public cache; after a later
// upload new requests take the redirect, so at worst a stale copy shows for
// a day.
func (app *App) ServeAvatar(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	u, err := app.queries.GetUserByUsername(r.Context(), name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if u.AvatarPath.Valid && u.AvatarPath.String != "" {
		http.Redirect(w, r, "/uploads/"+u.AvatarPath.String, http.StatusFound)
		return
	}

	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write([]byte(avatars.Render(name, u.AvatarNonce)))
}
