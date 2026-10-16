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
    p.created_at,
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
    p.created_at,
    COALESCE(u.username, '') AS author_username,
    COALESCE(u.avatar_path, '') AS author_avatar,
    COALESCE(u.avatar_nonce, 0) AS author_avatar_nonce
FROM posts p
LEFT JOIN users u ON p.user_id = u.id
WHERE p.id = ? LIMIT 1;

-- name: CreatePost :one
INSERT INTO posts (
    thread_id,
    user_id,
    body
) VALUES (
    ?, ?, ?
) RETURNING *;

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
