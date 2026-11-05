.PHONY: all build run test test-race generate fmt vet lint docker-build docker-up docker-down clean help css

BIN_DIR ?= bin
# Local Bulma checkout used by `make css` (see below).
BULMA_DIR ?= $(HOME)/src/bulma
BIN_NAME ?= disjoint
BINARY := $(BIN_DIR)/$(BIN_NAME)
MAIN_PKG := ./cmd/disjoint

PORT ?= 8080
BACKUP_DIR ?= backups
FROM ?=

# Short git hash stamped into the binary (shown in the footer); "dev" when
# the tree is not a git checkout (e.g. a source tarball).
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

all: build

## build: Compile a standalone static binary without CGO dependencies
build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags="-s -w -X disjoint/internal/buildinfo.Commit=$(GIT_COMMIT)" -o $(BINARY) $(MAIN_PKG)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/disjoint-backup ./cmd/disjoint-backup
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/disjoint-restore ./cmd/disjoint-restore
	@echo "Built binary at $(BINARY) (commit $(GIT_COMMIT)) plus backup/restore tools in $(BIN_DIR)/"

## run: Run the application locally (override port with PORT=xxxx)
run:
	PORT=$(PORT) go run $(MAIN_PKG)

## test: Run automated tests
test:
	go test -v ./...

## test-race: Run tests with data race detection
test-race:
	go test -race ./...

## generate: Regenerate sqlc Go code from db/schema.sql and query.sql
generate:
	sqlc generate

## css: Re-vendor Bulma from BULMA_DIR and regenerate web/static/disjoint.css
##      (dark-first theme override + web/css/site.css). Bump BULMA_DIR when
##      upgrading Bulma; the vendored copy is the only one the app uses.
css:
	@cp "$(BULMA_DIR)/css/bulma.min.css" web/static/bulma.min.css
	@cp "$(BULMA_DIR)/LICENSE" web/static/bulma-LICENSE
	@sh web/css/gen-theme.sh "$(BULMA_DIR)/css/bulma.css" > /tmp/disjoint-theme.css
	@cat /tmp/disjoint-theme.css web/css/site.css > web/static/disjoint.css
	@echo "Regenerated web/static/disjoint.css ($(BULMA_DIR)/css + web/css/site.css)"

# DB migrations are managed by goose (db/migrations/, single-file format with
# "-- +goose Up"/"-- +goose Down"). db/schema.sql is the cumulative schema snapshot
# used by sqlc -- keep it in sync when you add a migration. Create a new migration with:
#   goose -dir db/migrations sqlite "file:$(DATABASE_PATH)?_pragma=foreign_keys(1)" create <name> sql

## fmt: Format Go source code
fmt:
	gofmt -s -w .

## vet: Run go vet static analysis
vet:
	go vet ./...

## lint: Check formatting and run go vet
lint: vet
	@test -z "$$(gofmt -s -l .)" || (echo "Unformatted files:" && gofmt -s -l . && exit 1)

## backup: Snapshot the database and uploads into BACKUP_DIR (default backups/)
backup:
	BACKUP_DIR=$(BACKUP_DIR) go run ./cmd/disjoint-backup

## restore: Restore FROM=backups/<dir> (destructive; stop the app first)
restore:
	@test -n "$(FROM)" || (echo "usage: make restore FROM=backups/<dir>" && exit 1)
	go run ./cmd/disjoint-restore -from "$(FROM)" -force

## docker-build: Build Docker container image
docker-build:
	docker build -t disjoint .

## docker-up: Start container stack via docker compose
docker-up:
	docker compose up -d

## docker-down: Stop docker compose stack
docker-down:
	docker compose down

## clean: Remove build artifacts and temporary files
clean:
	rm -rf $(BIN_DIR)
	@echo "Cleaned build artifacts"

## help: Display available targets
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed -e 's/## //g' | awk 'BEGIN {FS = ": "}; {printf "  %-15s %s\n", $$1, $$2}'
