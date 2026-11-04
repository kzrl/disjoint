-- name: FirstUserID :one
SELECT id FROM users ORDER BY id ASC LIMIT 1;

-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: CreateUser :one
INSERT INTO users (
    username,
    password_hash
) VALUES (
    ?, ?
) RETURNING *;

-- name: GetUserByUsername :one
SELECT * FROM users
WHERE username = ? LIMIT 1;

-- name: GetUserByID :one
SELECT * FROM users
WHERE id = ? LIMIT 1;

-- name: ListFrontPageThreads :many
-- Global front page: non-expired threads, newest first (created_at DESC).
-- Last-comment columns describe the thread's most recent reply: the newest
-- post after the opening post (MIN(id) is the opening post, created in the
-- same transaction as the thread). All NULL when the thread has no replies.
SELECT 
    t.id,
    t.title,
    t.created_at,
    t.expires_at,
    t.ttl_seconds,
    COALESCE(u.username, '') AS author_username,
    COUNT(p.id) AS post_count,
    -- Threads always have an opening post (created in the same transaction);
    -- COALESCE + CAST keep the column non-NULL and typed for the scanner.
    CAST(COALESCE((SELECT MIN(p2.id) FROM posts p2 WHERE p2.thread_id = t.id), 0) AS INTEGER) AS first_post_id,
    lc.id AS last_comment_id,
    lc.body AS last_comment_body,
    lc.created_at AS last_comment_at,
    COALESCE(lcu.username, '') AS last_comment_author
FROM threads t
LEFT JOIN users u ON t.user_id = u.id
LEFT JOIN posts p ON p.thread_id = t.id
LEFT JOIN posts lc ON lc.id = (
    SELECT MAX(p2.id)
    FROM posts p2
    WHERE p2.thread_id = t.id
      AND p2.id > (SELECT MIN(p3.id) FROM posts p3 WHERE p3.thread_id = t.id)
)
LEFT JOIN users lcu ON lc.user_id = lcu.id
WHERE t.expires_at IS NULL OR t.expires_at > CURRENT_TIMESTAMP
GROUP BY t.id
ORDER BY t.created_at DESC, t.id DESC
LIMIT ? OFFSET ?;

-- name: SearchFrontPageThreads :many
-- Front-page search. Same row shape and ordering as ListFrontPageThreads,
-- filtered to non-expired threads whose title or any post body matches the
-- caller-supplied LIKE pattern (wrapped in %...%; % and _ keep their wildcard
-- meaning in a search term).
SELECT
    t.id,
    t.title,
    t.created_at,
    t.expires_at,
    t.ttl_seconds,
    COALESCE(u.username, '') AS author_username,
    COUNT(p.id) AS post_count,
    CAST(COALESCE((SELECT MIN(p2.id) FROM posts p2 WHERE p2.thread_id = t.id), 0) AS INTEGER) AS first_post_id,
    lc.id AS last_comment_id,
    lc.body AS last_comment_body,
    lc.created_at AS last_comment_at,
    COALESCE(lcu.username, '') AS last_comment_author
FROM threads t
LEFT JOIN users u ON t.user_id = u.id
LEFT JOIN posts p ON p.thread_id = t.id
LEFT JOIN posts lc ON lc.id = (
    SELECT MAX(p2.id)
    FROM posts p2
    WHERE p2.thread_id = t.id
      AND p2.id > (SELECT MIN(p3.id) FROM posts p3 WHERE p3.thread_id = t.id)
)
LEFT JOIN users lcu ON lc.user_id = lcu.id
WHERE (t.expires_at IS NULL OR t.expires_at > CURRENT_TIMESTAMP)
  AND (
    t.title LIKE ?
    OR EXISTS (
      SELECT 1 FROM posts sp
      WHERE sp.thread_id = t.id
        AND sp.body LIKE ?
    )
  )
GROUP BY t.id
ORDER BY t.created_at DESC, t.id DESC
LIMIT ? OFFSET ?;

-- name: ListFrontPageThreadImages :many
-- Front-page image strips: image attachments on the listed opening posts.
-- One round trip per page (same batching pattern as ListReactionsByPostIDs);
-- the caller must not invoke this with an empty postIds slice.
SELECT
    p.thread_id,
    a.id,
    a.storage_path,
    a.filename
FROM attachments a
JOIN posts p ON a.post_id = p.id
WHERE p.id IN (sqlc.slice('postIds'))
  AND (a.mime_type = 'image/jpeg' OR a.mime_type = 'image/png'
       OR a.mime_type = 'image/webp' OR a.mime_type = 'image/gif')
ORDER BY a.id ASC;

