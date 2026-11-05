
CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    is_admin INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    bio TEXT,
    avatar_path TEXT,
    avatar_nonce INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS user_presence (
    user_id INTEGER PRIMARY KEY,
    last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS invites (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    code TEXT NOT NULL UNIQUE,
    created_by INTEGER NOT NULL,
    used_by INTEGER,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    used_at DATETIME,
    expires_at DATETIME,
    FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (used_by) REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_invites_code ON invites(code);

CREATE TABLE IF NOT EXISTS threads (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    title TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME, -- NULL means no expiration; otherwise, the live expiry deadline (last comment + ttl)
    ttl_seconds INTEGER, -- Chosen lifespan in seconds; NULL means never expires
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS posts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    body TEXT NOT NULL, -- Stored as raw text/Markdown
    url TEXT,           -- Optional first-class link, rendered as a link card; never parsed as markdown. NULL when absent.
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME, -- NULL until first edit; set to the edit time by the edit handler
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS attachments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    post_id INTEGER NOT NULL,
    filename TEXT NOT NULL,         -- Original user-uploaded file name
    storage_path TEXT NOT NULL,     -- Unique disk path / filename on server (e.g. UUID-based)
    mime_type TEXT NOT NULL,        -- e.g., image/jpeg, video/mp4, application/pdf
    file_size INTEGER NOT NULL,     -- Size in bytes
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (post_id) REFERENCES posts(id) ON DELETE CASCADE
);

-- Indexes for performance
CREATE INDEX IF NOT EXISTS idx_threads_created_at ON threads(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_posts_thread_id ON posts(thread_id);
CREATE INDEX IF NOT EXISTS idx_threads_expires_at ON threads(expires_at);
CREATE INDEX IF NOT EXISTS idx_attachments_post_id ON attachments(post_id);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    user_id INTEGER NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at INTEGER NOT NULL,
    last_used_at INTEGER,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS login_tokens (
    token_hash TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL,
    created_by INTEGER,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at INTEGER NOT NULL,
    used_at    DATETIME,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_login_tokens_user ON login_tokens(user_id);
CREATE INDEX IF NOT EXISTS idx_login_tokens_expires ON login_tokens(expires_at);

CREATE TABLE IF NOT EXISTS emojis (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,       -- 'joy' or 'mario_fire' (used as :name:)
    source TEXT NOT NULL,            -- 'unicode' | 'custom'
    glyph TEXT,                      -- unicode emoji only, e.g. '🙂'
    storage_path TEXT,               -- custom emoji only; NULL for unicode
    mime_type TEXT,                  -- custom emoji only, e.g. image/png
    file_size INTEGER,               -- custom emoji only, bytes
    created_by INTEGER,              -- custom emoji only; NULL for unicode
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_emojis_name ON emojis(name);

CREATE TABLE IF NOT EXISTS reactions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    post_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    emoji_id INTEGER NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (post_id, user_id, emoji_id),
    FOREIGN KEY (post_id) REFERENCES posts(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (emoji_id) REFERENCES emojis(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_reactions_post_id ON reactions(post_id);


CREATE TABLE IF NOT EXISTS thread_reads (
    user_id INTEGER NOT NULL,
    thread_id INTEGER NOT NULL,
    last_read_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, thread_id),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_thread_reads_user ON thread_reads(user_id);

-- Inbox: one row per (recipient, thread). A new mention/reply bumps the row
-- (newest post, reason upgraded to 'mention', read_at cleared) instead of
-- adding an event.
CREATE TABLE IF NOT EXISTS notifications (
    user_id       INTEGER NOT NULL,
    thread_id     INTEGER NOT NULL,
    post_id       INTEGER NOT NULL,
    reason        TEXT NOT NULL CHECK (reason IN ('mention', 'reply')),
    last_event_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    read_at       DATETIME,
    PRIMARY KEY (user_id, thread_id),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE,
    FOREIGN KEY (post_id) REFERENCES posts(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_notifications_inbox ON notifications(user_id, last_event_at DESC);
CREATE INDEX IF NOT EXISTS idx_notifications_unread ON notifications(user_id) WHERE read_at IS NULL;
