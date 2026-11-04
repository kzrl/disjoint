package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"disjoint/internal/database"
)

func TestHealthzIsPublicBeforeSetup(t *testing.T) {
	app, db, _, _ := setupTestApp(t)
	defer db.Close()

	// No users exist yet: the health probe must not be redirected to /setup.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 from /healthz, got %d", rr.Code)
	}
	if body := strings.TrimSpace(rr.Body.String()); body != "ok" {
		t.Fatalf("expected body %q, got %q", "ok", body)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("expected Cache-Control no-store, got %q", got)
	}
}

func TestHealthzReportsDatabaseFailure(t *testing.T) {
	app, db, _, _ := setupTestApp(t)
	if err := db.Close(); err != nil {
		t.Fatalf("failed to close db: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 from /healthz with a closed DB, got %d", rr.Code)
	}
}

// TestUploadAttachmentLimit posts more than the per-post attachment cap and
// asserts the request is rejected before any thread row or file survives.
func TestUploadAttachmentLimit(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()

	user, err := queries.CreateUser(context.Background(), database.CreateUserParams{
		Username:     "uploader",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	cookie := authCookieFor(t, queries, user.ID)

	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("title", "too many attachments")
	_ = w.WriteField("body", "body")
	image := realPNG(t, 2, 2)
	for i := 0; i < 17; i++ { // one over maxUploadFilesPerPost
		part, err := w.CreateFormFile("files", fmt.Sprintf("image-%d.png", i))
		if err != nil {
			t.Fatalf("failed to create form file: %v", err)
		}
		if _, err := part.Write(image); err != nil {
			t.Fatalf("failed to write form file: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/threads/new", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect back to the form, got %d", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "/threads/new") {
		t.Fatalf("expected redirect to /threads/new, got %s", loc)
	}
	assertNoThreadsOrUploads(t, db, uploadDir)
}

// TestRejectedThreadRemovesUploadedFiles covers the validation path after the
// files have already been written: a bad link must not leave orphaned media.
func TestRejectedThreadRemovesUploadedFiles(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()

	user, err := queries.CreateUser(context.Background(), database.CreateUserParams{
		Username:     "uploader2",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	cookie := authCookieFor(t, queries, user.ID)

	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("title", "bad link thread")
	_ = w.WriteField("body", "body")
	_ = w.WriteField("url", "javascript:alert(1)")
	part, err := w.CreateFormFile("files", "image.png")
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	if _, err := part.Write(realPNG(t, 2, 2)); err != nil {
		t.Fatalf("failed to write form file: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/threads/new", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect back to the form, got %d", rr.Code)
	}
	assertNoThreadsOrUploads(t, db, uploadDir)
}

func assertNoThreadsOrUploads(t *testing.T, db *sql.DB, uploadDir string) {
	t.Helper()

	var threads int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&threads); err != nil {
		t.Fatalf("failed to count threads: %v", err)
	}
	if threads != 0 {
		t.Fatalf("expected no threads, got %d", threads)
	}
	if files := listUploadFiles(t, uploadDir); len(files) != 0 {
		t.Fatalf("expected no stored files, got %v", files)
	}
}
