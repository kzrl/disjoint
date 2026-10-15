# Disjoint

**Disjoint** is an open-source, self-hosted web application that blends the structured, topic-based asynchronous depth of classic forums (phpBB) with the casual, private, and ephemeral vibe of modern private chat groups.

Built with **Go**, **SQLite**, **sqlc**, **HTMX**, and Bulma v1 (vendored, dark-first). Zero external JavaScript frameworks.

---

## Features

- **Topic-Based Organization (phpBB-Style)**: Boards, thread topics, and chronological replies.
- **Ephemerality Twist (Expiring Threads)**: Set lifespans for threads (24h, 3d, 7d, 30d, or Never). A background worker automatically purges expired threads along with all attachments from disk.
- **HTMX Real-Time Interactions**: Live replies append directly to the thread without full page reloads (`multipart/form-data` supported).
- **Embedded File Attachments & Media**:
  - Images (`image/jpeg`, `image/png`, `image/webp`, `image/gif`) render responsive inline previews.
  - Videos (`video/mp4`, `video/webm`) render with native HTML5 players.
  - Documents and other files render as downloadable attachment cards.
- **Markdown Support**: Rich formatting, tables, task lists, blockquotes, and code blocks powered by Goldmark with safe HTML rendering.
- **@User Mentions**: `@username` tokens in posts (and bios) render as tinted pill links to that user's card. Code spans, e-mail addresses, and malformed tokens are never linked.
- **Quoting Posts**: A `Quote` button on every post inserts a Markdown blockquote — author attribution, the quoted text (capped at 500 chars), and a “view original” permalink — into the reply box. Works on HTMX-appended replies too; no schema change.
- **Unread Indicators**: Retro-modern `NEW` badges highlight active boards and threads updated since your last visit.
- **User Avatars**: Profile pictures (JPG, PNG, WEBP, up to 5MB) uploaded from your user card and shown next to your posts.
- **Zero-CGO Single Binary**: Pure Go SQLite driver (`modernc.org/sqlite`) and `//go:embed` for static assets and templates.
- **Self-Contained Offline Deployment**: Vendored HTMX and vendored Bulma v1 plus a small site stylesheet; no external CDNs required.
- **First-Run Setup**: Automatic detection on initial launch to create the administrator account.

---

## Quick Start

### 1. Run with Docker Compose (Recommended)

```bash
docker compose up -d
```

Open `http://localhost:8080` in your browser.

### 2. Build and Run from Source

**Prerequisites**: Go 1.22+

```bash
# Build the binary
go build -o disjoint ./cmd/disjoint

# Run
./disjoint
```

Open `http://localhost:8080` in your browser to complete initial setup.

---

## Configuration

Disjoint is configured via environment variables:

| Variable | Default | Description |
|---|---|---|
| `HOST` | `0.0.0.0` | Interface for the HTTP server |
| `PORT` | `8080` | Port for the HTTP server |
| `DATABASE_PATH` | `disjoint.db` | File path for SQLite database |
| `UPLOAD_DIR` | `uploads` | Directory for stored media attachments |
| `COOKIE_SECURE` | _(auto: TLS detected per request)_ | Force `Secure` on session cookies: `1` behind a TLS-terminating reverse proxy, `0` to disable |

### Behind a reverse proxy (Caddy)

```caddyfile
example.com {
	reverse_proxy 127.0.0.1:8080
}
```

Run the app with `HOST=127.0.0.1` and `COOKIE_SECURE=1`. Caddy sets
`X-Real-IP` to the real client address on proxied requests, and Disjoint's
login/register rate limiter uses it — but only when the connection arrives
from loopback, so the app must not be directly exposed (binding
`HOST=127.0.0.1` guarantees that).

---

## Running Tests

```bash
go test -v ./...
```
