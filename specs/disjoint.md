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

CREATE TABLE threads (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    title TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME, -- NULL means no expiration; otherwise the live expiry deadline (last comment + ttl_seconds)
    ttl_seconds INTEGER, -- NULL means never; otherwise the stored lifespan in seconds, reset onto expires_at by every new comment
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE posts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    body TEXT NOT NULL, -- Stored as raw text/Markdown
    url TEXT,           -- Optional first-class link (http/https, server-validated), rendered as a link card; never parsed as markdown
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME, -- NULL until first edit; set to the edit time by the edit handler
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

CREATE TABLE emojis (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,      -- 'joy' or 'mario_fire' (used as :name:)
    source TEXT NOT NULL,           -- 'unicode' (built-in, seeded) | 'custom' (uploaded)
    glyph TEXT,                     -- unicode emoji only
    storage_path TEXT,              -- custom emoji only (served from the uploads dir)
    mime_type TEXT,                 -- custom emoji only: image/png or image/gif
    file_size INTEGER,              -- custom emoji only, bytes
    created_by INTEGER,             -- custom emoji only
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL
);

CREATE TABLE reactions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    post_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    emoji_id INTEGER NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (post_id, user_id, emoji_id),  -- one reaction per user per emoji per post
    FOREIGN KEY (post_id) REFERENCES posts(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (emoji_id) REFERENCES emojis(id) ON DELETE CASCADE
);

-- Indexes for performance
CREATE INDEX idx_threads_created_at ON threads(created_at DESC);
CREATE INDEX idx_posts_thread_id ON posts(thread_id);
CREATE INDEX idx_threads_expires_at ON threads(expires_at);
CREATE INDEX idx_attachments_post_id ON attachments(post_id);

4. Core Features & Functional Requirements
A. Authentication & Security

    Registration / Login: Secure session-based authentication using server-side session rows and HTTP-only cookies, with golang.org/x/crypto/bcrypt for password hashing. The cookie holds a random token; the sessions table stores its SHA-256 hash mapped to a user with an expiry, so logout deletes the row and revokes the token immediately.

    First-Run Setup: If the users table is empty, redirect all requests to a setup/registration page to create the initial administrative user.

    Access Control: All application routes require an active authenticated session except `/login`, `/register`, `/setup` (first run only), `/healthz`, `/favicon.ico`, and `/static/*`.

    Passkeys (WebAuthn): An invited user may instead register with a passkey and no password: the signup ceremony creates a pending, invisible account row (users.pending_until) so WebAuthn has a user handle, then verification consumes the invite, activates the row in one transaction and starts a session. Passkey-only accounts have users.password_set=0 with a random unusable hash; the password login path refuses them with a pointer to the passkey button, pending rows cannot authenticate at all, and the worker deletes abandoned pending rows after ten minutes. Such accounts may later set a password only after a fresh passkey assertion in the same request. Users add further passkeys from /account (password confirmation required, max 10 per account) and sign in with them from /login without a username. Passkeys are additive: the password remains valid, so a lost device never locks an account out. The RP identity comes from WEBAUTHN_RP_ID / WEBAUTHN_RP_ORIGINS (derived from PUBLIC_BASE_URL when unset) and must be HTTPS outside localhost. Ceremony state is stored server-side in webauthn_ceremonies under an opaque, HttpOnly, 5-minute cookie and consumed exactly once; credentials live in webauthn_credentials (credential id, public key, sign count, transports, AAGUID, backup flags, label) and sign-count regressions are rejected and audited as possible clones. Login assertions use the same disabled-account rule and session creation as password login, and all failure responses are generic to avoid account enumeration.

    Account Security: Sessions have a 7-day idle timeout and a 30-day absolute lifetime; the middleware rejects an idle session via sessions.last_used_at and touches it at most every five minutes. Login/register are rate-limited per IP (10 per 15 minutes), password changes are limited to 5 attempts per 15 minutes per user, and new invites expire after 7 days (pre-migration NULL-expiry invites remain valid). GET /account renders a password-change form and a "Sign out everywhere" action. A password change requires the current password and a new password of at least 8 characters (the same minimum as setup and registration), stores a fresh bcrypt hash, deletes every session row for the user — including the current one — clears the cookie, and redirects to /login. "Sign out everywhere" (POST /account/sign-out-everywhere) deletes every session row for the viewer and clears the current cookie without touching the password.

    Password Recovery: an administrator can mint a single-use password reset link for any active account (not their own) from /admin/users. Only the token's SHA-256 hash is stored (password_reset_tokens); links expire after one hour, issuing a new one invalidates the previous one, and expired/used rows are pruned by the worker. GET /reset?token=... is public and only validates (never consumes); POST /reset consumes the token atomically, stores the new password, sets users.password_set=1 (so passkey-only accounts gain a password), revokes every session, deletes any other pending link, and redirects to /login — a reset link never creates a session. The token is stripped from the Caddy access log's query string like invite codes.

    Request Hardening: every state-changing request (POST/PUT/PATCH/DELETE) is checked for a same-origin `Origin` and a `Sec-Fetch-Site` value of `same-origin`/`none` (missing headers from non-browser clients are allowed, scheme is ignored because TLS may terminate at a proxy). Every request body is capped at 128 MB, with post bodies capped separately at 256 KB. Login/registration are rate-limited per IP (10 per 15 minutes); thread/post creation (30 per 5 minutes), profile/emoji uploads (30), search, autocomplete, reactions (300), thumbnail requests (1200), and password changes (5 per 15 minutes) are limited per user, returning 429 or a flash + redirect. Users have a 2 GiB stored-bytes quota covering attachments and custom emoji.

B. Structure & Organization (Global Front Page)

    Front Page (/): A single global list of all non-expired threads, sorted by creation date (newest first), paginated 30 per page (?page=N). A search box filters the same list by thread title or any post body (`?q=`, case-insensitive `LIKE`, capped at 100 characters); results keep the query in the pagination links and show a dedicated empty state. Rows for threads with posts the viewer has not seen yet carry an unread dot (see §F). Each row shows the thread title (with a dim "expires in …" tag for TTL threads), author, creation time, reply count, and a preview of the thread's most recent comment: author, relative time, and an escaped plain-text snippet (whitespace-flattened, ~120 chars) that links to the /threads/{id}#post-{id} anchor. Threads without replies show "No comments yet". Each row also carries the interactive reaction bar of the thread's opening post (chips, counts, picker; the picker opens downward on the front page), so reactions can be toggled without opening the thread, and a horizontal image strip when the opening post has image attachments: full-width 16:10 thumbnails, one per screenful with mandatory scroll-snap on all viewports, loaded lazily, capped at 8 per row with a "+N" chip for the overflow; the strip is horizontally scrollable with proximity scroll-snap on all viewports (native touch scroll, no JavaScript); every thumbnail and the chip link into /threads/{id}. Non-image attachments (video, documents) and images on replies never appear in the strip. The page also carries an "On the air" strip of users seen within the last 5 minutes.

    Thread Creation (/threads/new): Global — there are no boards. Form to create a thread with a title, initial post body, optional first-class link (posts.url), optional file attachments, and an expiration selection (e.g., 24 hours, 3 days, 7 days, 30 days, or Never). The selection is stored as threads.ttl_seconds (NULL for Never) and the live deadline expires_at = created_at + ttl (NULL for Never). The opening post needs a body, a link, or at least one attachment — a link alone is a valid post. Post bodies are capped at 256 KB.

C. The Ephemerality Twist (Expiring Threads)

    TTL reset on activity: threads carry a stored lifespan (ttl_seconds) and a live deadline (expires_at). On creation, expires_at = created_at + ttl. On every new comment the handler re-sets expires_at = now + ttl (NULL stays NULL for never-threads), so an active thread keeps living and only dies after ttl of silence. All expiry predicates and the worker operate exclusively on expires_at, so nothing else needs to know about the TTL.

    Author lifespan change: the thread's author may change the lifespan at any time before expiry from the thread header (a toggle button reveals an inline select of the same five choices as creation; POST /threads/{id}/expiration). The deadline resets to now + new ttl (or both columns to NULL for Never) — the same "ttl of silence from now" rule every reply applies — so the next comment then bumps from the new ttl. The change is author-only (non-authors get a 404), and a resubmitted, unchanged choice is a no-op that leaves expires_at untouched.

    Background Worker: A Go goroutine running on a ticker (e.g., every 5 minutes) that executes a cleanup query:
    SQL

    DELETE FROM threads WHERE expires_at IS NOT NULL AND expires_at < CURRENT_TIMESTAMP;

    Note: Foreign key cascading deletes must automatically remove associated posts, attachments, reactions, and files stored on disk. Deleting a custom emoji also cascades every reaction that used it.

D. Thread View & Interaction (HTMX Enhanced)

    Thread Detail View (/threads/{id}): Displays one chronological page of a thread (20 posts per page, `?page=N`). Page 1 starts with the opening post; the last page carries the composer and the live-update pill. Earlier pages show a "Go to the latest replies" link instead of the composer. Page links render "Page X of Y" with Prev/Next and clamp out-of-range values to the first/last page. Permalinks resolve across pages: `?from={post-id}` redirects to the canonical `?page=N#post-{post-id}` for that post (front-page last comments, inbox rows, gallery captions, quotes, edit redirects, and the new-replies pill all use it); an unknown or foreign post id falls back to page 1. Old fragment-only links are upgraded client-side once by `disjoint.js` (skipped when `?page=` is already present, so deleted posts cannot cause a redirect loop). Attachments and reactions are loaded only for the rendered page.

    Replying via HTMX (POST /threads/{id}/posts):

        Form submits via HTMX (multipart/form-data to support text + file attachments).

        Server validates input, saves post and files, renders the new post template component, and returns it.

        HTMX appends the new response to the post container (hx-swap="beforeend"), updating the thread live without a full page reload.

        The thread's updated_at timestamp is updated on insertion, and for TTL threads the expiry deadline is pushed out to now + ttl (never-threads keep a NULL deadline).

    Editing Posts & Thread Titles: Authors can edit their own content — author-only, no admin override:
        A post's author may edit the post's **body** and **first-class link**; attachments, expiration, and authorship are immutable on edit. The edit is a standard form on a dedicated page (GET/POST /posts/{id}/edit); the post card shows an author-only "Edit" button, and a successful save flashes and redirects to the post's anchor (/threads/{id}#post-{pid}). Validation mirrors creation: the link must be an absolute http(s) URL with a host (≤2048 chars), and a post with no attachments needs a body or a link (an attachment-only post may be edited down to attachments only). A resubmitted, unchanged form is a no-op that leaves `updated_at` untouched.
        A thread's author may rename the thread via an inline form in the thread header (POST /threads/{id}/title). Titles are trimmed, required, and capped at 200 characters; the front page reflects renames immediately. The opening post's body is edited through the normal post-edit flow.
        Ownership is enforced in the UPDATE's WHERE clause (`user_id = ?`): a non-author gets a 404, never a 403, and never a validation error. Expired threads are not editable.
        `posts.updated_at` (NULL until first edit) is set to the edit time on every successful edit; the post card shows a dim "(edited)" marker — visible to all viewers, with the edit time in its tooltip. `created_at` is never changed and there is no edit-history table. Because posts can now be edited, a stored quote (see Quoting) is a point-in-time copy and can go stale; the "view original" permalink line still resolves to the post. Reactions survive edits (they are per-post, not per-text).

    Deleting Content: Authors may delete their own replies (POST /posts/{id}/delete); thread authors may delete their threads (POST /threads/{id}/delete); administrators may delete any thread or reply. Deleting the opening post deletes the whole thread, because a thread cannot exist without it. Permission is checked before the delete: non-owners and non-admins get a 404, not a 403. The row delete cascades posts, attachments, reactions, reads, and inbox rows; the handler then removes the attachment files and cached thumbnails from disk. The UI renders delete controls only for permitted viewers and asks for confirmation before submitting.

    Markdown Support: Parse post bodies using a Go Markdown library (e.g., [github.com/yuin/goldmark](https://github.com/yuin/goldmark)) to support basic text formatting, lists, quotes, and code blocks. Goldmark runs with the GFM extension (tables, strikethrough, autolinks, task lists), hard line wraps, and raw HTML neutralized. The same pipeline renders post bodies and user-card bios.

    Post Links: Each post (opening post and replies) accepts an optional first-class link field (posts.url), separate from the markdown body. Server-side validation requires an absolute http(s) URL with a host (≤2048 chars); the scheme check is the security boundary because template escaping alone would pass a javascript: href through. A valid link renders as a link card under the post body (full URL as the label, truncated with an ellipsis; title carries the verbatim URL; target="_blank" rel="noopener noreferrer"). A link alone satisfies the post-content requirement, so link-only posts are legal. The link is stored verbatim, never re-parsed as markdown, and is excluded from quotes (quotes read the .content body block only).

    @User Mentions: An @username token in any Markdown body matching [A-Za-z0-9]{3,32} and not immediately preceded by a word character (letter/digit/underscore) renders as a tinted pill link to the user's card: <a class="mention" href="/users/{name}">@{name}</a>. Rendering is stateless (no DB lookup in the render path), so a mention of an unknown user links to a 404 card — consistent with how author links behave for deleted users. Tokens inside code spans/fences, inside link labels, or in e-mail local parts (user@host) are never linked. The name is alphanumeric by construction, so the href can never carry a scheme or path.

    Mention Autocomplete: Typing `@` (or `@partial`) in any compose textarea — reply, new thread, edit post, bio — opens a suggestion list backed by `GET /api/mentions?q={prefix}`. The endpoint is auth-gated by the global middleware, returns `{"users":[{"name":...,"avatar":...}]}` (at most 10, case-insensitive ASCII prefix, recently active first then alphabetical), rejects prefixes longer than 32 characters with 400, and escapes `%`/`_`/`\` for SQLite `LIKE ... ESCAPE '\'`. The client trigger mirrors the markdown mention parser exactly: the last run of `[A-Za-z0-9]{0,32}` ending at the caret, preceded by start-of-text or a non-word character, so `user@host` and glued words never trigger. Suggestions are fetched with a 120 ms debounce and an AbortController (stale responses cannot repaint); selection replaces the token with `@name ` via `setRangeText` (native undo preserved) and closes the list. Arrow keys move the highlight, Enter/Tab/click inserts, Escape/blur closes; Ctrl/Cmd+Enter still submits. The dropdown is anchored to the textarea's `.control` wrapper, not the caret; no user data is rendered with innerHTML.

    Quoting Posts: Every post card carries a Quote button. A small event-delegated inline script (delegated on the posts container so it also works on posts appended by HTMX) reads the rendered text of that post and inserts a Markdown blockquote into the reply field:

        > **AUTHOR wrote:**
        > first line
        > second line
        >
        > — [view original](/threads/{id}#post-{id})

    Every physical line is > -prefixed (blank lines as bare >), so the quote renders as a single blockquote. The quoted text is soft-capped at 500 characters (truncated with …) while the permalink line is always kept; attachments are excluded from quotes by construction. The quote is stored in the reply's body as ordinary Markdown — no schema change. A post may be edited after it was quoted (see Editing), so a stored quote is a point-in-time copy that can go stale; the "view original" permalink line always resolves to the post.

    Slack-Style Reactions: Every post — the thread's opening post included — carries a reaction bar — below its body on the thread page, under the title in the front-page thread rows — showing one chip per reacted emoji (glyph for built-in unicode emoji, a scaled image for custom uploads) with a count, highlighted when the viewer has reacted, plus a “+” trigger opening a picker grid of every emoji (built-in and custom). Hovering a chip lists who reacted (native title tooltip: “Reacted by: a, b, c (you)”; the usernames join comes from a LEFT JOIN on the reactions rows, so a dangling user degrades to being skipped rather than breaking the tooltip). POST /posts/{id}/reactions toggles the viewer's reaction for the given emoji_id (INSERT OR IGNORE, then delete on the no-op), and responds with only the re-rendered bar, which HTMX swaps in place (hx-swap="outerHTML") — no full post re-render. The UNIQUE (post_id, user_id, emoji_id) constraint makes concurrent toggles safe. Custom emoji are PNG/GIF only (≤256 KB, ≤256×256 px), named [a-z0-9_]{2,64}, uploaded on the /emojis community page where any member can delete any custom emoji (deletion cascades its reactions).

    Inline Emoji: A `:name:` token in any post body or bio renders as the emoji — the unicode glyph for built-ins, an `<img class="emoji-img emoji-inline" alt=":name:" loading="lazy">` (reaction-chip sizing) for custom uploads. The token grammar mirrors the uploader contract exactly: `:`, 2–64 chars of `[a-z0-9_]`, `:`, not preceded by a word character, so `10:30:00`, `http://x:8080/path`, and `note:joy:here` all stay literal. The parser resolves the name at parse time against a process-wide snapshot of the emojis table and returns nil when it is unknown, so the token is never consumed: typos, deleted emoji, code spans/fences, and link labels degrade to literal `:name:` text (deleting a custom emoji therefore downgrades every old post that used it, with no data rewrite). The snapshot is warmed in NewApp and refreshed by the emoji upload/delete handlers (atomic pointer swap; the render path never queries the database). Only the storage path is DB-sourced, so it is HTML-escaped by hand in the renderer.

    Liveness (polling, no SSE): The navbar bell polls GET /inbox/count every 60s (`hx-get` + `hx-trigger="every 60s [!document.hidden]"`, `hx-swap="outerHTML"`) and the endpoint returns the whole anchor, so the swap cannot drop the trigger; the count is the same expiry-filtered query the layout uses. The last page of a thread carries a `#new-posts` container that polls GET /threads/{id}/updates?after={newest_rendered_post} every 20s; the count covers only other users' posts newer than that id (`NewPostsAfter`, so a viewer's own reply is never offered back to them) and the fragment keeps polling when the answer is zero. The pill it offers is a reload link (/threads/{id}?from={first_new}#post-{first_new}) rather than an HTMX append: the viewer's own HTMX replies can interleave with other users' posts, so appending only the missing ones would break chronological order, and a reload also refreshes the unread/notification markers. Both fragments are sent `Cache-Control: no-store` (a cached count would freeze the very signal the poll exists to deliver) and both endpoints answer 200 with the container they replace (an empty body would delete the polling element and a 4xx is not swapped), degrade to a non-polling container for missing or expired threads (stopping the loop quietly), and pause while the tab is hidden (the filter is re-evaluated every tick, so they resume). No websockets: the app stays request/response.

    Image Viewer: The shared `<dialog>` lightbox opens any `.lightbox-link`. When that link sits inside a `[data-lightbox-group]` (the thread page's posts container; a future gallery grid uses the same attribute), the viewer browses the whole group in DOM/post order: prev/next overlay arrows (clamped and disabled at the ends, hidden along with the counter and filename for single-image groups), ArrowLeft/ArrowRight and Home/End on the dialog (Escape stays native), and a horizontal swipe classified as `|dx| >= 40px && |dx| > 1.5|dy|` (`touch-action: pan-y` lets horizontal drags reach the pointer handlers while vertical drags stay native). Links outside a group keep the previous one-image behaviour, so new thumbnail surfaces cannot accidentally join a thread's set. The grid's already-cached thumbnail paints immediately, then the full-size original is swapped in only after it has loaded — swapping a cached `src` is a repaint, so there is no blank frame — and a per-render token discards stale swaps after fast navigation. The item list is rebuilt on every open, so replies appended by HTMX are included the next time the viewer is opened. No server endpoints or schema are involved.

    Mentions & Replies Inbox: Every new post writes inbox entries in the same transaction as the post. The `notifications` table holds one row per (recipient, thread) — PRIMARY KEY (user_id, thread_id), reason “mention” or “reply”, post_id pointing at the newest triggering post, read_at NULL while unread — so a busy thread is one inbox row and one badge increment, not one per reply. Recipients are the names the renderer turned into mention pills (extracted from the same goldmark AST via markdown.Mentions, so code spans, link labels, escaped tokens, and e-mail local parts never notify; unknown names notify nobody) plus every user who has posted in the thread, minus the actor. A trigger bumps post_id/last_event_at, clears read_at, and upgrades reason reply→mention (never the reverse). Reading is explicit: GET /inbox marks nothing, opening the thread clears that thread's entry (read-through, beside the unread-dot marker), and the inbox header's “Mark all read” (POST /inbox/read) clears everything; the navbar bell shows the unread count, computed once per full page render and filtered by thread expiry in SQL so it never advertises a thread that already 404s. Edits, reactions, and renames do not notify. All three FKs cascade, so purging an expired thread or deleting a user cleans the inbox for free.

E. File Uploads & Media Rendering

    Storage Handling: Uploaded files are saved to a local directory (configurable via UPLOAD_DIR, defaulting to ./uploads), renamed with a secure unique prefix (e.g., UUID) to prevent collisions, while preserving original filenames in the database.

    Validation: Enforce a maximum file size limit (50MB per file, 100MB per post, 16 files per post) and validate allowed MIME types (Images: image/jpeg, image/png, image/webp, image/gif; Videos: video/mp4, video/webm). Each user also has a 2 GiB stored-bytes quota covering attachments and custom emoji. Image uploads validate dimensions from headers before decoding (emoji ≤256×256, avatars ≤4096×4096/16MP). SQLite runs with `secure_delete` and the expiry worker truncates the WAL; backups are not encrypted and retain pre-expiry content.

    Serving Media: Serve uploaded files via a static route handler (e.g., /uploads/{filename}).

    Thumbnails: Inline images are served from /thumbs/{storage_path}?w={320|640|1280} (default 640). Generation is lazy — the first request decodes the original, applies the JPEG EXIF orientation (so portrait phone photos are not sideways), scales with golang.org/x/image/draw, and writes a JPEG (quality 82), or a PNG when the pixels carry transparency, into <UPLOAD_DIR>/thumbs/<uuid>.<width>.<version>.<ext>. The write is a temp file plus rename, so no partial cache entry is ever served; storage paths are immutable UUIDs and the renderer version is part of the filename, so derivatives are served with Cache-Control: immutable (bumping Version is what invalidates the whole cache after an algorithm change). Unknown widths are a 404 (the allowlist is the whole width policy). Anything that cannot be thumbnailed is served as the original bytes instead: GIFs (a first-frame re-encode would silently freeze animations), sources already narrower than the requested width (no upscaling), and files that fail to decode or exceed 40 megapixels (checked with image.DecodeConfig before pixels are allocated). Decode work is bounded by a two-slot process-wide semaphore; duplicate generation for the same key is harmless because the write is atomic. The expiry worker deletes every cached width alongside the original, so thumbnails die with their thread. WebP decoding is registered via golang.org/x/image/webp; the build stays CGO-free.

    Gallery (/gallery): a read-only wall of every image attachment on a live thread — opening posts and replies — newest upload first (`attachments.id DESC`), 48 per page with Prev/Next pagination. The query uses the same JPEG/PNG/WebP/GIF allowlist as the front-page strips and the same expiry predicate; non-image attachments and expired threads never appear. Tiles are a CSS multi-column masonry over natural aspect ratios (`columns: 4 15rem` + `break-inside: avoid`), so the newest image is the top of the first column (column-major). Each tile's image opens the shared lightbox (`data-lightbox-group`, so arrows/keyboard/swipe work), served through `/thumbs/{path}?w={320|640|1280}` with `srcset`; the caption links to `/threads/{id}#post-{pid}`. No per-user state, no filter/search, no new schema or index; the worker's expiry purge removes the tiles and files automatically. Guests are redirected to `/login` by the existing middleware.

    Template Rendering:

        image/* files render as responsive inline images (<img src="..." loading="lazy">) with light-box or scaling styles.

        video/* files render as native HTML5 video players (<video controls preload="metadata"><source src="..." type="..."></video>).

        Other files render as a clean downloadable file card.

F. Unread Indicators

    Per-user thread read markers are back, now at thread scope: `thread_reads(user_id, thread_id, last_read_at)` with `PRIMARY KEY (user_id, thread_id)` and both foreign keys cascading. Opening a thread upserts the viewer's marker. A thread is unread when it has at least one post by someone else newer than the marker; before the first open the account's creation time is the fallback, so a new account does not inherit the whole archive as unread. The viewer's own posts never count. The front page renders a dot next to unread thread titles; the marker is best-effort (a failed upsert never breaks the page) and is written on the same GET that renders the thread, beside the inbox read-through. Board-level NEW badges remain gone along with boards.

5. Suggested Project Directory Structure
Plaintext

disjoint/
├── cmd/
│   └── disjoint/
│       └── main.go          # Entrypoint, server setup, router wiring, worker init
├── internal/
│   ├── database/            # sqlc generated files & database connection helpers
│   ├── handlers/            # HTTP handlers (auth, front page, threads, posts, uploads)
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