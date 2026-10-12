# Disjoint

**Disjoint** is an open-source, self-hosted web application that blends the structured, topic-based asynchronous depth of classic forums (phpBB) with the casual, private, and ephemeral vibe of modern private chat groups.

Built with **Go**, **SQLite**, **sqlc**, **HTMX**, and Bulma v1 (vendored, dark-first). Zero external JavaScript frameworks.

---

## Features

- **Topic-Based Organization (phpBB-Style)**: Categories, nested boards, thread topics, and chronological replies.
- **Ephemerality Twist (Expiring Threads)**: Set lifespans for threads (24h, 3d, 7d, 30d, or Never). A background worker automatically purges expired threads along with all attachments from disk.
- **HTMX Real-Time Interactions**: Live replies append directly to the thread without full page reloads (`multipart/form-data` supported).
- **Embedded File Attachments & Media**:
  - Images (`image/jpeg`, `image/png`, `image/webp`, `image/gif`) render responsive inline previews.
  - Videos (`video/mp4`, `video/webm`) render with native HTML5 players.
  - Documents and other files render as downloadable attachment cards.
- **Markdown Support**: Rich formatting, tables, task lists, blockquotes, and code blocks powered by Goldmark with safe HTML rendering.
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
| `PORT` | `8080` | Port for the HTTP server |
| `DATABASE_PATH` | `disjoint.db` | File path for SQLite database |
| `UPLOAD_DIR` | `uploads` | Directory for stored media attachments |

---

## Running Tests

```bash
go test -v ./...
```
