-- +goose Up
-- First-class post link. An optional URL stored separately from the
-- markdown body and rendered as a link card under the post, so sharing a
-- link does not depend on markdown autolinks and link-only posts (a URL
-- with no body text) are possible. NULL means the post has no link.
ALTER TABLE posts ADD COLUMN url TEXT;

-- +goose Down
ALTER TABLE posts DROP COLUMN url;
