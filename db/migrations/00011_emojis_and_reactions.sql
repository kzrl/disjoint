-- +goose Up
-- Slack-style reactions. Two tables:
--  - emojis: one row per available emoji. 'unicode' rows are the seeded
--    built-ins (rendered as glyphs); 'custom' rows are user-uploaded images
--    (PNG/GIF/WEBP) served from the uploads directory, like attachments.
--  - reactions: one per (post, user, emoji). The UNIQUE constraint enforces
--    Slack's one-reaction-per-user-per-emoji rule; toggling is an
--    INSERT OR IGNORE followed by a DELETE when nothing was inserted.
-- All FKs cascade, so purging an expired thread or deleting a user/emoji
-- removes their reactions for free.
CREATE TABLE emojis (
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

CREATE INDEX idx_emojis_name ON emojis(name);

CREATE TABLE reactions (
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

CREATE INDEX idx_reactions_post_id ON reactions(post_id);

-- Built-in unicode emoji (Slack-style short names). Custom uploads may never
-- collide with these names (UNIQUE constraint).
INSERT INTO emojis (name, source, glyph) VALUES
    ('joy', 'unicode', '🙂'),
    ('excited', 'unicode', '🤩'),
    ('tada', 'unicode', '🎉'),
    ('clap', 'unicode', '👏'),
    ('heart', 'unicode', '❤️'),
    ('eyes', 'unicode', '👀'),
    ('thinking', 'unicode', '🤔'),
    ('thumbsup', 'unicode', '👍'),
    ('thumbsdown', 'unicode', '👎'),
    ('fire', 'unicode', '🔥'),
    ('rocket', 'unicode', '🚀'),
    ('party', 'unicode', '🥳');

-- +goose Down
DROP TABLE reactions;
DROP TABLE emojis;