-- name: GetThreadByID :one
SELECT 
    t.id,
    t.user_id,
    t.title,
    t.created_at,
    t.updated_at,
    t.expires_at,
    t.ttl_seconds,
    COALESCE(u.username, '') AS author_username,
    COALESCE(u.avatar_path, '') AS author_avatar,
    COALESCE(u.avatar_nonce, 0) AS author_avatar_nonce
FROM threads t
LEFT JOIN users u ON t.user_id = u.id
WHERE t.id = ? AND (t.expires_at IS NULL OR t.expires_at > CURRENT_TIMESTAMP)
LIMIT 1;

-- name: CreateThread :one
INSERT INTO threads (
    user_id,
    title,
    expires_at,
    ttl_seconds
) VALUES (
    ?, ?, ?, ?
) RETURNING *;

-- name: BumpThreadExpiration :exec
-- Called after every new post: advances updated_at and, for TTL threads,
-- pushes the expiry deadline out to last-comment + ttl (computed in Go).
-- Never-threads pass a NULL expires_at and stay NULL.
UPDATE threads
SET updated_at = CURRENT_TIMESTAMP,
    expires_at = ?
WHERE id = ?;

-- name: SetThreadExpiration :execrows
-- Author-only lifespan change. expires_at is reset to the caller-computed
-- value (now + ttl, or NULL for never-threads) - the same "ttl of silence
-- from now" rule every reply applies. Ownership is enforced in the WHERE
-- clause (0 rows = not found or not the author). updated_at is
-- deliberately untouched: a settings change is not a comment.
UPDATE threads
SET ttl_seconds = ?, expires_at = ?
WHERE id = ? AND user_id = ?;

-- name: DeleteThread :exec
DELETE FROM threads
WHERE id = ?;

-- name: GetAttachmentsForExpiredThreads :many
SELECT a.id, a.storage_path
FROM attachments a
JOIN posts p ON a.post_id = p.id
JOIN threads t ON p.thread_id = t.id
WHERE t.expires_at IS NOT NULL AND t.expires_at < CURRENT_TIMESTAMP;

-- name: DeleteExpiredThreads :exec
DELETE FROM threads
WHERE expires_at IS NOT NULL AND expires_at < CURRENT_TIMESTAMP;

-- name: ListPostsByThread :many
SELECT 
    p.id,
    p.thread_id,
    p.user_id,
    p.body,
    COALESCE(p.url, '') AS url,
    p.created_at,
    p.updated_at,
    COALESCE(u.username, '') AS author_username,
    COALESCE(u.avatar_path, '') AS author_avatar,
    COALESCE(u.avatar_nonce, 0) AS author_avatar_nonce
FROM posts p
LEFT JOIN users u ON p.user_id = u.id
WHERE p.thread_id = ?
ORDER BY p.created_at ASC, p.id ASC;

-- name: GetPostByID :one
SELECT 
    p.id,
    p.thread_id,
    p.user_id,
    p.body,
    COALESCE(p.url, '') AS url,
    p.created_at,
    p.updated_at,
    COALESCE(u.username, '') AS author_username,
    COALESCE(u.avatar_path, '') AS author_avatar,
    COALESCE(u.avatar_nonce, 0) AS author_avatar_nonce
FROM posts p
LEFT JOIN users u ON p.user_id = u.id
WHERE p.id = ? LIMIT 1;

-- name: CreatePost :one
-- url is the optional first-class link (NULL when absent); it is stored
-- verbatim after server-side validation and never parsed as markdown.
INSERT INTO posts (
    thread_id,
    user_id,
    body,
    url
) VALUES (
    ?, ?, ?, ?
) RETURNING *;

-- name: UpdatePost :execrows
-- Author-only edit of body and the first-class link. Ownership is enforced
-- in the WHERE clause: 0 rows means the post does not exist OR the caller
-- is not the author (the handler returns 404 for both). updated_at moves
-- to CURRENT_TIMESTAMP on every successful edit (NULL = never edited);
-- created_at is never touched.
UPDATE posts
SET body = ?, url = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ? AND user_id = ?;

-- name: UpdateThreadTitle :execrows
-- Author-only thread rename, same WHERE-clause ownership rule as UpdatePost
-- (0 rows = not found or not the author). threads.updated_at is bumped; the
-- expiry deadline (expires_at) is deliberately left alone.
UPDATE threads
SET title = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ? AND user_id = ?;

-- name: CreateAttachment :one
INSERT INTO attachments (
    post_id,
    filename,
    storage_path,
    mime_type,
    file_size
) VALUES (
    ?, ?, ?, ?, ?
) RETURNING *;

