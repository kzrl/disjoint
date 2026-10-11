# Multi-stage Dockerfile for Disjoint
# Stage 1: Build static binary
FROM golang:1.24-alpine AS builder

WORKDIR /src

RUN apk add --no-cache ca-certificates tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /disjoint ./cmd/disjoint

# Stage 2: Minimal runtime image
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app

COPY --from=builder /disjoint /app/disjoint

# Create data directory for SQLite database and uploads
RUN mkdir -p /data/uploads

ENV PORT=8080 \
    DATABASE_PATH=/data/disjoint.db \
    UPLOAD_DIR=/data/uploads \
    SESSION_SECRET=""

VOLUME ["/data"]

EXPOSE 8080

ENTRYPOINT ["/app/disjoint"]
