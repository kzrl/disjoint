# Disjoint

**Disjoint** is free & open-source web application that tries to bring the vibes from classic internet forums and BBSes. Another place for the group chat to hang out online.

Self hosted, not some corporate advertising funnel.

Built with **Go**, **SQLite**, **sqlc**, **HTMX**, and Bulma v1.

---

## Features

- **Global Front Page**: Every thread lives on the front page, sorted by creation date (newest first).

- **Search**: The front page searches thread titles and post bodies, with paginated results that keep the query in the URL.

- **Unread Markers**: Threads with posts you have not seen yet carry a dot on the front page. Opening a thread clears it; your own posts never count.

- **Expiring Threads (TTL)**: Set lifespans for threads (24h, 3d, 7d, 30d, or Never). Every new comment pushes the deadline out by the chosen lifespan, so active threads keep living. A background worker automatically purges expired threads along with all attachments from disk.

- **HTMX Real-Time Interactions**: Live replies append directly to the thread without full page reloads (`multipart/form-data` supported).

- **Embedded File Attachments & Media**:
  - **Images** (`image/jpeg`, `image/png`, `image/webp`, `image/gif`) render responsive inline previews, served as cached 320/640/1280px thumbnails (`srcset`) with the full-size original one click away in the lightbox.
  - **Videos** (`video/mp4`, `video/webm`) render with native HTML5 players.
  - **Documents** and other files render as downloadable attachment cards.

- **Image Viewer**: Click any image in a thread to browse every image in that thread — on-screen arrows, ←/→ and Home/End, or a horizontal swipe on touch, with a position counter. The thumbnail paints instantly and sharpens to full resolution in place.

- **Markdown Support**: Rich formatting, tables, task lists, blockquotes, and code blocks powered by Goldmark with safe HTML rendering.

- **@User Mentions**: `@username` tokens in posts (and bios) render as tinted pill links to that user's card. Code spans, e-mail addresses, and malformed tokens are never linked.

- **First-Class Link Posts**: Every post — opening post or reply — can carry a separate link field, rendered as a clean link card under the post. Links are validated to http(s) server-side, making URL sharing instant.

- **Slack-Style Reactions**: React to any post with built-in unicode emoji or custom community emoji

- **Inline Emoji**: Type `:joy:` or any community emoji name in a post or your bio and it renders inline — built-ins as their glyph, custom uploads at reaction-chip size. Typos and deleted names stay as plain text.

- **Live Updates**: The navbar bell refreshes itself every minute, and open threads grow a "N new replies" pill as the conversation continues — no manual reload. Polling pauses on hidden tabs.

- **Mentions & Replies Inbox**: A navbar bell counts the conversations waiting for you. An `@mention` notifies exactly the users whose names render as pills, and replies notify everyone in the thread. One row per thread (not per reply), read only when you open the thread or press “Mark all read”.

- **User Avatars**: Profile pictures (JPG, PNG, WEBP, up to 5MB) uploaded from your user card and shown next to your posts.

- **Self-Contained Offline Deployment**: Vendored HTMX and vendored Bulma v1 plus a small site stylesheet; no external CDNs required.

- **First-Run Setup**: Automatic detection on initial launch to create the administrator account.

---

## Quick Start

### 1. Run with Docker Compose (Recommended)

Create a `docker-compose.yml` file:

```yaml
services:
  disjoint:
    image: ghcr.io/kzrl/disjoint:latest
    container_name: disjoint
    ports:
      - "8080:8080"
    volumes:
      - disjoint_data:/data
    restart: unless-stopped

volumes:
  disjoint_data:
```

Then run:
```bash
docker compose up -d
```

Open `http://localhost:8080` in your browser. All data persists in the `disjoint_data` volume.

The image runs as UID 10001. If you bind-mount host directories instead of using the named volume, make them writable by that UID.

Images are published to GHCR by CI on pushes to `master` (tag `latest`) and on `v*` tags. You can also build the repository's own compose file locally with `docker compose up -d --build`.

### 2. Build and Run from Source

**Prerequisites**: Go 1.27+

```bash
# Build the binary (CGO disabled, commit-stamped)
make build

# Run (or: make run, PORT=9090 to override the port)
./bin/disjoint
```

On first run, the SQLite database (`disjoint.db`) and the `uploads/` directory are created in the working directory. Open `http://localhost:8080` to complete initial setup.

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

### Uploads

A single post (opening post or reply) accepts at most **16 attachments**, **50 MB per file**, and **100 MB combined**; any request body is capped at 128 MB. Accepted types are JPEG, PNG, WebP, GIF, MP4, WebM, PDF, and plain text.

### Health check

`GET /healthz` is public (it bypasses login and first-run setup) and returns `200 ok` when the database answers, or `503` when it does not. The Docker image ships a `HEALTHCHECK` that probes this endpoint.

### Behind a reverse proxy (Caddy)

```caddyfile
example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Run the app with `HOST=127.0.0.1` and `COOKIE_SECURE=1`. Caddy sets `X-Real-IP` to the real client address on proxied requests, which Disjoint's login/register rate limiter uses (when connections arrive from loopback).

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

## License

[Apache License 2.0](LICENSE)
