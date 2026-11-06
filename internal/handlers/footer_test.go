package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"disjoint/internal/database"
)

// The footer always links to the public repository (GPL-3.0 source offer),
// independently of whether the build stamped a commit hash.
func TestFooterLinksToRepository(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	user, err := queries.CreateUser(ctx, database.CreateUserParams{Username: "footer_user", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, user.ID)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, `href="https://github.com/kzrl/disjoint"`) {
		t.Fatalf("footer is missing the repository link")
	}
	if !strings.Contains(body, "Source on GitHub") {
		t.Fatalf("footer repo link has no label")
	}
}