-- name: ListAttachmentsByThreadID :many
SELECT a.*
FROM attachments a
JOIN posts p ON a.post_id = p.id
WHERE p.thread_id = ?
ORDER BY a.id ASC;

-- name: CountAttachmentsByPostID :one
-- Used by the edit handler: a post that carries attachments may be saved
-- with an empty body and link (the attachments still satisfy the content
-- rule, and attachments are immutable on edit).
SELECT COUNT(*) FROM attachments WHERE post_id = ?;

-- name: SetUserAdmin :exec
UPDATE users SET is_admin = 1 WHERE id = ?;

-- name: RemoveUserAdmin :exec
UPDATE users SET is_admin = 0 WHERE id = ?;

-- name: DeleteUser :exec
DELETE FROM users
WHERE id = ?;

-- name: ListUsers :many
SELECT id, username, is_admin, created_at FROM users ORDER BY id ASC;

-- name: CreateInvite :one
INSERT INTO invites (code, created_by) VALUES (?, ?) RETURNING *;

-- name: GetInviteByCode :one
SELECT * FROM invites WHERE code = ? AND used_by IS NULL LIMIT 1;

-- name: ConsumeInvite :execrows
-- Returns the number of rows updated: 0 means the invite was already
-- used (or deleted) by a concurrent request.
UPDATE invites SET used_by = ?, used_at = CURRENT_TIMESTAMP WHERE code = ? AND used_by IS NULL;

-- name: ListInvites :many
SELECT
    i.id,
    i.code,
    i.created_by,
    i.used_by,
    i.created_at,
    i.used_at,
    cu.username AS creator_username,
    uu.username AS used_by_username
FROM invites i
JOIN users cu ON i.created_by = cu.id
LEFT JOIN users uu ON i.used_by = uu.id
ORDER BY i.created_at DESC;

-- name: DeleteUnusedInvite :exec
DELETE FROM invites WHERE id = ? AND used_by IS NULL;

-- name: MarkThreadRead :exec
-- Upsert the viewer's read marker for a thread. Called on every logged-in
-- thread page view; the front page compares posts.created_at against it.
INSERT INTO thread_reads (user_id, thread_id, last_read_at)
VALUES (?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(user_id, thread_id) DO UPDATE SET last_read_at = CURRENT_TIMESTAMP;

-- name: ListUnreadThreadIDs :many
-- Thread IDs with at least one post the viewer has not seen. "Seen" is the
-- read marker, falling back to the account creation time before the first
-- open (so a new account does not inherit the whole archive as unread).
-- The viewer's own posts never count as unread.
SELECT DISTINCT p.thread_id
FROM posts p
LEFT JOIN thread_reads r
    ON r.thread_id = p.thread_id AND r.user_id = sqlc.arg('viewer_id')
WHERE p.user_id <> sqlc.arg('viewer_id')
  AND p.created_at > COALESCE(r.last_read_at, sqlc.arg('account_created_at'));

-- name: UpsertUserPresence :exec
-- Throttled in SQL: the upsert only writes if the stored timestamp is more
-- than 30 seconds old, so the middleware can call this on every request
-- without amplifying writes.
INSERT INTO user_presence (user_id, last_seen_at)
VALUES (?, CURRENT_TIMESTAMP)
ON CONFLICT(user_id) DO UPDATE SET last_seen_at = CURRENT_TIMESTAMP
WHERE user_presence.last_seen_at < datetime('now', '-30 seconds');

-- name: ListOnlineUsers :many
SELECT
    u.id,
    u.username,
    p.last_seen_at
FROM user_presence p
JOIN users u ON u.id = p.user_id
WHERE p.last_seen_at >= datetime('now', '-5 minutes')
ORDER BY p.last_seen_at DESC;

-- name: GetUserPresence :one
SELECT last_seen_at FROM user_presence WHERE user_id = ? LIMIT 1;

-- name: CountPostsByUserID :one
SELECT COUNT(*) FROM posts WHERE user_id = ?;

-- name: UpdateUserBio :exec
UPDATE users SET bio = ? WHERE id = ?;

-- name: SetUserAvatar :exec
UPDATE users SET avatar_path = ? WHERE id = ?;

-- name: RemoveUserAvatar :exec
UPDATE users SET avatar_path = NULL WHERE id = ?;

-- name: SetUserAvatarNonce :exec
UPDATE users SET avatar_nonce = ? WHERE id = ?;

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?);

-- name: GetSessionUserID :one
SELECT user_id FROM sessions WHERE token_hash = ? AND expires_at > ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = ?;

-- name: DeleteSessionsForUser :exec
DELETE FROM sessions WHERE user_id = ?;

