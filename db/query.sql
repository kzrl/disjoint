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

-- name: ListBoards :many
SELECT * FROM boards
ORDER BY position ASC, id ASC;

-- name: GetBoardByID :one
SELECT * FROM boards
WHERE id = ? LIMIT 1;

-- name: CreateBoard :one
INSERT INTO boards (
    name,
    description,
    position
) VALUES (
    ?, ?, ?
) RETURNING *;

-- name: ListBoardsWithDetails :many
SELECT 
    b.id,
    b.name,
    b.description,
    b.position,
    COUNT(DISTINCT t.id) AS thread_count,
    COUNT(p.id) AS post_count,
    CAST(COALESCE(MAX(t.updated_at), '') AS TEXT) AS latest_updated_at
FROM boards b
LEFT JOIN threads t ON t.board_id = b.id AND (t.expires_at IS NULL OR t.expires_at > CURRENT_TIMESTAMP)
LEFT JOIN posts p ON p.thread_id = t.id
GROUP BY b.id
ORDER BY b.position ASC, b.id ASC;

-- name: ListLatestThreadsByBoard :many
-- One row per board: the most recently updated non-expired thread.
-- Replaces a per-board GetLatestThreadForBoard round trip (N+1).
SELECT 
    t.id,
    t.board_id,
    t.title,
    t.created_at,
    t.updated_at,
    COALESCE(u.username, '') AS author_username
FROM (
    SELECT 
        id, board_id, title, created_at, updated_at, user_id,
        ROW_NUMBER() OVER (
            PARTITION BY board_id
            ORDER BY updated_at DESC, id DESC
        ) AS rn
    FROM threads
    WHERE expires_at IS NULL OR expires_at > CURRENT_TIMESTAMP
) t
LEFT JOIN users u ON t.user_id = u.id
WHERE t.rn = 1
ORDER BY t.board_id ASC;

-- name: ListThreadsByBoard :many
SELECT 
    t.id,
    t.board_id,
    t.user_id,
    t.title,
    t.created_at,
    t.updated_at,
    t.expires_at,
    COALESCE(u.username, '') AS author_username,
    COUNT(p.id) AS post_count
FROM threads t
LEFT JOIN users u ON t.user_id = u.id
LEFT JOIN posts p ON p.thread_id = t.id
WHERE t.board_id = ? AND (t.expires_at IS NULL OR t.expires_at > CURRENT_TIMESTAMP)
GROUP BY t.id
ORDER BY t.updated_at DESC;

-- name: GetThreadByID :one
SELECT 
    t.id,
    t.board_id,
    t.user_id,
    t.title,
    t.created_at,
    t.updated_at,
    t.expires_at,
    COALESCE(u.username, '') AS author_username,
    COALESCE(u.avatar_path, '') AS author_avatar,
    COALESCE(u.avatar_nonce, 0) AS author_avatar_nonce,
    b.name AS board_name
FROM threads t
LEFT JOIN users u ON t.user_id = u.id
JOIN boards b ON t.board_id = b.id
WHERE t.id = ? AND (t.expires_at IS NULL OR t.expires_at > CURRENT_TIMESTAMP)
LIMIT 1;

-- name: CreateThread :one
INSERT INTO threads (
    board_id,
    user_id,
    title,
    expires_at
) VALUES (
    ?, ?, ?, ?
) RETURNING *;

-- name: TouchThread :exec
UPDATE threads
SET updated_at = CURRENT_TIMESTAMP
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

-- name: UpsertUserBoardRead :exec
INSERT INTO user_board_reads (user_id, board_id, last_read_at)
VALUES (?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(user_id, board_id) DO UPDATE SET last_read_at = CURRENT_TIMESTAMP;

-- name: GetUserBoardRead :one
SELECT last_read_at
FROM user_board_reads
WHERE user_id = ? AND board_id = ? LIMIT 1;

-- name: ListUserBoardReads :many
SELECT board_id, last_read_at
FROM user_board_reads
WHERE user_id = ?;

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

-- name: ListRecentActivity :many
SELECT
    p.id,
    p.created_at,
    COALESCE(u.username, '') AS author_username,
    t.id AS thread_id,
    t.title AS thread_title,
    t.board_id AS board_id,
    b.name AS board_name
FROM posts p
LEFT JOIN users u ON p.user_id = u.id
JOIN threads t ON p.thread_id = t.id
JOIN boards b ON t.board_id = b.id
WHERE t.expires_at IS NULL OR t.expires_at > CURRENT_TIMESTAMP
ORDER BY p.created_at DESC, p.id DESC
LIMIT ?;

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
