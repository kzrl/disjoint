-- +goose Up
-- +goose NO TRANSACTION
-- Make threads.user_id and posts.user_id nullable so anonymous content is
-- stored with NO link to its author. SQLite cannot loosen a NOT NULL
-- constraint in place, so each table is rebuilt. foreign_keys is toggled
-- off for the rebuild: with it on, DROP TABLE threads would cascade-delete
-- every row in posts (and DROP TABLE posts would cascade-delete every row
-- in attachments) before the copies below run. The PRAGMA is a no-op
-- inside a transaction, so this migration must run WITHOUT one (NO
-- TRANSACTION); the cost is that a failure mid-migration is not rolled
-- back.
PRAGMA foreign_keys = OFF;

CREATE TABLE threads_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    board_id INTEGER NOT NULL,
    user_id INTEGER, -- NULL for anonymous threads (author cannot be determined)
    title TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME, -- NULL means no expiration; otherwise, TTL/Hard expiry target
    FOREIGN KEY (board_id) REFERENCES boards(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

INSERT INTO threads_new (id, board_id, user_id, title, created_at, updated_at, expires_at)
    SELECT id, board_id, user_id, title, created_at, updated_at, expires_at FROM threads;

DROP TABLE threads;
ALTER TABLE threads_new RENAME TO threads;
CREATE INDEX idx_threads_board_id ON threads(board_id);
CREATE INDEX idx_threads_updated_at ON threads(updated_at DESC);
CREATE INDEX idx_threads_expires_at ON threads(expires_at);

CREATE TABLE posts_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_id INTEGER NOT NULL,
    user_id INTEGER, -- NULL for anonymous posts (author cannot be determined)
    body TEXT NOT NULL, -- Stored as raw text/Markdown
    is_anonymous INTEGER NOT NULL DEFAULT 0, -- Display as "Anonymous" instead of username
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

INSERT INTO posts_new (id, thread_id, user_id, body, is_anonymous, created_at)
    SELECT id, thread_id, user_id, body, is_anonymous, created_at FROM posts;

DROP TABLE posts;
ALTER TABLE posts_new RENAME TO posts;
CREATE INDEX idx_posts_thread_id ON posts(thread_id);

-- Sever the link for existing anonymous content.
UPDATE posts SET user_id = NULL WHERE is_anonymous = 1;
UPDATE threads SET user_id = NULL
WHERE id IN (
    SELECT p.thread_id
    FROM posts p
    WHERE p.is_anonymous = 1
      AND p.id = (SELECT MIN(p2.id) FROM posts p2 WHERE p2.thread_id = p.thread_id)
);

PRAGMA foreign_keys = ON;

-- +goose Down
-- Best effort: the anonymous-author link was destroyed and cannot be
-- restored on downgrade; NULL authors are re-linked to user 1 as a
-- placeholder so the NOT NULL constraint holds.
PRAGMA foreign_keys = OFF;

CREATE TABLE threads_old (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    board_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    title TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME,
    FOREIGN KEY (board_id) REFERENCES boards(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

INSERT INTO threads_old (id, board_id, user_id, title, created_at, updated_at, expires_at)
    SELECT id, board_id, COALESCE(user_id, 1), title, created_at, updated_at, expires_at FROM threads;

DROP TABLE threads;
ALTER TABLE threads_old RENAME TO threads;
CREATE INDEX idx_threads_board_id ON threads(board_id);
CREATE INDEX idx_threads_updated_at ON threads(updated_at DESC);
CREATE INDEX idx_threads_expires_at ON threads(expires_at);

CREATE TABLE posts_old (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    body TEXT NOT NULL,
    is_anonymous INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

INSERT INTO posts_old (id, thread_id, user_id, body, is_anonymous, created_at)
    SELECT id, thread_id, COALESCE(user_id, 1), body, is_anonymous, created_at FROM posts;

DROP TABLE posts;
ALTER TABLE posts_old RENAME TO posts;
CREATE INDEX idx_posts_thread_id ON posts(thread_id);

PRAGMA foreign_keys = ON;
