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

	// Seed default categories and boards if none exist
	ctx := context.Background()
	cats, err := queries.ListCategories(ctx)
	if err == nil && len(cats) == 0 {
		c1, err := queries.CreateCategory(ctx, CreateCategoryParams{
			Name:     "General",
			Position: 0,
		})
		if err == nil {
			_, _ = queries.CreateBoard(ctx, CreateBoardParams{
				CategoryID:  c1.ID,
				Name:        "General Discussion",
				Description: sql.NullString{String: "A cozy spot for all kinds of conversations.", Valid: true},
				Position:    0,
			})
			_, _ = queries.CreateBoard(ctx, CreateBoardParams{
				CategoryID:  c1.ID,
				Name:        "Introductions & Lounge",
				Description: sql.NullString{String: "Say hello and kick back with friends.", Valid: true},
				Position:    1,
			})
		}

		c2, err := queries.CreateCategory(ctx, CreateCategoryParams{
			Name:     "Media & Links",
			Position: 1,
		})
		if err == nil {
			_, _ = queries.CreateBoard(ctx, CreateBoardParams{
				CategoryID:  c2.ID,
				Name:        "Photos & Videos",
				Description: sql.NullString{String: "Share pictures, clips, and favorite moments.", Valid: true},
				Position:    0,
			})
			_, _ = queries.CreateBoard(ctx, CreateBoardParams{
				CategoryID:  c2.ID,
				Name:        "Interesting Finds",
				Description: sql.NullString{String: "Cool links, articles, and recommendations.", Valid: true},
				Position:    1,
			})
		}
	}

	return conn, queries, nil
}
