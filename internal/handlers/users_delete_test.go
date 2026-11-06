package handlers_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"disjoint/internal/database"
)

// writeUpload creates a file in the upload dir (plus a cached thumbnail next to
// it) and returns its storage path, mirroring what an upload handler leaves
// behind on disk.
func writeUpload(t *testing.T, uploadDir, name string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(uploadDir, "thumbs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploadDir, name), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	thumb := filepath.Join(uploadDir, "thumbs", strings.TrimSuffix(name, filepath.Ext(name))+".jpg")
	if err := os.WriteFile(thumb, []byte("thumb"), 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestDeleteUserRemovesPostsAndUploads(t *testing.T) {
	app, db, queries, uploadDir := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	admin, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "del_admin", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.SetUserAdmin(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	adminCookie := authCookieFor(t, queries, admin.ID)

	victim, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "del_victim", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	bystander, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "del_bystander", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}

	// The victim's thread (with an attachment) and a reply of theirs in the
	// bystander's thread (with an attachment of its own). The bystander keeps
	// one attachment that must survive.
	victimThread, err := queries.CreateThread(ctx, database.CreateThreadParams{UserID: victim.ID, Title: "Victim thread"})
	if err != nil {
		t.Fatal(err)
	}
	victimPost, err := queries.CreatePost(ctx, database.CreatePostParams{ThreadID: victimThread.ID, UserID: victim.ID, Body: "opening"})
	if err != nil {
		t.Fatal(err)
	}
	// A reply by the bystander inside the victim's thread also disappears with
	// the thread, so its file must be cleaned up too.
	bystanderPostInVictimThread, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: victimThread.ID, UserID: bystander.ID, Body: "reply in victim thread",
	})
	if err != nil {
		t.Fatal(err)
	}

	bystanderThread, err := queries.CreateThread(ctx, database.CreateThreadParams{UserID: bystander.ID, Title: "Bystander thread"})
	if err != nil {
		t.Fatal(err)
	}
	bystanderPost, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: bystanderThread.ID, UserID: bystander.ID, Body: "kept opening",
	})
	if err != nil {
		t.Fatal(err)
	}
	victimReply, err := queries.CreatePost(ctx, database.CreatePostParams{
		ThreadID: bystanderThread.ID, UserID: victim.ID, Body: "victim reply",
	})
	if err != nil {
		t.Fatal(err)
	}

	victimFiles := []string{
		writeUpload(t, uploadDir, "victim-own.png"),
		writeUpload(t, uploadDir, "victim-thread-reply.png"),
		writeUpload(t, uploadDir, "victim-emoji.png"),
	}
	avatarFile := writeUpload(t, uploadDir, "victim-avatar.png")
	keptFile := writeUpload(t, uploadDir, "bystander-keep.png")
	reservedFile := writeUpload(t, uploadDir, "bystander-in-victim-thread.png")

	attach := func(postID int64, storage string) {
		t.Helper()
		if _, err := queries.CreateAttachment(ctx, database.CreateAttachmentParams{
			PostID:      postID,
			Filename:    storage,
			StoragePath: storage,
			MimeType:    "image/png",
			FileSize:    7,
		}); err != nil {
			t.Fatal(err)
		}
	}
	attach(victimPost.ID, victimFiles[0])
	attach(bystanderPostInVictimThread.ID, reservedFile)
	attach(victimReply.ID, victimFiles[1])
	attach(bystanderPost.ID, keptFile)

	if _, err := queries.CreateEmoji(ctx, database.CreateEmojiParams{
		Name:        "victim_emoji",
		Source:      "custom",
		StoragePath: sql.NullString{String: victimFiles[2], Valid: true},
		MimeType:    sql.NullString{String: "image/png", Valid: true},
		FileSize:    sql.NullInt64{Int64: 7, Valid: true},
		CreatedBy:   sql.NullInt64{Int64: victim.ID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.SetUserAvatar(ctx, database.SetUserAvatarParams{
		AvatarPath: sql.NullString{String: avatarFile, Valid: true},
		ID:         victim.ID,
	}); err != nil {
		t.Fatal(err)
	}

	// 1. A wrong confirmation phrase deletes nothing.
	rr := postForm(router, fmt.Sprintf("/admin/users/%d/delete", victim.ID), "confirm=nope", adminCookie)
	if !flashCookieSet(rr, "flash_error", "Type DELETE USER to confirm deleting an account") {
		t.Fatalf("expected confirmation flash for a wrong phrase")
	}
	if _, err := queries.GetUserByID(ctx, victim.ID); err != nil {
		t.Fatalf("victim must survive a rejected confirmation: %v", err)
	}

	// 2. The real phrase deletes the account and everything attached to it.
	rr = postForm(router, fmt.Sprintf("/admin/users/%d/delete", victim.ID),
		url.Values{"confirm": {"DELETE USER"}}.Encode(), adminCookie)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin/users" {
		t.Fatalf("delete = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if !flashCookieSet(rr, "flash_success", "User deleted") {
		t.Fatalf("expected delete success flash")
	}

	if _, err := queries.GetUserByID(ctx, victim.ID); err == nil {
		t.Fatalf("victim row still exists")
	}
	for _, label := range []struct {
		name string
		id   int64
	}{{"victim thread", victimThread.ID}} {
		if _, err := queries.GetThreadByID(ctx, label.id); err == nil {
			t.Errorf("%s still exists", label.name)
		}
	}
	if _, err := queries.GetPostByID(ctx, victimReply.ID); err == nil {
		t.Errorf("victim reply still exists")
	}
	if _, err := queries.GetThreadByID(ctx, bystanderThread.ID); err != nil {
		t.Errorf("bystander thread must survive: %v", err)
	}
	if _, err := queries.GetPostByID(ctx, bystanderPost.ID); err != nil {
		t.Errorf("bystander post must survive: %v", err)
	}
	if _, err := queries.GetEmojiByName(ctx, "victim_emoji"); err == nil {
		t.Errorf("custom emoji row survived the account deletion")
	}
	// 3. Uploads are gone from disk; the bystander's kept file is untouched.
	for _, name := range victimFiles {
		if _, err := os.Stat(filepath.Join(uploadDir, name)); !os.IsNotExist(err) {
			t.Errorf("victim upload %s still on disk (err=%v)", name, err)
		}
		thumb := filepath.Join(uploadDir, "thumbs", strings.TrimSuffix(name, filepath.Ext(name))+".jpg")
		if _, err := os.Stat(thumb); !os.IsNotExist(err) {
			t.Errorf("cached thumbnail for %s still on disk (err=%v)", name, err)
		}
	}
	for _, name := range []string{reservedFile, avatarFile} {
		if _, err := os.Stat(filepath.Join(uploadDir, name)); !os.IsNotExist(err) {
			t.Errorf("cascade-removed upload %s still on disk (err=%v)", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(uploadDir, keptFile)); err != nil {
		t.Errorf("bystander upload was deleted: %v", err)
	}

	// The admin list offers the delete control with its typed confirmation.
	req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	page := rec.Body.String()
	if !strings.Contains(page, `pattern="DELETE USER"`) {
		t.Errorf("admin list is missing the typed delete confirmation")
	}
	if !strings.Contains(page, fmt.Sprintf(`/admin/users/%d/delete`, bystander.ID)) {
		t.Errorf("admin list is missing the delete action for a member")
	}

	// 4. The pages that used the deleted content still render for the admin.
	for _, path := range []string{"/admin/users", "/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(adminCookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s after delete = %d", path, rec.Code)
		}
	}
}

func TestDeleteUserGuards(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	admin, member := newAdminAndMember(t, queries)
	adminCookie := authCookieFor(t, queries, admin.ID)
	memberCookie := authCookieFor(t, queries, member.ID)
	deleteForm := url.Values{"confirm": {"DELETE USER"}}.Encode()

	// Self-delete refused.
	rr := postForm(router, fmt.Sprintf("/admin/users/%d/delete", admin.ID), deleteForm, adminCookie)
	if !flashCookieSet(rr, "flash_error", "You cannot delete your own account") {
		t.Errorf("expected self-delete flash_error")
	}
	if _, err := queries.GetUserByID(ctx, admin.ID); err != nil {
		t.Errorf("admin must survive self-delete: %v", err)
	}

	// Deleting another admin is refused (revoke admin first).
	otherAdmin, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "del_other_admin", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.SetUserAdmin(ctx, otherAdmin.ID); err != nil {
		t.Fatal(err)
	}
	rr = postForm(router, fmt.Sprintf("/admin/users/%d/delete", otherAdmin.ID), deleteForm, adminCookie)
	if !flashCookieSet(rr, "flash_error", "Admins cannot be deleted. Revoke admin first") {
		t.Errorf("expected admin-target flash_error")
	}
	if _, err := queries.GetUserByID(ctx, otherAdmin.ID); err != nil {
		t.Errorf("other admin must survive: %v", err)
	}

	// Non-admins cannot delete anyone.
	rr = postForm(router, fmt.Sprintf("/admin/users/%d/delete", admin.ID), deleteForm, memberCookie)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Errorf("non-admin delete = %d to %s", rr.Code, rr.Header().Get("Location"))
	}
	if _, err := queries.GetUserByID(ctx, admin.ID); err != nil {
		t.Errorf("target must survive a non-admin attempt: %v", err)
	}
}
