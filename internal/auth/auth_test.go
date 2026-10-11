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

func TestSessionTokenSigningAndVerification(t *testing.T) {
	secret := "my-secret-key-12345"
	userID := int64(42)

	token := CreateSessionToken(userID, secret)
	verifiedID, err := VerifySessionToken(token, secret)
	if err != nil {
		t.Fatalf("failed to verify token: %v", err)
	}
	if verifiedID != userID {
		t.Fatalf("expected user ID %d, got %d", userID, verifiedID)
	}

	// Test with wrong secret
	_, err = VerifySessionToken(token, "wrong-secret")
	if err == nil {
		t.Fatalf("expected verification to fail with wrong secret")
	}

	// Test malformed token
	_, err = VerifySessionToken("invalid:token", secret)
	if err == nil {
		t.Fatalf("expected verification to fail for malformed token")
	}
}
