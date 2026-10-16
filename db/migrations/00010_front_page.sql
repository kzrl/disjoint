-- +goose Up
-- +goose NO TRANSACTION
-- Replace the board-based forum with a global front page (boards retired).
--  - threads.ttl_seconds stores the thread's chosen lifespan (NULL = never
--    expires); expires_at remains the live expiry deadline and is recomputed
--    as last-comment + ttl on every new post. Backfilled from the original
--    creation-time expires_at - created_at delta.
--  - threads.board_id, boards, and user_board_reads (per-board unread state)
--    are dropped; the front page sorts by created_at, so
--    idx_threads_updated_at is swapped for idx_threads_created_at.
-- threads is rebuilt rather than column-dropped: SQLite's DROP COLUMN
-- refuses to run when the column appears in a table-level FOREIGN KEY
-- clause (post-drop sanity check), which is how 00006 declared board_id.
-- foreign_keys is toggled off for the rebuild and drops (as in
-- 00003/00008): with it on, DROP TABLE boards would cascade-delete every
-- thread before the copy runs. The PRAGMA is a no-op inside a transaction,
-- so this migration must run WITHOUT one (NO TRANSACTION). A failure
-- mid-migration is not rolled back.
PRAGMA foreign_keys = OFF;

CREATE TABLE threads_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    title TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME, -- NULL means no expiration; otherwise the live expiry deadline (last comment + ttl)
    ttl_seconds INTEGER, -- Chosen lifespan in seconds; NULL means never expires
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

INSERT INTO threads_new (id, user_id, title, created_at, updated_at, expires_at, ttl_seconds)
SELECT
    id, user_id, title, created_at, updated_at, expires_at,
    CASE WHEN expires_at IS NOT NULL
         THEN CAST((julianday(expires_at) - julianday(created_at)) * 86400 AS INTEGER)
    END
FROM threads;

DROP TABLE threads;
ALTER TABLE threads_new RENAME TO threads;
CREATE INDEX idx_threads_created_at ON threads(created_at DESC);
CREATE INDEX idx_threads_expires_at ON threads(expires_at);

DROP TABLE user_board_reads;
DROP TABLE boards;

PRAGMA foreign_keys = ON;

-- +goose Down
-- Best effort: board assignments are not recoverable (board rows are
-- dropped), so every thread lands in a single placeholder "General" board
-- and per-board read state starts empty.
PRAGMA foreign_keys = OFF;

CREATE TABLE boards (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    description TEXT,
    position INTEGER NOT NULL DEFAULT 0
);
INSERT INTO boards (id, name, description, position) VALUES (1, 'General', NULL, 0);

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
SELECT id, 1, user_id, title, created_at, updated_at, expires_at FROM threads;

DROP TABLE threads;
ALTER TABLE threads_old RENAME TO threads;
CREATE INDEX idx_threads_board_id ON threads(board_id);
CREATE INDEX idx_threads_updated_at ON threads(updated_at DESC);
CREATE INDEX idx_threads_expires_at ON threads(expires_at);

CREATE TABLE user_board_reads (
    user_id INTEGER NOT NULL,
    board_id INTEGER NOT NULL,
    last_read_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, board_id),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (board_id) REFERENCES boards(id) ON DELETE CASCADE
);

PRAGMA foreign_keys = ON;
