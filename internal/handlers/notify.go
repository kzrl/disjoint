package handlers

import (
	"context"
	"sort"

	"disjoint/internal/database"
	"disjoint/internal/markdown"
)

// maxMentionNotifications bounds how many mentioned users one post can notify.
// A body stuffed with hundreds of mentions must not write hundreds of rows or
// build an oversized IN list; the first names win (first-appearance order).
const maxMentionNotifications = 20

// notifyPostRecipients records inbox entries for one freshly created post.
// Recipients are (thread participants ∪ mentioned users) − actor, with reason
// "mention" for mentioned users and "reply" for everyone else. Mention names
// come from markdown.Mentions, i.e. the same goldmark AST the renderer uses,
// so a user is notified exactly when their name would render as a pill.
//
// It must run inside the post-creation transaction (qtx) so the post and its
// notifications commit together, matching how attachments are saved.
func (app *App) notifyPostRecipients(ctx context.Context, qtx *database.Queries, threadID, postID, actorID int64, body string) error {
	reasons := make(map[int64]string)

	// 1. Mentioned users win the reason ("mention" is never downgraded).
	names := markdown.Mentions(body)
	if len(names) > maxMentionNotifications {
		names = names[:maxMentionNotifications]
	}
	if len(names) > 0 {
		// Never pass an empty slice: "IN ()" is a syntax error.
		users, err := qtx.ListUserIDsByUsernames(ctx, names)
		if err != nil {
			return err
		}
		for _, u := range users {
			if u.ID != actorID {
				reasons[u.ID] = "mention"
			}
		}
	}

	// 2. Everyone who has posted in the thread (the opening post means the
	// author is always included) is notified of the reply.
	recipients, err := qtx.ListThreadRecipients(ctx, threadID)
	if err != nil {
		return err
	}
	for _, id := range recipients {
		if id == actorID {
			continue
		}
		if _, already := reasons[id]; !already {
			reasons[id] = "reply"
		}
	}

	// Sorted for deterministic writes: map iteration order is random, and
	// stable row order keeps test failures readable.
	ids := make([]int64, 0, len(reasons))
	for id := range reasons {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		if err := qtx.UpsertNotification(ctx, database.UpsertNotificationParams{
			UserID:   id,
			ThreadID: threadID,
			PostID:   postID,
			Reason:   reasons[id],
		}); err != nil {
			return err
		}
	}
	return nil
}
