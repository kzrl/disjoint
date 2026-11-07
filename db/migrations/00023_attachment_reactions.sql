-- +goose Up
-- Per-image reactions for the gallery. Post reactions live in `reactions`;
-- these are keyed by the attachment so two images in the same post can carry
-- different reactions. Rows cascade away with the attachment (and therefore
-- with the post/thread/expiry cleanup).
CREATE TABLE IF NOT EXISTS attachment_reactions (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    attachment_id INTEGER NOT NULL,
    user_id       INTEGER NOT NULL,
    emoji_id      INTEGER NOT NULL,
    created_at    DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (attachment_id, user_id, emoji_id),
    FOREIGN KEY (attachment_id) REFERENCES attachments(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (emoji_id) REFERENCES emojis(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_attachment_reactions_attachment
    ON attachment_reactions(attachment_id);

-- +goose Down
DROP TABLE IF EXISTS attachment_reactions;
