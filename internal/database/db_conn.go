package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"disjoint/db"
	"github.com/pressly/goose/v3"

	_ "modernc.org/sqlite"
)

// OpenDB opens a SQLite database connection with proper pragmas and applies migrations.
func OpenDB(dbPath string) (*sql.DB, *Queries, error) {
	dir := filepath.Dir(dbPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("failed to create database directory: %w", err)
		}
	}

	dsn := fmt.Sprintf("%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", dbPath)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// Ping database. The DSN's _pragma params apply per connection (the
	// only reliable way to set per-connection pragmas like foreign_keys
	// on a pool); journal_mode and busy_timeout are covered there too.
	if err := conn.Ping(); err != nil {
		return nil, nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Apply database migrations (goose manages the goose_db_version table).
	goose.SetBaseFS(db.MigrationsFS)
	if err := goose.SetDialect("sqlite"); err != nil {
		return nil, nil, fmt.Errorf("failed to set goose dialect: %w", err)
	}
	if err := goose.Up(conn, "migrations"); err != nil {
		return nil, nil, fmt.Errorf("failed to apply migrations: %w", err)
	}

	queries := New(conn)

	// Seed default boards if none exist
	ctx := context.Background()
	boards, err := queries.ListBoards(ctx)
	if err == nil && len(boards) == 0 {
		seed := []struct {
			name, desc string
			position   int64
		}{
			{"General Discussion", "A cozy spot for all kinds of conversations.", 0},
			{"Introductions & Lounge", "Say hello and kick back with friends.", 1},
			{"Photos & Videos", "Share pictures, clips, and favorite moments.", 2},
			{"Interesting Finds", "Cool links, articles, and recommendations.", 3},
		}
		for _, b := range seed {
			_, _ = queries.CreateBoard(ctx, CreateBoardParams{
				Name:        b.name,
				Description: sql.NullString{String: b.desc, Valid: true},
				Position:    b.position,
			})
		}
	}

	return conn, queries, nil
}
