-- +goose Up
-- +goose NO TRANSACTION
-- Remove anonymous posting entirely (feature retired):
--   1. Purge legacy anonymous content: posts/threads stored with
--      user_id IS NULL since 00003, plus their attachment rows.
--      IMPORTANT: the files on disk are NOT deleted by this migration.
--      Run tools/cleanup_anonymous (dry-run, then --yes) BEFORE first
--      starting the app with this migration, or the files are orphaned.
--   2. Rebuild threads and posts with user_id NOT NULL, and drop the
--      now-redundant posts.is_anonymous column (since 00003, anonymous
--      was exactly "user_id IS NULL").
-- IRREVERSIBLE: purged rows cannot be restored.
-- SQLite cannot tighten NOT NULL in place, so each table is rebuilt.
-- foreign_keys is toggled off for the rebuilds (as in 00003); the
-- PRAGMA is a no-op inside a transaction, so this migration must run
-- WITHOUT one (NO TRANSACTION). A failure mid-migration is not rolled
-- back.
PRAGMA foreign_keys = OFF;

DELETE FROM attachments
WHERE post_id IN (
    SELECT id FROM posts
    WHERE user_id IS NULL
       OR thread_id IN (SELECT id FROM threads WHERE user_id IS NULL));
DELETE FROM posts
WHERE user_id IS NULL
   OR thread_id IN (SELECT id FROM threads WHERE user_id IS NULL);
DELETE FROM threads WHERE user_id IS NULL;

CREATE TABLE threads_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    board_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
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
    user_id INTEGER NOT NULL,
    body TEXT NOT NULL, -- Stored as raw text/Markdown
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

INSERT INTO posts_new (id, thread_id, user_id, body, created_at)
    SELECT id, thread_id, user_id, body, created_at FROM posts;

DROP TABLE posts;
ALTER TABLE posts_new RENAME TO posts;
CREATE INDEX idx_posts_thread_id ON posts(thread_id);

PRAGMA foreign_keys = ON;

-- +goose Down
-- Best effort: the purged anonymous content was destroyed and cannot be
-- restored. Re-add the nullable user_id and the is_anonymous flag.
PRAGMA foreign_keys = OFF;

CREATE TABLE threads_old (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    board_id INTEGER NOT NULL,
    user_id INTEGER,
    title TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME,
    FOREIGN KEY (board_id) REFERENCES boards(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

INSERT INTO threads_old (id, board_id, user_id, title, created_at, updated_at, expires_at)
    SELECT id, board_id, user_id, title, created_at, updated_at, expires_at FROM threads;

DROP TABLE threads;
ALTER TABLE threads_old RENAME TO threads;
CREATE INDEX idx_threads_board_id ON threads(board_id);
CREATE INDEX idx_threads_updated_at ON threads(updated_at DESC);
CREATE INDEX idx_threads_expires_at ON threads(expires_at);

CREATE TABLE posts_old (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_id INTEGER NOT NULL,
    user_id INTEGER,
    body TEXT NOT NULL,
    is_anonymous INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

INSERT INTO posts_old (id, thread_id, user_id, body, created_at)
    SELECT id, thread_id, user_id, body, created_at FROM posts;

DROP TABLE posts;
ALTER TABLE posts_old RENAME TO posts;
CREATE INDEX idx_posts_thread_id ON posts(thread_id);

PRAGMA foreign_keys = ON;