-- name: PruneExpiredSessions :exec
DELETE FROM sessions WHERE expires_at <= ?;

-- name: ListAllEmojis :many
-- Every available emoji (built-in unicode + custom uploads), stable
-- name order for the reaction picker. The uploader's username (not just the
-- ID) is joined in so the emoji page can attribute custom uploads; empty
-- when the uploader was deleted (created_by is SET NULL).
SELECT
    e.id,
    e.name,
    e.source,
    COALESCE(e.glyph, '') AS glyph,
    COALESCE(e.storage_path, '') AS storage_path,
    COALESCE(e.mime_type, '') AS mime_type,
    CAST(COALESCE(e.file_size, 0) AS INTEGER) AS file_size,
    COALESCE(uc.username, '') AS created_by_username
FROM emojis e
LEFT JOIN users uc ON e.created_by = uc.id
ORDER BY e.name ASC;

-- name: GetEmojiByID :one
SELECT * FROM emojis WHERE id = ? LIMIT 1;

-- name: GetEmojiByName :one
SELECT * FROM emojis WHERE name = ? LIMIT 1;

-- name: CreateEmoji :one
INSERT INTO emojis (
    name,
    source,
    glyph,
    storage_path,
    mime_type,
    file_size,
    created_by
) VALUES (
    ?, ?, ?, ?, ?, ?, ?
) RETURNING *;

-- name: DeleteEmoji :exec
DELETE FROM emojis WHERE id = ?;

-- name: AddReaction :execrows
-- Returns the number of rows inserted: 0 means this (post, user, emoji)
-- combination already exists (the UNIQUE constraint), i.e. the click is a
-- toggle-off. Concurrent toggles are safe: one request wins the insert, the
-- other's insert is ignored and it deletes.
INSERT OR IGNORE INTO reactions (post_id, user_id, emoji_id) VALUES (?, ?, ?);

-- name: RemoveReaction :execrows
DELETE FROM reactions WHERE post_id = ? AND user_id = ? AND emoji_id = ?;

-- name: ListReactionsByPostID :many
-- Reaction rows for a single post with emoji details, for rendering the
-- post's reaction bar after a toggle.
SELECT
    r.post_id,
    r.user_id,
    r.emoji_id,
    r.created_at,
    e.name AS emoji_name,
    e.source AS emoji_source,
    COALESCE(e.glyph, '') AS emoji_glyph,
    COALESCE(e.storage_path, '') AS emoji_storage_path,
    COALESCE(e.mime_type, '') AS emoji_mime_type,
    CAST(COALESCE(e.file_size, 0) AS INTEGER) AS emoji_file_size,
    COALESCE(u.username, '') AS username
FROM reactions r
JOIN emojis e ON r.emoji_id = e.id
LEFT JOIN users u ON r.user_id = u.id
WHERE r.post_id = ?
ORDER BY r.emoji_id ASC, r.id ASC;

-- name: ListReactionsByThreadID :many
-- All reaction rows for a thread's posts, joined with their emoji so the
-- handler can group counts per post in Go without a second query.
SELECT
    r.post_id,
    r.user_id,
    r.emoji_id,
    r.created_at,
    e.name AS emoji_name,
    e.source AS emoji_source,
    COALESCE(e.glyph, '') AS emoji_glyph,
    COALESCE(e.storage_path, '') AS emoji_storage_path,
    COALESCE(e.mime_type, '') AS emoji_mime_type,
    CAST(COALESCE(e.file_size, 0) AS INTEGER) AS emoji_file_size,
    COALESCE(u.username, '') AS username
FROM reactions r
JOIN emojis e ON r.emoji_id = e.id
JOIN posts p ON r.post_id = p.id
LEFT JOIN users u ON r.user_id = u.id
WHERE p.thread_id = ?
ORDER BY r.post_id ASC, r.emoji_id ASC, r.id ASC;

-- name: ListReactionsByPostIDs :many
-- Reaction rows for a set of posts in one round trip. The front page
-- shows each thread's opening-post reactions for a whole page at once.
SELECT
    r.post_id,
    r.user_id,
    r.emoji_id,
    r.created_at,
    e.name AS emoji_name,
    e.source AS emoji_source,
    COALESCE(e.glyph, '') AS emoji_glyph,
    COALESCE(e.storage_path, '') AS emoji_storage_path,
    COALESCE(e.mime_type, '') AS emoji_mime_type,
    CAST(COALESCE(e.file_size, 0) AS INTEGER) AS emoji_file_size,
    COALESCE(u.username, '') AS username
