// Package backup creates and restores consistent on-disk backups of the
// SQLite database and uploads directory. The database snapshot uses SQLite's
// VACUUM INTO, which is safe to run against a live WAL database and produces a
// single compact file; uploads are archived as a gzip-compressed tar.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	manifestName = "manifest.json"
	dbName       = "disjoint.db"
	uploadsName  = "uploads.tar.gz"
)

// DatabaseName / UploadsName are the public filenames inside a backup folder.
const (
	DatabaseName = dbName
	UploadsName  = uploadsName
)

// Manifest describes one backup directory.
type Manifest struct {
	CreatedAt      time.Time `json:"created_at"`
	Database       string    `json:"database"`
	Uploads        string    `json:"uploads"`
	DatabaseSource string    `json:"database_source"`
	UploadSource   string    `json:"upload_source"`
}

// Create writes a timestamped backup directory under outDir and returns its
// path. The source database must exist; a missing uploads directory is treated
// as empty (a backup is still useful before the first upload).
func Create(ctx context.Context, dbPath, uploadDir, outDir string) (string, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return "", fmt.Errorf("database %s: %w", dbPath, err)
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return "", fmt.Errorf("create backup directory: %w", err)
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	dir, err := os.MkdirTemp(outDir, "disjoint-"+stamp+"-")
	if err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	dbTarget := filepath.Join(dir, dbName)
	if err := snapshotDB(ctx, dbPath, dbTarget); err != nil {
		cleanup()
		return "", err
	}
	if err := os.Chmod(dbTarget, 0o600); err != nil {
		cleanup()
		return "", err
	}

	uploadsTarget := filepath.Join(dir, uploadsName)
	if err := archiveUploads(uploadDir, uploadsTarget); err != nil {
		cleanup()
		return "", err
	}

	manifest := Manifest{
		CreatedAt:      time.Now().UTC(),
		Database:       dbName,
		Uploads:        uploadsName,
		DatabaseSource: dbPath,
		UploadSource:   uploadDir,
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		cleanup()
		return "", err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, manifestName), data, 0o600); err != nil {
		cleanup()
		return "", err
	}

	return dir, nil
}

// snapshotDB uses VACUUM INTO so the copy is transactionally consistent even
// while the application is writing (WAL + busy timeout).
func snapshotDB(ctx context.Context, source, target string) error {
	db, err := sql.Open("sqlite", source)
	if err != nil {
		return fmt.Errorf("open source database: %w", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		return fmt.Errorf("set busy timeout: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping source database: %w", err)
	}

	_ = os.Remove(target) // VACUUM INTO refuses an existing target
	quoted := strings.ReplaceAll(target, "'", "''")
	if _, err := db.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
		return fmt.Errorf("snapshot database: %w", err)
	}
	return checkDB(target)
}

func checkDB(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open snapshot: %w", err)
	}
	defer db.Close()

	var result string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("check snapshot: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("snapshot integrity check failed: %s", result)
	}
	return nil
}

func archiveUploads(uploadDir, target string) error {
	f, err := os.Create(target)
	if err != nil {
		return fmt.Errorf("create uploads archive: %w", err)
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	var walkErr error
	if _, err := os.Stat(uploadDir); err == nil {
		walkErr = filepath.WalkDir(uploadDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == uploadDir {
				return nil
			}
			// Symlinks are not followed: a backup must never pull arbitrary
			// files from outside the uploads tree.
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(uploadDir, path)
			if err != nil {
				return err
			}
			header.Name = filepath.ToSlash(rel)
			if d.IsDir() {
				header.Name += "/"
			}
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			src, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, src)
			closeErr := src.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		})
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read uploads directory: %w", err)
	}

	if walkErr != nil {
		return fmt.Errorf("archive uploads: %w", walkErr)
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Close()
}

