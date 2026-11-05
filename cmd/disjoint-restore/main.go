// Command disjoint-restore replaces the SQLite database and uploads directory
// from a backup made by disjoint-backup. It refuses to run without -force.
// Stop the application before running it.
package main

import (
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
	from := flag.String("from", "", "backup directory created by disjoint-backup (required)")
	dbPath := flag.String("db", env("DATABASE_PATH", "disjoint.db"), "SQLite database path")
	uploadDir := flag.String("uploads", env("UPLOAD_DIR", "uploads"), "uploads directory")
	force := flag.Bool("force", false, "confirm the destructive restore")
	flag.Parse()

	if *from == "" {
		fmt.Fprintln(os.Stderr, "usage: disjoint-restore -from <backup-dir> -force")
		os.Exit(2)
	}
	if !*force {
		log.Fatal("[restore] refusing to run without -force (stop the application first)")
	}
	if err := backup.Restore(*from, *dbPath, *uploadDir, *force); err != nil {
		log.Fatalf("[restore] %v", err)
	}
	fmt.Printf("Restored database from %s\n", *from)
	fmt.Printf("Previous database and uploads (if any) were kept with a .pre-restore-* suffix.\n")
}
