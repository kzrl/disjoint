package handlers

import (
	"net/http"
	"strconv"

	"disjoint/internal/database"
	"disjoint/internal/middleware"
)

// ReactionGroup is one (emoji, count) bucket for a single post: every user
// who reacted with the same emoji collapses into one chip.
type ReactionGroup struct {
	EmojiID     int64
	Name        string
	Source      string // 'unicode' | 'custom'
	Glyph       string // unicode emoji only
	StoragePath string // custom emoji only
	Count       int
	You         bool     // the viewing user reacted with this emoji
	Users       []string // usernames of everyone who reacted, for the chip's hover tooltip
}

// ReactionBarData is the context for the reaction_bar.html partial.
type ReactionBarData struct {
	PostID    int64
	Reactions []ReactionGroup
	// Emojis is the full emoji list rendered into the picker. The bar is
	// re-rendered whole after every toggle, so the picker must survive the
	// swap — every bar on the page carries the same list.
	Emojis []database.ListAllEmojisRow
}

// reactionRow is the intermediate shape both reaction queries are mapped to,
// so the bar and the thread page share one grouping implementation.
type reactionRow struct {
	postID      int64
	userID      int64
	emojiID     int64
	name        string
	source      string
	glyph       string
	storagePath string
	username    string // reactant's username, for the chip's hover tooltip
}

func groupReactions(rows []reactionRow, currentUserID int64) map[int64][]ReactionGroup {
	type key struct {
		post  int64
		emoji int64
	}
	idx := make(map[key]int) // key -> index into out
	out := make(map[int64][]ReactionGroup)
	for _, r := range rows {
		k := key{r.postID, r.emojiID}
		i, ok := idx[k]
		if !ok {
			out[r.postID] = append(out[r.postID], ReactionGroup{
				EmojiID:     r.emojiID,
				Name:        r.name,
				Source:      r.source,
				Glyph:       r.glyph,
				StoragePath: r.storagePath,
			})
			i = len(out[r.postID]) - 1
			idx[k] = i
		}
		g := &out[r.postID][i]
		g.Count++
		if r.userID == currentUserID {
			g.You = true
		}
		if r.username != "" {
			g.Users = append(g.Users, r.username)
		}
	}
	return out
}

// ToggleReaction handles POST /posts/{id}/reactions: adds the user's reaction
// for the given emoji, or removes it if the user already reacted (Slack
// toggle semantics). Responds with the post's re-rendered reaction bar for
// HTMX outerHTML swap.
func (app *App) ToggleReaction(w http.ResponseWriter, r *http.Request) {
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

	postID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	emojiID, err := strconv.ParseInt(r.FormValue("emoji_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid emoji", http.StatusBadRequest)
		return
	}

	if _, err := app.queries.GetPostByID(ctx, postID); err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := app.queries.GetEmojiByID(ctx, emojiID); err != nil {
		http.NotFound(w, r)
		return
	}

	added, err := app.queries.AddReaction(ctx, database.AddReactionParams{
		PostID:  postID,
		UserID:  user.ID,
		EmojiID: emojiID,
	})
	if err != nil {
		http.Error(w, "failed to toggle reaction", http.StatusInternalServerError)
		return
	}
	if added == 0 {
		// The (post, user, emoji) triple already existed: this click is a
		// toggle-off.
		if _, err := app.queries.RemoveReaction(ctx, database.RemoveReactionParams{
			PostID:  postID,
			UserID:  user.ID,
			EmojiID: emojiID,
		}); err != nil {
			http.Error(w, "failed to toggle reaction", http.StatusInternalServerError)
			return
		}
	}

	rows, err := app.queries.ListReactionsByPostID(ctx, postID)
	if err != nil {
		http.Error(w, "failed to load reactions", http.StatusInternalServerError)
		return
	}
	var mapped []reactionRow
	for _, r := range rows {
		mapped = append(mapped, reactionRow{
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

	emojis, err := app.queries.ListAllEmojis(ctx)
	if err != nil {
		http.Error(w, "failed to load emojis", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := app.reactionBarTmpl.ExecuteTemplate(w, "reaction_bar", ReactionBarData{
		PostID:    postID,
		Reactions: groupReactions(mapped, user.ID)[postID],
		Emojis:    emojis,
	}); err != nil {
		http.Error(w, "failed to render reaction bar", http.StatusInternalServerError)
	}
}
