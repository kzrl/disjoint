package auth

import (
	"testing"
)

func TestHashAndCheckPassword(t *testing.T) {
	password := "supersecret123"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("unexpected error hashing password: %v", err)
	}

	if !CheckPassword(hash, password) {
		t.Fatalf("expected password check to succeed")
	}

	if CheckPassword(hash, "wrongpassword") {
		t.Fatalf("expected password check to fail for incorrect password")
	}
}

func TestSessionTokenGenerationAndHashing(t *testing.T) {
	token, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	if len(token) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(token))
	}
	if token == "" {
		t.Fatalf("expected non-empty token")
	}

	// Tokens must be unique
	token2, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("failed to generate second token: %v", err)
	}
	if token == token2 {
		t.Fatalf("expected unique tokens, got %s twice", token)
	}

	// TokenHash is deterministic and not the token itself
	if h1, h2 := TokenHash(token), TokenHash(token); h1 != h2 {
		t.Fatalf("expected deterministic hash")
	}
	if TokenHash(token) == token {
		t.Fatalf("hash must differ from the raw token")
	}
	if TokenHash(token) == TokenHash(token2) {
		t.Fatalf("different tokens must hash differently")
	}
}
