.PHONY: all build run test test-race generate fmt vet lint docker-build docker-up docker-down clean help

BIN_DIR ?= bin
BIN_NAME ?= disjoint
BINARY := $(BIN_DIR)/$(BIN_NAME)
MAIN_PKG := ./cmd/disjoint

PORT ?= 8080

all: build

## build: Compile a standalone static binary without CGO dependencies
build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags="-s -w" -o $(BINARY) $(MAIN_PKG)
	@echo "Built binary at $(BINARY)"

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
