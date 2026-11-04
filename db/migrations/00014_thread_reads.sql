-- +goose Up
-- Per-user thread read markers for the front page's unread dots. A row is
-- upserted whenever a logged-in user opens a thread; a thread counts as
-- unread when it has posts newer than the marker (or newer than the account
-- before the first open) written by someone else. Both FKs cascade, so
-- purging an expired thread or deleting a user cleans these up for free.
CREATE TABLE thread_reads (
    user_id INTEGER NOT NULL,
    thread_id INTEGER NOT NULL,
    last_read_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, thread_id),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE
);

CREATE INDEX idx_thread_reads_user ON thread_reads(user_id);

-- +goose Down
DROP TABLE thread_reads;
