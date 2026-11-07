package worker

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"disjoint/internal/database"
)

// Abandoned passkey signups hold a username but are not accounts; the sweeper
// must delete them and leave real users alone.
func TestCleanupDeletesExpiredPendingUsers(t *testing.T) {
	db, queries, err := database.OpenDB(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	active, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "active_user", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	expired, err := queries.CreatePendingUser(ctx, database.CreatePendingUserParams{
		Username:     "expired_pending",
		PasswordHash: "h",
		PendingUntil: sql.NullTime{Time: time.Now().UTC().Add(-time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := queries.CreatePendingUser(ctx, database.CreatePendingUserParams{
		Username:     "fresh_pending",
		PasswordHash: "h",
		PendingUntil: sql.NullTime{Time: time.Now().UTC().Add(5 * time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	w := New(queries, t.TempDir(), time.Minute)
	if err := w.CleanupOnce(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if _, err := queries.GetUserByID(ctx, expired.ID); err == nil {
		t.Fatalf("expired pending user survived cleanup")
	}
	if _, err := queries.GetUserByID(ctx, fresh.ID); err != nil {
		t.Errorf("unexpired pending user was deleted: %v", err)
	}
	if _, err := queries.GetUserByID(ctx, active.ID); err != nil {
		t.Errorf("active user was deleted: %v", err)
	}

	// Pending rows also never count toward first-run setup.
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE pending_until IS NULL`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("active user count = %d, want 1", count)
	}
}
