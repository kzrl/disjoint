Here is the complete, comprehensive technical specification for Disjoint, fully updated to include file attachments (images and video rendering), the SQLite + sqlc stack, Go backend, HTMX interactivity, and expiring threads.

You can hand this entire document directly to a coding agent to build the application from scratch.
Technical Specification: Disjoint
1. Overview & Vision

Disjoint is an open-source, self-hosted web application that blends the structured, topic-based asynchronous depth of old-school forums (phpBB) with the casual, private, and ephemeral vibe of modern private chat groups (Signal).

It is designed for close-knit groups of friends who want an alternative to bloated chat apps or public forums, prioritizing raw performance, self-hostability, data privacy, and a clutter-free environment.
2. Tech Stack & Architecture

    Language: Go (Latest stable version)

    Database: SQLite (modernc.org/sqlite pure-Go driver recommended to avoid CGO dependencies and simplify cross-compilation)

    Database Tooling: sqlc for generating type-safe Go code from raw SQL queries.

    Frontend Interactivity: Server-Side Rendered (SSR) HTML templates + HTMX for partial page updates and real-time interactions without a heavy SPA framework.

    Styling: Bulma v1 (vendored precompiled, MIT) with a dark-first theme override and a small site-specific stylesheet (forum rows, post cards, markdown bodies). Regenerate with `make css` (BULMA_DIR points at a local Bulma checkout). See tasks/plan-bulma.md.

    File Storage: Local disk storage for uploaded media (images, videos, documents).

    Deployment: Single static binary containing embedded templates/assets, plus an official Dockerfile and docker-compose.yml with a persistent volume for SQLite and uploads.

3. Database Schema (schema.sql)

The database must use SQLite with foreign key constraints enabled (PRAGMA foreign_keys = ON;).
SQL

CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE boards (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    description TEXT,
    position INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE threads (
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

CREATE TABLE posts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    body TEXT NOT NULL, -- Stored as raw text/Markdown
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE attachments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    post_id INTEGER NOT NULL,
    filename TEXT NOT NULL,         -- Original user-uploaded file name
    storage_path TEXT NOT NULL,     -- Unique disk path / filename on server (e.g. UUID-based)
    mime_type TEXT NOT NULL,        -- e.g., image/jpeg, video/mp4, application/pdf
    file_size INTEGER NOT NULL,     -- Size in bytes
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (post_id) REFERENCES posts(id) ON DELETE CASCADE
);

-- Track last read timestamps for unread indicators
CREATE TABLE user_board_reads (
    user_id INTEGER NOT NULL,
    board_id INTEGER NOT NULL,
    last_read_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, board_id),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (board_id) REFERENCES boards(id) ON DELETE CASCADE
);

-- Indexes for performance
CREATE INDEX idx_threads_board_id ON threads(board_id);
CREATE INDEX idx_threads_updated_at ON threads(updated_at DESC);
CREATE INDEX idx_posts_thread_id ON posts(thread_id);
CREATE INDEX idx_threads_expires_at ON threads(expires_at);
CREATE INDEX idx_attachments_post_id ON attachments(post_id);

4. Core Features & Functional Requirements
A. Authentication & Security

    Registration / Login: Secure session-based authentication using server-side session rows and HTTP-only cookies, with golang.org/x/crypto/bcrypt for password hashing. The cookie holds a random token; the sessions table stores its SHA-256 hash mapped to a user with an expiry, so logout deletes the row and revokes the token immediately.

    First-Run Setup: If the users table is empty, redirect all requests to a setup/registration page to create the initial administrative user.

    Access Control: All application routes require an active authenticated session.

B. Structure & Organization (phpBB Style)

    Index View (/): Displays boards with descriptions, and a summary preview of the latest thread in each board.

    Board View (/boards/{id}): Lists threads belonging to that board, sorted by updated_at DESC. Displays thread title, author, reply count, creation time, and expiration status.

    Board Creation is admin-only: GET/POST /boards/new require an administrator (non-admins are redirected home with an error flash), and the "+ New Board" navbar link (and the empty-index CTA) render only for admins.

    Thread Creation (/boards/{id}/threads/new): Form to create a thread with a title, initial post body, optional file attachments, and an expiration selection (e.g., 24 hours, 3 days, 7 days, 30 days, or Never).

C. The Ephemerality Twist (Expiring Threads)

    Background Worker: A Go goroutine running on a ticker (e.g., every 5 minutes) that executes a cleanup query:
    SQL

    DELETE FROM threads WHERE expires_at IS NOT NULL AND expires_at < CURRENT_TIMESTAMP;

    Note: Foreign key cascading deletes must automatically remove associated posts, attachments, and files stored on disk.

