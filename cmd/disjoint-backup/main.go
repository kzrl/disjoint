// Command disjoint-backup writes a consistent timestamped snapshot of the
// SQLite database and the uploads directory.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"disjoint/internal/backup"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	dbPath := flag.String("db", env("DATABASE_PATH", "disjoint.db"), "SQLite database path")
	uploadDir := flag.String("uploads", env("UPLOAD_DIR", "uploads"), "uploads directory")
	outDir := flag.String("out", env("BACKUP_DIR", "backups"), "directory that receives timestamped backup folders")
	flag.Parse()

	dir, err := backup.Create(context.Background(), *dbPath, *uploadDir, *outDir)
	if err != nil {
		log.Fatalf("[backup] %v", err)
	}
	fmt.Printf("Backup written to %s\n", dir)
	fmt.Printf("  database: %s\n", dir+"/"+backup.DatabaseName)
	fmt.Printf("  uploads:  %s\n", dir+"/"+backup.UploadsName)
	fmt.Printf("Restore with: disjoint-restore -from %s -force (stop the app first)\n", dir)
}
