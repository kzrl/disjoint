# Disjoint

**Disjoint** is a web application that brings the vibes from classic [internet forums](https://en.wikipedia.org/wiki/Internet_forum) [BBSes](https://en.wikipedia.org/wiki/Bulletin_board_system) and the [Cozy Web](https://maggieappleton.com/cozy-web). 

It's a place for the group chat to hang out on the web and share fun stuff. It's private and posts can expire after a specified amount of time.

Designed to be self hosted. It's not some corporate advertising funnel to mine your data. Run it on a cheap virtual server somewhere and bring your mates.

Built with Go, SQLite, sqlc, HTMX, and Bulma.

---

## Some of the features

- **Front Page**: Every thread lives on the front page, sorted by creation date (newest first). No weird algorithms designed to steal your attention.

- **Expiring Threads (TTL)**: Set lifespans for threads (24h, 3d, 7d, 30d, or Never). Every new comment pushes the deadline out by the chosen lifespan, so active discussions continue and old posts disappear.

- **Image Viewer**: Click any image in a thread to browse every image in that thread — on-screen arrows, ←/→ and Home/End, or a horizontal swipe on touch.

- **Image Gallery**: `/gallery` collects every image on threads.

- **@User Mentions**: `@username` tokens in posts (and bios) render as links to that user's card.

- **Slack-Style Reactions**: React to any post with built-in unicode emoji or custom community uploaded emoji

- **Passkeys**: Add Touch ID / Windows Hello / phone / security-key sign-in from `/account`, then sign in without typing a password. Passwords keep working, so losing a device never locks you out. Invited users can also create a **passkey-only account** (no password) from the invite page; those accounts can set a password later after confirming with a passkey. Requires HTTPS (or localhost) and the two `WEBAUTHN_*` variables below.

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

Images are published to GHCR by CI on pushes to `main` (tag `latest`) and on `v*` tags. You can also build the repository's own compose file locally with `docker compose up -d --build`.

### 2. Build and Run from Source

**Prerequisites**: Go 1.27.3+

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
| `PUBLIC_BASE_URL` | _(request host)_ | Public origin (e.g. `https://disjoint.example.com`) used for admin-issued reset/invite links; also the fallback source for the WebAuthn RP ID/origins |
| `WEBAUTHN_RP_ID` | _(derived from `PUBLIC_BASE_URL`, then `localhost`)_ | Relying Party ID for passkeys; must be the site's registrable domain (e.g. `disjoint.example.com`) |
| `WEBAUTHN_RP_ORIGINS` | _(derived from `PUBLIC_BASE_URL`, else `https://<RP ID>`)_ | Comma-separated origins allowed to use passkeys (e.g. `https://disjoint.example.com`) |

### Uploads

A single post (opening post or reply) accepts at most **16 attachments**, **50 MB per file**, and **100 MB combined**; any request body is capped at 128 MB. Accepted types are JPEG, PNG, WebP, GIF, MP4, WebM, PDF, and plain text.

### Health check

`GET /healthz` is public (it bypasses login and first-run setup) and returns `200 ok` when the database answers, or `503` when it does not. The Docker image ships a `HEALTHCHECK` that probes this endpoint.

### Backups

`disjoint-backup` writes a timestamped folder containing:

- `disjoint.db` — a transactionally consistent snapshot made with SQLite `VACUUM INTO`
- `uploads.tar.gz` — attachments, avatars, custom emoji, and cached thumbnails
- `manifest.json` — creation time and source paths

From a source checkout:

```bash
make backup                    # writes to ./backups/
BACKUP_DIR=/mnt/backups make backup
```

In Docker (the image sets `BACKUP_DIR=/data/backups`):

```bash
docker compose exec disjoint /app/disjoint-backup
# uses the exact path printed by the tool
docker cp disjoint:/data/backups/disjoint-<full-name> ./disjoint-backup
```

Keep a copy outside the host/volume. Backup folders are never pruned automatically.

#### Restore

**Stop the application first.** Restoring replaces the live database and uploads.

From a source checkout:

```bash
make restore FROM=backups/disjoint-<full-name>
```

In Docker:

```bash
docker compose stop disjoint
docker cp ./disjoint-backup disjoint:/data/backups/
docker compose run --rm --entrypoint /app/disjoint-restore disjoint \
  -from /data/backups/disjoint-backup -force
docker compose start disjoint
```

The restore tool validates the snapshot with `PRAGMA quick_check`, requires `-force`, and moves the previous database and uploads aside as `*.pre-restore-<stamp>` instead of deleting them. Delete those safety copies once the restore is verified.

### Security notes

- Session cookies are HttpOnly, SameSite=Lax, and Secure when TLS is detected or `COOKIE_SECURE=1` is set. Secure sessions use the `__Host-disjoint_session` name, so a sibling subdomain cannot plant a session cookie. Sessions have a 7-day idle timeout and a 30-day absolute lifetime; changing a password revokes every session.
- The database and its WAL/SHM sidecars are restricted to the service user (`0600`); the process runs with a `0077` umask, so newly created data files stay private. New uploads are written `0600` in a `0750` (or stricter) directory.
- The CSP is `script-src 'self'`: all page JavaScript lives in `/static`, there are no inline scripts or `onsubmit` handlers, and HTMX eval is disabled. Responses also carry `Permissions-Policy` and, over TLS, `Strict-Transport-Security`.
- The Compose service binds to `127.0.0.1` and sets `COOKIE_SECURE=1`; put a TLS-terminating proxy in front of it (see below).
- New invite links expire after seven days. Existing pre-migration invites with no expiry remain valid until used.
- Post bodies are capped at **256 KB**; attachments are capped at 100 MB per post, and each user gets a **2 GiB** attachment + custom-emoji quota.
- SQLite runs with `secure_delete` and the expiry worker truncates the WAL, but backups are **not encrypted** and retain content from before expiry. Copy backups only to encrypted storage and apply your own retention policy. Secure erasure on SSDs is never guaranteed.
- Uploaded media is served only to authenticated members, with `Cache-Control: private`. External Markdown images are blocked by CSP (`img-src 'self' data: blob:`; blob is for local paste previews).
- The app rate-limits posts, profile/emoji uploads, searches, autocomplete, reactions, and password changes per user.

### Behind a reverse proxy (Caddy)

```caddyfile
example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Run the app with `HOST=127.0.0.1` and `COOKIE_SECURE=1`. Caddy sets `X-Real-IP` to the real client address on proxied requests, which Disjoint's login/register rate limiter uses (when connections arrive from loopback). The proxy must preserve the original `Host` header (Caddy does by default): state-changing requests are checked against the request host as CSRF defense-in-depth.

---

## Running Tests

```bash
make test        # go test -v ./...
make test-race   # data-race detector
make vuln        # govulncheck (dependencies + called symbols)
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
| `cmd/disjoint-backup` / `cmd/disjoint-restore` | Backup and restore CLIs |
| `internal/` | Handlers, auth, avatars, backup, markdown, background worker |
| `db/` | `schema.sql` and sqlc query definitions |
| `web/` | Templates, vendored static assets (HTMX, Bulma) |
| `specs/` | Design specs (core app, user/admin flows) |


---

## License

[GNU General Public License v3.0](LICENSE)

Disjoint is free software: you can redistribute it and/or modify it under the
terms of the GNU General Public License as published by the Free Software
Foundation, either version 3 of the License, or (at your option) any later
version.

Disjoint is distributed in the hope that it will be useful, but WITHOUT ANY
WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR A
PARTICULAR PURPOSE. See the GNU General Public License for more details.

Vendored third-party assets keep their own licenses:

- Bulma v1 — MIT (see `web/static/bulma-LICENSE`)
- HTMX — BSD 2-Clause
