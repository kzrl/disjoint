-- +goose Up
-- The mentions & replies inbox: one row per (recipient, thread). An inbox
-- entry is a conversation that needs attention, not a single event, so a busy
-- thread bumps its existing row instead of adding one (one badge increment,
-- one listing row). reason is the strongest trigger ever seen for the
-- conversation ('mention' is never downgraded to 'reply'); read_at is cleared
-- by any new trigger. All FKs cascade, so purging an expired thread or
-- deleting a user removes the entry for free — the inbox can never link to a
-- thread that no longer exists.
CREATE TABLE notifications (
    user_id       INTEGER NOT NULL,
    thread_id     INTEGER NOT NULL,
    post_id       INTEGER NOT NULL,             -- newest triggering post
    reason        TEXT NOT NULL CHECK (reason IN ('mention', 'reply')),
    last_event_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    read_at       DATETIME,                     -- NULL = unread
    PRIMARY KEY (user_id, thread_id),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE,
    FOREIGN KEY (post_id) REFERENCES posts(id) ON DELETE CASCADE
);

-- Inbox listing: newest activity first, per user.
CREATE INDEX idx_notifications_inbox ON notifications(user_id, last_event_at DESC);
-- Badge count: partial index, only unread rows are ever counted.
CREATE INDEX idx_notifications_unread ON notifications(user_id) WHERE read_at IS NULL;

-- +goose Down
DROP TABLE notifications;
