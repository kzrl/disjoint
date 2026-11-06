package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"hash/crc32"
	"image"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"disjoint/internal/auth"
	"disjoint/internal/database"
)

// hugePNGDimensions is a valid PNG header declaring an enormous image. The
// decoder must reject it from the header without allocating the pixels.
func hugePNG(w, h uint32) []byte {
	sig := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], w)
	binary.BigEndian.PutUint32(ihdr[4:8], h)
	ihdr[8] = 8  // bit depth
	ihdr[9] = 6  // RGBA
	ihdr[10] = 0 // compression
	ihdr[11] = 0 // filter
	ihdr[12] = 0 // interlace
	chunk := func(typ string, data []byte) []byte {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.BigEndian, uint32(len(data)))
		b.WriteString(typ)
		b.Write(data)
		crc := crc32.NewIEEE()
		_, _ = crc.Write([]byte(typ))
		_, _ = crc.Write(data)
		_ = binary.Write(&b, binary.BigEndian, crc.Sum32())
		return b.Bytes()
	}
	out := append([]byte{}, sig...)
	out = append(out, chunk("IHDR", ihdr)...)
	out = append(out, chunk("IEND", nil)...)
	return out
}

func TestFaviconDoesNotLeakIndex(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	if _, err := queries.CreateUser(context.Background(), database.CreateUserParams{
		Username: "favicon_user", PasswordHash: "hash",
	}); err != nil {
		t.Fatal(err)
	}
	router := app.Routes()

	for _, target := range []string{"/favicon.ico", "/favicon.ico?q=secret&page=2"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		// The icon itself is a public asset now; the point of this test is
		// that the path never renders the authenticated front page.
		if rr.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", target, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
			t.Fatalf("%s Content-Type = %q, want image/png", target, ct)
		}
		if strings.Contains(strings.ToLower(rr.Body.String()), "<html") {
			t.Fatalf("%s leaked the index page", target)
		}
	}
}

func TestPostBodyLimit(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	user, err := queries.CreateUser(context.Background(), database.CreateUserParams{
		Username: "body_limit_user", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)

	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("title", "too big")
	_ = w.WriteField("body", strings.Repeat("x", 256*1024+1))
	_ = w.WriteField("expiration", "never")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/threads/new", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("oversized body = %d, want redirect", rr.Code)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("oversized body created %d threads", count)
	}
}

func TestStorageQuotaRejectsUpload(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()
	user, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username: "quota_user", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{UserID: user.ID, Title: "quota"})
	if err != nil {
		t.Fatal(err)
	}
	post, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: thread.ID, UserID: user.ID, Body: "opening"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateAttachment(ctx, database.CreateAttachmentParams{
		PostID:      post.ID,
		Filename:    "big.bin",
		StoragePath: "big.bin",
		MimeType:    "application/pdf",
		FileSize:    2 << 30, // at the quota limit; any new byte must be rejected
	}); err != nil {
		t.Fatal(err)
	}

	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("title", "quota thread")
	_ = w.WriteField("body", "body")
	part, err := w.CreateFormFile("files", "small.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(realPNG(t, 2, 2)); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, "/threads/new", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("quota upload = %d, want redirect", rr.Code)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("quota upload created a thread: count=%d", count)
	}
	if entries, err := os.ReadDir(uploadDir); err == nil && len(entries) != 0 {
		t.Fatalf("quota upload left files: %v", entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestEmojiAndAvatarRejectOversizedDimensions(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	user, err := queries.CreateUser(context.Background(), database.CreateUserParams{
		Username: "image_bomb_user", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)

	bomb := hugePNG(100000, 100000)
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(bomb)); err != nil || cfg.Width != 100000 {
		t.Fatalf("crafted PNG header did not parse as expected: %+v %v", cfg, err)
	}

	// Emoji upload must reject from the header.
	body, ctype := emojiMultipart(t, "bomb", "bomb.png", bomb)
	req := httptest.NewRequest(http.MethodPost, "/emojis", body)
	req.Header.Set("Content-Type", ctype)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("oversized emoji = %d", rr.Code)
	}
	var emojiCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM emojis WHERE name = 'bomb'`).Scan(&emojiCount); err != nil {
		t.Fatal(err)
	}
	if emojiCount != 0 {
		t.Fatal("oversized emoji was stored")
	}

	// Avatar upload must reject from the header too.
	body, ctype = profileMultipart(t, map[string]string{"bio": "x"}, "bomb.png", bomb)
	req = httptest.NewRequest(http.MethodPost, "/users/me/profile", body)
	req.Header.Set("Content-Type", ctype)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("oversized avatar = %d", rr.Code)
	}
	if entries, err := os.ReadDir(uploadDir); err == nil && len(entries) != 0 {
		t.Fatalf("oversized image left files: %v", entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestExpiredThreadCannotBeExtended(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()
	user, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username: "expiry_user", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)
	thread, err := queries.CreateThread(ctx, database.CreateThreadParams{
		UserID: user.ID,
		Title:  "expired",
		ExpiresAt: sql.NullTime{
			Time:  time.Now().UTC().Add(-time.Hour),
			Valid: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: thread.ID, UserID: user.ID, Body: "opening",
	}); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"expiration": {"never"}, "page": {"1"}}
	req := httptest.NewRequest(http.MethodPost, "/threads/"+strconv.FormatInt(thread.ID, 10)+"/expiration", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	// GetThreadByID filters expired threads, so the handler returns 404 before
	// the defense-in-depth 410 check. Either way the thread must not change.
	if rr.Code != http.StatusNotFound {
		t.Fatalf("extending an expired thread = %d, want 404", rr.Code)
	}
	var storedExpiry sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT expires_at FROM threads WHERE id = ?`, thread.ID).Scan(&storedExpiry); err != nil {
		t.Fatal(err)
	}
	if !storedExpiry.Valid || storedExpiry.Time.After(time.Now().UTC()) {
		t.Fatal("expired thread was revived")
	}
}

func TestExpiredInviteIsRejected(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()
	admin, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username: "invite_admin", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	invite, err := queries.CreateInvite(ctx, database.CreateInviteParams{
		Code: "expired-code", CreatedBy: admin.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE invites SET expires_at = datetime('now', '-1 day') WHERE id = ?`, invite.ID); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"code": {invite.Code}, "username": {"inviteduser"}, "password": {"password123"}}
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expired invite registration = %d", rr.Code)
	}
	if _, err := queries.GetUserByUsername(ctx, "inviteduser"); err == nil {
		t.Fatal("expired invite created a user")
	}
}

func TestIdleSessionIsRejected(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	ctx := context.Background()
	user, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username: "idle_user", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET last_used_at = ? WHERE token_hash = ?`,
		time.Now().Add(-auth.SessionIdleTimeout-time.Hour).Unix(), auth.TokenHash(cookie.Value)); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("idle session = %d to %s, want login", rr.Code, rr.Header().Get("Location"))
	}
}

func TestUploadServingRejectsNonCanonicalName(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	user, err := queries.CreateUser(context.Background(), database.CreateUserParams{
		Username: "path_user", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploadDir, "ok.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/uploads/..", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	// ServeMux may clean the path and redirect before the handler; the key
	// invariant is that no upload bytes are served.
	if rr.Code == http.StatusOK {
		t.Fatalf("non-canonical upload name was served: %d", rr.Code)
	}
}
