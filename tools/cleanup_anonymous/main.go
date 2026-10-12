// Command cleanup_anonymous deletes the files on disk that belong to legacy
// anonymous content (posts/threads stored with user_id IS NULL before
// migration 00006).
//
// Migration 00006_drop_and_purge_anonymous.sql purges the database rows but
// cannot touch the filesystem; once the attachment rows are gone the
// storage_path -> file mapping is lost. Run this tool BEFORE first starting
// the app with migration 00006, or the files are left orphaned in UPLOAD_DIR.
//
// Usage:
//
//	DATABASE_PATH=disjoint.db UPLOAD_DIR=uploads go run ./tools/cleanup_anonymous           # dry-run
//	DATABASE_PATH=disjoint.db UPLOAD_DIR=uploads go run ./tools/cleanup_anonymous -yes      # delete
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const purgeQuery = `
SELECT a.storage_path
FROM attachments a
JOIN posts p ON a.post_id = p.id
WHERE p.user_id IS NULL
   OR p.thread_id IN (SELECT id FROM threads WHERE user_id IS NULL)`

func main() {
	log.SetFlags(0)

	dbPath := flag.String("db", envOr("DATABASE_PATH", "disjoint.db"), "path to the SQLite database")
	dir := flag.String("dir", envOr("UPLOAD_DIR", "uploads"), "upload directory containing the files")
	yes := flag.Bool("yes", false, "actually delete the files (default: dry-run)")
	flag.Parse()

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Refuse to run after migration 00006 has purged the rows: the
	// attachment rows (and therefore the file mapping) would already be gone.
	var gooseVer int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied = 1`).Scan(&gooseVer); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			log.Printf("note: no goose_db_version table; assuming migration 00006 has not been applied")
		} else {
			log.Fatalf("check goose_db_version: %v", err)
		}
	} else if gooseVer >= 6 {
		log.Fatalf("migration 00006 has already been applied (goose version %d); the attachment rows are gone and the file mapping is unrecoverable — the files, if any remain in %s, must be cleaned up manually", gooseVer, *dir)
	}

	rows, err := db.Query(purgeQuery)
	if err != nil {
		log.Fatalf("query attachment paths: %v", err)
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			log.Fatalf("scan: %v", err)
		}
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Fatalf("iterate: %v", err)
	}

	if len(paths) == 0 {
		fmt.Println("No anonymous attachments found; nothing to do.")
		return
	}

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatalf("resolve upload dir: %v", err)
	}

	fmt.Printf("%d file(s) belong to legacy anonymous content:\n", len(paths))
	for _, p := range paths {
		full := filepath.Join(absDir, filepath.Clean("/"+p))
		// Never delete outside the upload directory.
		if !strings.HasPrefix(full, absDir+string(os.PathSeparator)) {
			log.Fatalf("refusing to delete %s: resolves outside %s", p, absDir)
		}
		if !*yes {
			fmt.Println("  (dry-run) would delete:", full)
			continue
		}
		if err := os.Remove(full); err != nil {
			if os.IsNotExist(err) {
				fmt.Println("  (missing, skipped):", full)
				continue
			}
			log.Fatalf("delete %s: %v", full, err)
		}
		fmt.Println("  deleted:", full)
	}
	if !*yes {
		fmt.Println("Dry-run only. Re-run with -yes to delete.")
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