FROM reactions r
JOIN emojis e ON r.emoji_id = e.id
LEFT JOIN users u ON r.user_id = u.id
WHERE r.post_id IN (sqlc.slice('postIds'))
ORDER BY r.post_id ASC, r.emoji_id ASC, r.id ASC;

-- name: UpsertNotification :exec
-- Record or bump one inbox entry. A new trigger makes the row unread again,
-- points it at the newest triggering post, and upgrades 'reply' to 'mention'
-- (never the reverse). Called inside the post-creation transaction so a post
-- and its notifications commit together.
INSERT INTO notifications (user_id, thread_id, post_id, reason)
VALUES (sqlc.arg('user_id'), sqlc.arg('thread_id'), sqlc.arg('post_id'), sqlc.arg('reason'))
ON CONFLICT(user_id, thread_id) DO UPDATE SET
    post_id       = excluded.post_id,
    reason        = CASE WHEN excluded.reason = 'mention' THEN 'mention' ELSE notifications.reason END,
    last_event_at = CURRENT_TIMESTAMP,
    read_at       = NULL;

-- name: ListInbox :many
-- The viewer's inbox: one row per thread, newest activity first. The actor is
-- the author of the triggering post (there is no denormalized actor column),
-- and expired-but-not-yet-purged threads are filtered out so the inbox never
-- links to a thread that already 404s.
SELECT
    n.thread_id,
    n.post_id,
    n.reason,
    n.last_event_at,
    n.read_at,
    t.title,
    p.body,
    COALESCE(p.url, '') AS url,
    COALESCE(u.username, '') AS actor_username,
    COALESCE(u.avatar_path, '') AS actor_avatar,
    CAST(COALESCE(u.avatar_nonce, 0) AS INTEGER) AS actor_avatar_nonce
FROM notifications n
JOIN threads t ON t.id = n.thread_id
JOIN posts p   ON p.id = n.post_id
LEFT JOIN users u ON u.id = p.user_id
WHERE n.user_id = sqlc.arg('user_id')
  AND (t.expires_at IS NULL OR t.expires_at >= CURRENT_TIMESTAMP)
ORDER BY n.last_event_at DESC, n.thread_id DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountUnreadNotifications :one
-- Navbar badge count. Expired-but-unpurged threads are excluded: the cleanup
-- worker only runs once a minute, and the badge must not advertise a thread
-- that already 404s. Both joins are on primary keys, so this stays cheap.
SELECT COUNT(*)
FROM notifications n
JOIN threads t ON t.id = n.thread_id
WHERE n.user_id = sqlc.arg('user_id')
  AND n.read_at IS NULL
  AND (t.expires_at IS NULL OR t.expires_at >= CURRENT_TIMESTAMP);

-- name: MarkAllNotificationsRead :exec
-- Backs the inbox's "Mark all read" button (POST /inbox/read). Nothing marks
-- notifications read implicitly except opening the thread; the inbox itself
-- never clears on view.
UPDATE notifications SET read_at = CURRENT_TIMESTAMP
WHERE user_id = sqlc.arg('user_id') AND read_at IS NULL;

-- name: MarkThreadNotificationRead :exec
-- Read-through: opening a thread counts as dealing with its inbox entry.
UPDATE notifications SET read_at = CURRENT_TIMESTAMP
WHERE user_id = sqlc.arg('user_id')
  AND thread_id = sqlc.arg('thread_id')
  AND read_at IS NULL;

-- name: ListThreadRecipients :many
-- Everyone who has posted in the thread. The opening post means the thread's
-- author is always included; the caller removes the actor.
SELECT DISTINCT user_id FROM posts WHERE thread_id = sqlc.arg('thread_id');

-- name: ListUserIDsByUsernames :many
-- Resolve mentioned names to real users in one round trip. Usernames are
-- case-sensitive everywhere else (login, user cards), so membership is exact;
-- an unknown mention notifies nobody and still renders as a 404 pill.
SELECT id, username FROM users
WHERE username IN (sqlc.slice('usernames'));

-- name: NewPostsAfter :one
-- The "N new replies" pill on a thread page. Counts other users' posts newer
-- than the viewer's last rendered post (the viewer's own posts are already in
-- their DOM, so counting them would offer a reload that shows nothing new).
-- first_new_id is the anchor to land on after the reload; 0 when the count is 0.
SELECT COUNT(*) AS new_count,
       CAST(COALESCE(MIN(p.id), 0) AS INTEGER) AS first_new_id
FROM posts p
WHERE p.thread_id = sqlc.arg('thread_id')
  AND p.id > sqlc.arg('after')
  AND p.user_id <> sqlc.arg('viewer_id');
