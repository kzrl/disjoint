-- +goose Up
-- +goose NO TRANSACTION
-- Remove board categories entirely (feature retired). Boards become a flat
-- top-level list ordered by position, id.
-- SQLite cannot drop a NOT NULL FK column in place, so boards is rebuilt.
-- foreign_keys is toggled off for the rebuild (as in 00006); the PRAGMA is
-- a no-op inside a transaction, so this migration must run WITHOUT one
-- (NO TRANSACTION). A failure mid-migration is not rolled back.
PRAGMA foreign_keys = OFF;

CREATE TABLE boards_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    description TEXT,
    position INTEGER NOT NULL DEFAULT 0
);

INSERT INTO boards_new (id, name, description, position)
    SELECT id, name, description, position FROM boards;

DROP TABLE boards;
ALTER TABLE boards_new RENAME TO boards;

DROP TABLE categories;

PRAGMA foreign_keys = ON;

-- +goose Down
-- Best effort: the original per-board category assignment is not recoverable
-- (category rows are dropped), so all boards land in a single "General"
-- category.
PRAGMA foreign_keys = OFF;

CREATE TABLE categories (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    position INTEGER NOT NULL DEFAULT 0
);
INSERT INTO categories (id, name, position) VALUES (1, 'General', 0);

CREATE TABLE boards_old (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    category_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    description TEXT,
    position INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (category_id) REFERENCES categories(id) ON DELETE CASCADE
);

INSERT INTO boards_old (id, category_id, name, description, position)
    SELECT id, 1, name, description, position FROM boards;

DROP TABLE boards;
ALTER TABLE boards_old RENAME TO boards;

PRAGMA foreign_keys = ON;
