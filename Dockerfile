# Multi-stage Dockerfile for Disjoint
# Stage 1: Build static binary
FROM golang:1.27.3-alpine AS builder

WORKDIR /src

RUN apk add --no-cache ca-certificates tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN mkdir -p /out && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/disjoint ./cmd/disjoint \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/disjoint-backup ./cmd/disjoint-backup \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/disjoint-restore ./cmd/disjoint-restore

# Stage 2: Minimal runtime image
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 disjoint \
    && adduser -S -u 10001 -G disjoint disjoint

WORKDIR /app

COPY --from=builder /out/disjoint /app/disjoint
COPY --from=builder /out/disjoint-backup /app/disjoint-backup
COPY --from=builder /out/disjoint-restore /app/disjoint-restore
COPY LICENSE /app/LICENSE

# Create the data directory before declaring the volume. Named volumes created
# from this image inherit these permissions, so the non-root process can write
# the database and uploads. Bind mounts must be writable by UID 10001.
RUN mkdir -p /data/uploads && chmod 0750 /data/uploads && chown -R disjoint:disjoint /data

ENV PORT=8080 \
    DATABASE_PATH=/data/disjoint.db \
    UPLOAD_DIR=/data/uploads \
    BACKUP_DIR=/data/backups

VOLUME ["/data"]

EXPOSE 8080

USER disjoint

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${PORT:-8080}/healthz" || exit 1

ENTRYPOINT ["/app/disjoint"]
