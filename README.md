# Disjoint

[![Go 1.27+](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)

**Disjoint** is an open-source, self-hosted web application that blends the structured, topic-based asynchronous depth of classic forums with the casual, private, and ephemeral vibe of modern private chat groups.

Built with **Go**, **SQLite**, **sqlc**, **HTMX**, and Bulma v1. Zero external JavaScript frameworks.

---

## Features

- **Global Front Page**: Every thread lives on front page, sorted by creation date (newest first).
- **Expiring Threads**: Set lifespans for threads (24h, 3d, 7d, 30d, or Never). Every new comment pushes the deadline out by the chosen lifespan, so active threads keep living. A background worker automatically purges expired threads along with all attachments from disk.
- **HTMX Real-Time Interactions**: Live replies append directly to the thread without full page reloads (`multipart/form-data` supported).
- **Embedded File Attachments & Media**:
  - Images (`image/jpeg`, `image/png`, `image/webp`, `image/gif`) render responsive inline previews.
  - Videos (`video/mp4`, `video/webm`) render with native HTML5 players.
  - Documents and other files render as downloadable attachment cards.
- **Markdown Support**: Rich formatting, tables, task lists, blockquotes, and code blocks powered by Goldmark with safe HTML rendering.
- **@User Mentions**: `@username` tokens in posts (and bios) render as tinted pill links to that user's card. Code spans, e-mail addresses, and malformed tokens are never linked.
- **Quoting Posts**: A `Quote` button on every post inserts a Markdown blockquote — author attribution, the quoted text (capped at 500 chars), and a “view original” permalink — into the reply box. Works on HTMX-appended replies too; no schema change.
- **Link Posts**: Every post — opening post or reply — can carry a first-class link field, rendered as a link card under the post and never parsed as markdown. Links are validated to http(s) server-side, and a link alone (no body) is a valid post, so sharing a URL takes one field instead of a sentence of markdown.
- **Slack-Style Reactions**: React to any post — the thread's opening post included — with built-in unicode emoji or custom community emoji (PNG/GIF uploads, `:snake_case_name:` style, managed on the `/emojis` page). One reaction per user per emoji per post; clicking a chip toggles it, and hovering a chip shows who reacted. Reaction bars update in place via HTMX, and deleting an emoji removes its reactions. Thread rows on the front page show the opening post's reactions too, so you can react without opening the thread.
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

Open `http://localhost:8080` in your browser. All data (SQLite database and uploads) persists in the named `disjoint_data` volume, mounted at `/data` inside the container.

### 2. Build and Run from Source

**Prerequisites**: Go 1.27+

```bash
# Build the binary (CGO disabled, commit-stamped)
make build

# Run (or: make run, PORT=9090 to override the port)
./bin/disjoint
```

On first run, the SQLite database (`disjoint.db`) and the `uploads/` directory are created in the working directory.

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
`HOST=127.0.0.1` guarantees that). For TLS termination, add a `tls` directive
(or `internal` for localhost) to the Caddy site block.

---

## Running Tests

```bash
make test        # go test -v ./...
make test-race   # data-race detector
```

---

## Development

```bash
make generate    # regenerate sqlc Go code from db/schema.sql
make css         # re-vendor Bulma and regenerate web/static/disjoint.css
```

### Project layout

| Path | Purpose |
|---|---|
| `cmd/disjoint` | Entry point |
| `internal/` | Handlers, auth, avatars, markdown, background worker |
| `db/` | `schema.sql` and sqlc query definitions |
| `web/` | Templates, vendored static assets (HTMX, Bulma) |
| `specs/` | Design specs (core app, user/admin flows) |

---

## Security notes

- Sessions are opaque server-side tokens; no shared secret is required or read from the environment.
- Login and registration are rate-limited; behind a reverse proxy the limiter keys on the real client IP (see above).
- Avatar and attachment uploads are type- and size-checked.

---

## License

[Apache License 2.0](LICENSE)