// Restore replaces the database and uploads directory from a backup created by
// Create. It refuses to run unless force is true. The old database and uploads
// are moved aside with a .pre-restore-<stamp> suffix rather than deleted, so a
// mistaken restore can be reversed.
//
// The caller MUST stop the application first: replacing a database under a live
// process is not safe.
func Restore(fromDir, dbPath, uploadDir string, force bool) error {
	if !force {
		return fmt.Errorf("restore is destructive; pass -force after stopping the application")
	}
	srcDB := filepath.Join(fromDir, dbName)
	srcUploads := filepath.Join(fromDir, uploadsName)
	if _, err := os.Stat(srcDB); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	if _, err := os.Stat(srcUploads); err != nil {
		return fmt.Errorf("backup uploads archive: %w", err)
	}
	if err := checkDB(srcDB); err != nil {
		return fmt.Errorf("backup database is not usable: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(uploadDir), 0o755); err != nil {
		return err
	}

	stamp := time.Now().UTC().Format("20060102-150405")

	// Extract uploads into a temporary sibling first, so a corrupt archive
	// cannot destroy the current uploads tree.
	tmpUploads, err := os.MkdirTemp(filepath.Dir(uploadDir), ".uploads-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpUploads)
	if err := extractUploads(srcUploads, tmpUploads); err != nil {
		return err
	}
	if err := os.Chmod(tmpUploads, 0o755); err != nil {
		return err
	}

	tmpDB, err := os.CreateTemp(filepath.Dir(dbPath), ".restore-*.db")
	if err != nil {
		return err
	}
	tmpDBPath := tmpDB.Name()
	defer os.Remove(tmpDBPath)
	if err := copyFile(srcDB, tmpDB); err != nil {
		tmpDB.Close()
		return err
	}
	if err := tmpDB.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpDBPath, 0o600); err != nil {
		return err
	}

	// Swap uploads.
	oldUploads := ""
	if _, err := os.Stat(uploadDir); err == nil {
		oldUploads = uploadDir + ".pre-restore-" + stamp
		if err := os.Rename(uploadDir, oldUploads); err != nil {
			return fmt.Errorf("move current uploads aside: %w", err)
		}
	}
	if err := os.Rename(tmpUploads, uploadDir); err != nil {
		if oldUploads != "" {
			_ = os.Rename(oldUploads, uploadDir)
		}
		return fmt.Errorf("install restored uploads: %w", err)
	}
	defer func() {
		// Remove the deferred cleanup target: it no longer exists after the
		// successful rename, so RemoveAll is a harmless no-op.
		_ = os.RemoveAll(tmpUploads)
	}()

	// Swap database. Move the old WAL/SHM sidecars with the old database so
	// SQLite cannot replay them over the restored file.
	oldDB := ""
	if _, err := os.Stat(dbPath); err == nil {
		oldDB = dbPath + ".pre-restore-" + stamp
		if err := os.Rename(dbPath, oldDB); err != nil {
			_ = os.Rename(uploadDir, tmpUploads)
			if oldUploads != "" {
				_ = os.Rename(oldUploads, uploadDir)
			}
			return fmt.Errorf("move current database aside: %w", err)
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			if _, err := os.Stat(dbPath + suffix); err == nil {
				_ = os.Rename(dbPath+suffix, oldDB+suffix)
			}
		}
	}
	if err := os.Rename(tmpDBPath, dbPath); err != nil {
		// Roll both trees back so a failed restore leaves the previous
		// installation intact.
		_ = os.RemoveAll(uploadDir)
		if oldUploads != "" {
			_ = os.Rename(oldUploads, uploadDir)
		}
		if oldDB != "" {
			_ = os.Rename(oldDB, dbPath)
			for _, suffix := range []string{"-wal", "-shm"} {
				if _, statErr := os.Stat(oldDB + suffix); statErr == nil {
					_ = os.Rename(oldDB+suffix, dbPath+suffix)
				}
			}
		}
		return fmt.Errorf("install restored database: %w", err)
	}

	return nil
}

func extractUploads(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(header.Name)
		if name == "." || filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(filepath.Separator)) || name == ".." {
			return fmt.Errorf("unsafe path in uploads archive: %q", header.Name)
		}
		target := filepath.Join(dest, name)
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(filepath.Separator)) {
			return fmt.Errorf("unsafe path in uploads archive: %q", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			// Skip devices, sockets, etc. A backup only ever writes regular
			// files and directories.
		}
	}
}

func copyFile(src string, dst *os.File) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if _, err := io.Copy(dst, in); err != nil {
		return err
	}
	return dst.Sync()
}

// List returns backup directory names under outDir, newest first. It is a
// convenience for the CLI and for operators who keep multiple snapshots.
func List(outDir string) ([]string, error) {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "disjoint-") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}
