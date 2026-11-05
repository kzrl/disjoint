package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"disjoint/internal/database"
)

type mentionSuggestion struct {
	Name   string `json:"name"`
	Avatar string `json:"avatar"`
}

type mentionResponse struct {
	Users []mentionSuggestion `json:"users"`
}

func TestMentionSuggestionsEndpoint(t *testing.T) {
	app, db, queries, _ := setupTestApp(t)
	defer db.Close()
	router := app.Routes()
	ctx := context.Background()

	viewer, err := queries.CreateUser(ctx, database.CreateUserParams{
		Username:     "mentionviewer",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authCookieFor(t, queries, viewer.ID)

	// Guests cannot use the endpoint.
	req := httptest.NewRequest(http.MethodGet, "/api/mentions?q=al", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("guest /api/mentions = %d to %s", rr.Code, rr.Header().Get("Location"))
	}

	// Alphabetical tie-break, case-insensitive prefix.
	for _, name := range []string{"alice", "albert", "alex", "bob", "zoe"} {
		if _, err := queries.CreateUser(ctx, database.CreateUserParams{
			Username:     name,
			PasswordHash: "hash",
		}); err != nil {
			t.Fatal(err)
		}
	}

	get := func(q string) (*httptest.ResponseRecorder, mentionResponse) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/mentions?q="+url.QueryEscape(q), nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		var out mentionResponse
		if rr.Code == http.StatusOK {
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q", ct)
			}
			if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
				t.Fatalf("decode response: %v", err)
			}
		}
		return rr, out
	}

	rr, out := get("al")
	if rr.Code != http.StatusOK {
		t.Fatalf("q=al = %d: %s", rr.Code, rr.Body.String())
	}
	var names []string
	for _, u := range out.Users {
		names = append(names, u.Name)
		if !strings.HasPrefix(u.Avatar, "/avatars/"+u.Name+"?v=") {
			t.Errorf("avatar for %q = %q", u.Name, u.Avatar)
		}
	}
	want := []string{"albert", "alex", "alice"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("q=al names = %v, want %v", names, want)
	}

	// Case-insensitive and prefix-only.
	if _, out = get("AL"); len(out.Users) != 3 || out.Users[0].Name != "albert" {
		t.Fatalf("q=AL users = %+v", out.Users)
	}
	rr, out = get("zzz")
	if rr.Code != http.StatusOK || out.Users == nil || len(out.Users) != 0 {
		t.Fatalf("q=zzz users = %+v, want an empty array", out.Users)
	}

	// A bare @ returns the top-10 list (at most).
	for i := 0; i < 12; i++ {
		if _, err := queries.CreateUser(ctx, database.CreateUserParams{
			Username:     fmt.Sprintf("bulk%02d", i),
			PasswordHash: "hash",
		}); err != nil {
			t.Fatal(err)
		}
	}
	rr, out = get("")
	if rr.Code != http.StatusOK || len(out.Users) != 10 {
		t.Fatalf("q= empty users = %d, want 10", len(out.Users))
	}

	// Presence orders the recently active candidate first.
	alice, err := queries.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.UpsertUserPresence(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	_, out = get("al")
	if len(out.Users) != 3 || out.Users[0].Name != "alice" {
		t.Fatalf("presence order = %+v, want alice first", out.Users)
	}

	// LIKE metacharacters are escaped, not treated as wildcards.
	rr, out = get("%")
	if rr.Code != http.StatusOK || len(out.Users) != 0 {
		t.Fatalf("q=%% users = %+v, want none", out.Users)
	}
	rr, out = get("_")
	if rr.Code != http.StatusOK || len(out.Users) != 0 {
		t.Fatalf("q=_ users = %+v, want none", out.Users)
	}
	rr, out = get(`\`)
	if rr.Code != http.StatusOK || len(out.Users) != 0 {
		t.Fatalf("q=backslash users = %+v, want none", out.Users)
	}

	// Too-long prefixes are a 400 before any query runs.
	rr, _ = get(strings.Repeat("a", 33))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("33-char q = %d, want 400", rr.Code)
	}
}
