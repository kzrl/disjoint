package database

import (
	"database/sql"
	"fmt"
	"log"
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
		// The database holds password hashes, session/reset-token hashes, and
		// every private message. A new directory is private to the service
		// user; an existing one is left as the operator configured it.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, fmt.Errorf("failed to create database directory: %w", err)
		}
	}

	// secure_delete zeroes freed pages so expired/deleted posts are not left
	// recoverable in the database file. WAL is checkpointed by the worker.
	dsn := fmt.Sprintf("%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=secure_delete(1)", dbPath)
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

	secureDBFiles(dbPath)

	return conn, New(conn), nil
}

// secureDBFiles clamps the database and its SQLite sidecars to owner-only.
// New files follow the process umask, which is commonly 022, and this file is
// readable enough to be worth pinning explicitly. Missing sidecars are fine:
// they are created on first write and the process umask (see cmd/disjoint)
// keeps them private.
func secureDBFiles(dbPath string) {
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm", dbPath + "-journal"} {
		if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
			log.Printf("[database] warning: could not restrict %s permissions: %v", path, err)
		}
	}
}
