package worker

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"disjoint/internal/database"
	"disjoint/internal/thumbs"
)

// Worker handles periodic cleanup of expired threads and their media files.
type Worker struct {
	queries   *database.Queries
	uploadDir string
	interval  time.Duration
}

// New creates a new Worker instance.
func New(queries *database.Queries, uploadDir string, interval time.Duration) *Worker {
	if interval <= 0 {
		interval = 1 * time.Minute
	}
	return &Worker{
		queries:   queries,
		uploadDir: uploadDir,
		interval:  interval,
	}
}

// Start launches the cleanup worker in a background goroutine.
func (w *Worker) Start(ctx context.Context) {
	go func() {
		log.Printf("[worker] Expired thread cleanup worker started (interval: %v)", w.interval)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Println("[worker] Expired thread cleanup worker stopping")
				return
			case <-ticker.C:
				if err := w.CleanupOnce(ctx); err != nil {
					log.Printf("[worker] Error cleaning up expired threads: %v", err)
				}
			}
		}
	}()
}

// CleanupOnce performs a single pass of identifying and deleting expired
// threads and their files.
func (w *Worker) CleanupOnce(ctx context.Context) error {
	// 1. Collect the attachment files that belong to expired threads.
	attachments, err := w.queries.GetAttachmentsForExpiredThreads(ctx)
	if err != nil {
		return err
	}

	// 2. Delete the rows first (cascades to posts, attachments, reads). The
	// order matters: if file deletion then fails, we are left with orphaned
	// files (harmless, recoverable) — never attachment rows pointing at
	// files that no longer exist.
	if err := w.queries.DeleteExpiredThreads(ctx); err != nil {
		return err
	}

	// 3. Remove physical files from disk, including every cached thumbnail.
	// Thumbnails are derived and unversioned in the DB, so they must follow
	// their original out the door here.
	for _, att := range attachments {
		if att.StoragePath != "" {
			name := filepath.Base(filepath.Clean(att.StoragePath))
			if name == "." || name == ".." || name == "" {
				continue
			}
			fullPath := filepath.Join(w.uploadDir, name)
			if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
				log.Printf("[worker] Warning: could not delete expired attachment file %s: %v", fullPath, err)
			}
			thumbs.Remove(w.uploadDir, att.StoragePath)
		}
	}

	// 4. Prune expired one-time login links.
	if err := w.queries.DeleteExpiredLoginTokens(ctx, time.Now().Unix()); err != nil {
		log.Printf("[worker] Warning: could not prune expired login links: %v", err)
	}

	// 5. Truncate the WAL so pages zeroed by secure_delete do not linger in
	// the write-ahead log. Busy databases simply retry on the next tick.
	if err := w.queries.CheckpointWAL(ctx); err != nil {
		log.Printf("[worker] Warning: could not checkpoint WAL: %v", err)
	}

	// 6. Prune expired sessions. Best-effort: leftover expired rows are
	// harmless (GetSessionUserID checks absolute and idle expiry) and the next tick retries.
	if err := w.queries.PruneExpiredSessions(ctx, time.Now().Unix()); err != nil {
		log.Printf("[worker] Warning: could not prune expired sessions: %v", err)
	}

	return nil
}
