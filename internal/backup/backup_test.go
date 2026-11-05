package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func makeSourceDB(t *testing.T, path, marker string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE marker (value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO marker (value) VALUES (?)`, marker); err != nil {
		t.Fatal(err)
	}
}

func readMarker(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var marker string
	if err := db.QueryRow(`SELECT value FROM marker LIMIT 1`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	return marker
}

func TestCreateAndRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	dbPath := filepath.Join(source, "disjoint.db")
	uploadDir := filepath.Join(source, "uploads")
	makeSourceDB(t, dbPath, "original")

	if err := os.MkdirAll(filepath.Join(uploadDir, "thumbs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploadDir, "photo.jpg"), []byte("photo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploadDir, "thumbs", "photo.640.jpg"), []byte("thumb"), 0o644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(source, "backups")
	dir, err := Create(ctx, dbPath, uploadDir, outDir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, manifestName)); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	if got := readMarker(t, filepath.Join(dir, dbName)); got != "original" {
		t.Fatalf("backup marker = %q", got)
	}

	// The uploads archive must contain both the file and the cached thumb.
	f, err := os.Open(filepath.Join(dir, uploadsName))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[h.Name] = true
	}
	for _, want := range []string{"photo.jpg", "thumbs/photo.640.jpg"} {
		if !seen[want] {
			t.Errorf("archive missing %q, got %v", want, seen)
		}
	}

	// Restore into a fresh tree that already has different content.
	target := t.TempDir()
	newDB := filepath.Join(target, "data", "disjoint.db")
	newUploads := filepath.Join(target, "data", "uploads")
	if err := os.MkdirAll(filepath.Dir(newDB), 0o755); err != nil {
		t.Fatal(err)
	}
	makeSourceDB(t, newDB, "current")
	if err := os.MkdirAll(newUploads, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newUploads, "current.txt"), []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Restore(dir, newDB, newUploads, false); err == nil {
		t.Fatal("Restore without force should fail")
	}
	if got := readMarker(t, newDB); got != "current" {
		t.Fatalf("refused restore changed the database: %q", got)
	}

	if err := Restore(dir, newDB, newUploads, true); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := readMarker(t, newDB); got != "original" {
		t.Fatalf("restored marker = %q", got)
	}
	if _, err := os.Stat(filepath.Join(newUploads, "photo.jpg")); err != nil {
		t.Fatalf("restored upload missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newUploads, "current.txt")); !os.IsNotExist(err) {
		t.Fatalf("old upload survived the restore")
	}
	if matches, _ := filepath.Glob(newDB + ".pre-restore-*"); len(matches) == 0 {
		t.Fatal("expected a safety copy of the previous database")
	}
	if matches, _ := filepath.Glob(newUploads + ".pre-restore-*"); len(matches) == 0 {
		t.Fatal("expected a safety copy of the previous uploads")
	}
}

func TestRestoreRejectsUnsafeArchivePath(t *testing.T) {
	root := t.TempDir()
	from := filepath.Join(root, "backup")
	if err := os.MkdirAll(from, 0o755); err != nil {
		t.Fatal(err)
	}
	makeSourceDB(t, filepath.Join(from, dbName), "backup")

	// Hand-build a malicious archive.
	f, err := os.Create(filepath.Join(from, uploadsName))
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	err = Restore(from, filepath.Join(target, "disjoint.db"), filepath.Join(target, "uploads"), true)
	if err == nil {
		t.Fatal("expected unsafe archive path to be rejected")
	}
}