D. Thread View & Interaction (HTMX Enhanced)

    Thread Detail View (/threads/{id}): Displays the original post, all chronological replies, and any attached media.

    Replying via HTMX (POST /threads/{id}/posts):

        Form submits via HTMX (multipart/form-data to support text + file attachments).

        Server validates input, saves post and files, renders the new post template component, and returns it.

        HTMX appends the new response to the post container (hx-swap="beforeend"), updating the thread live without a full page reload.

        The thread's updated_at timestamp is updated on insertion.

    Markdown Support: Parse post bodies using a Go Markdown library (e.g., [github.com/yuin/goldmark](https://github.com/yuin/goldmark)) to support basic text formatting, lists, quotes, and code blocks. Goldmark runs with the GFM extension (tables, strikethrough, autolinks, task lists), hard line wraps, and raw HTML neutralized. The same pipeline renders post bodies and user-card bios.

    @User Mentions: An @username token in any Markdown body matching [A-Za-z0-9]{3,32} and not immediately preceded by a word character (letter/digit/underscore) renders as a tinted pill link to the user's card: <a class="mention" href="/users/{name}">@{name}</a>. Rendering is stateless (no DB lookup in the render path), so a mention of an unknown user links to a 404 card — consistent with how author links behave for deleted users. Tokens inside code spans/fences, inside link labels, or in e-mail local parts (user@host) are never linked. The name is alphanumeric by construction, so the href can never carry a scheme or path.

    Quoting Posts: Every post card carries a Quote button. A small event-delegated inline script (delegated on the posts container so it also works on posts appended by HTMX) reads the rendered text of that post and inserts a Markdown blockquote into the reply field:

        > **AUTHOR wrote:**
        > first line
        > second line
        >
        > — [view original](/threads/{id}#post-{id})

    Every physical line is > -prefixed (blank lines as bare >), so the quote renders as a single blockquote. The quoted text is soft-capped at 500 characters (truncated with …) while the permalink line is always kept; attachments are excluded from quotes by construction. The quote is stored in the reply's body as ordinary Markdown — no schema change, and since posts are immutable a stored quote can never go stale.

E. File Uploads & Media Rendering

    Storage Handling: Uploaded files are saved to a local directory (configurable via UPLOAD_DIR, defaulting to ./uploads), renamed with a secure unique prefix (e.g., UUID) to prevent collisions, while preserving original filenames in the database.

    Validation: Enforce a maximum file size limit (e.g., 50MB–100MB) and validate allowed MIME types (Images: image/jpeg, image/png, image/webp, image/gif; Videos: video/mp4, video/webm).

    Serving Media: Serve uploaded files via a static route handler (e.g., /uploads/{filename}).

    Template Rendering:

        image/* files render as responsive inline images (<img src="..." loading="lazy">) with light-box or scaling styles.

        video/* files render as native HTML5 video players (<video controls preload="metadata"><source src="..." type="..."></video>).

        Other files render as a clean downloadable file card.

F. Unread Indicators

    When a user views a board or thread, update their timestamp in user_board_reads.

    Compare threads.updated_at against user_board_reads.last_read_at to render an unread indicator badge on active boards and threads.

5. Suggested Project Directory Structure
Plaintext

disjoint/
├── cmd/
│   └── disjoint/
│       └── main.go          # Entrypoint, server setup, router wiring, worker init
├── internal/
│   ├── database/            # sqlc generated files & database connection helpers
│   ├── handlers/            # HTTP handlers (auth, boards, threads, posts, uploads)
│   ├── middleware/          # Auth session validation middleware
│   └── worker/              # Background goroutine for expiring threads cleanup
├── db/
│   ├── schema.sql           # SQLite schema definitions
│   └── query.sql            # sqlc query statements
├── uploads/                 # Local directory for stored media files (git-ignored)
├── web/
│   ├── css/                 # site.css (hand-maintained) + gen-theme.sh (dark-first theme generator)
│   ├── static/              # Vendored Bulma, generated disjoint.css, HTMX (embedded)
│   └── templates/           # HTML templates (.html components and layouts)
├── sqlc.yaml                # sqlc code generation configuration
├── Dockerfile               # Multi-stage Docker build for a single static binary
├── docker-compose.yml       # Compose file mounting persistent data & uploads volumes
└── go.mod

6. Non-Functional Requirements

    Zero External JS Frameworks: No React, Vue, or heavy client-side build steps. Plain HTML templates + HTMX script tag.

    Single Binary Embedding: Use Go's //go:embed directive to bundle HTML templates and CSS assets directly into the compiled binary executable.

    Configuration via Environment Variables:

        PORT (default: 8080)

        DATABASE_PATH (default: disjoint.db)

        UPLOAD_DIR (default: uploads)