package handlers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"disjoint/internal/middleware"
)

const maxMentionQueryLength = 32

// MentionCandidate is one row in the autocomplete dropdown. Avatar is a
// /avatars/{name}?v={nonce} URL: that route serves the uploaded image when one
// exists and otherwise renders the deterministic default avatar.
type MentionCandidate struct {
	Name   string `json:"name"`
	Avatar string `json:"avatar"`
}

type mentionSuggestionsResponse struct {
	Users []MentionCandidate `json:"users"`
}

// MentionSuggestions returns up to 10 username prefix matches for the compose
// autocomplete. It is authenticated by the global middleware; unknown users
// are legal mentions in the renderer, so an empty result is not an error.
func (app *App) MentionSuggestions(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !allowUser(app.apiLimiter, user.ID) {
		w.Header().Set("Retry-After", "300")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > maxMentionQueryLength {
		http.Error(w, "query is too long", http.StatusBadRequest)
		return
	}

	pattern := escapeLikePattern(q) + "%"
	rows, err := app.queries.ListMentionCandidates(r.Context(), pattern)
	if err != nil {
		http.Error(w, "failed to load mention suggestions", http.StatusInternalServerError)
		return
	}

	users := make([]MentionCandidate, 0, len(rows))
	for _, row := range rows {
		users = append(users, MentionCandidate{
			Name: row.Username,
			Avatar: "/avatars/" + url.PathEscape(row.Username) +
				"?v=" + strconv.FormatInt(row.AvatarNonce, 10),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(mentionSuggestionsResponse{Users: users})
}

// escapeLikePattern escapes the three metacharacters of SQLite LIKE. The SQL
// explicitly uses ESCAPE '\'; the two must stay in sync.
func escapeLikePattern(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(s)
}
