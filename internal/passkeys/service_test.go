package passkeys

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/descope/virtualwebauthn"

	"disjoint/internal/database"
)

// testService builds a service against a throwaway SQLite database with a
// localhost Relying Party (the only origin a test server can honestly claim).
func testService(t *testing.T) (*Service, *database.Queries, database.User) {
	return testServiceWithDB(t)
}

func testServiceWithDB(t *testing.T) (*Service, *database.Queries, database.User) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "passkeys.db")
	db, queries, err := database.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	account, err := queries.CreateUser(context.Background(), database.CreateUserParams{
		Username:     "passkey_user",
		PasswordHash: "hash",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	svc, err := NewService(Config{
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		RPDisplayName: "Disjoint",
	}, queries)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	testDB = db
	return svc, queries, account
}

// testDB is the raw handle from the most recent testService call, used by
// tests that need to manipulate ceremony rows directly.
var testDB *sql.DB

func rp() virtualwebauthn.RelyingParty {
	return virtualwebauthn.RelyingParty{ID: "localhost", Name: "Disjoint", Origin: "http://localhost"}
}

func postJSON(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://localhost/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost")
	return req
}

// TestRegistrationAndLoginCeremony drives both ceremonies with a real
// authenticator implementation: attestation and assertion are signed with a
// generated key, so signature, challenge, origin and RP-ID verification all run.
func TestRegistrationAndLoginCeremony(t *testing.T) {
	svc, queries, account := testService(t)
	ctx := context.Background()

	// --- Registration -----------------------------------------------------
	options, err := svc.BeginRegistration(ctx, &account)
	if err != nil {
		t.Fatalf("begin registration: %v", err)
	}
	rawOptions, err := json.Marshal(map[string]any{"publicKey": options.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	attOptions, err := virtualwebauthn.ParseAttestationOptions(string(rawOptions))
	if err != nil {
		t.Fatalf("parse attestation options: %v", err)
	}
	// A discoverable credential carries the user handle, so the authenticator
	// must be told which handle it stores.
	authenticator := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{
		UserHandle: userHandle(account.ID),
	})
	credential := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	attestation := virtualwebauthn.CreateAttestationResponse(rp(), authenticator, credential, *attOptions)

	name, err := svc.FinishRegistration(ctx, &account, options.Token, postJSON(t, attestation))
	if err != nil {
		t.Fatalf("finish registration: %v", err)
	}
	if name == "" {
		t.Fatalf("expected a default credential name")
	}

	stored, err := queries.ListWebAuthnCredentialsByUser(ctx, account.ID)
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored credentials = %d (err=%v), want 1", len(stored), err)
	}
	if stored[0].SignCount != 0 {
		t.Errorf("fresh credential sign count = %d, want 0", stored[0].SignCount)
	}

	// A ceremony token is single use: replaying the same finish fails.
	if _, err := svc.FinishRegistration(ctx, &account, options.Token, postJSON(t, attestation)); err == nil {
		t.Fatalf("replayed registration ceremony was accepted")
	}

	// --- Discoverable login ----------------------------------------------
	loginOptions, err := svc.BeginLogin(ctx)
	if err != nil {
		t.Fatalf("begin login: %v", err)
	}
	rawLogin, err := json.Marshal(map[string]any{"publicKey": loginOptions.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	assertOptions, err := virtualwebauthn.ParseAssertionOptions(string(rawLogin))
	if err != nil {
		t.Fatalf("parse assertion options: %v", err)
	}
	assertion := virtualwebauthn.CreateAssertionResponse(rp(), authenticator, credential, *assertOptions)

	userID, credentialID, err := svc.FinishLogin(ctx, loginOptions.Token, postJSON(t, assertion))
	if err != nil {
		t.Fatalf("finish login: %v", err)
	}
	if userID != account.ID {
		t.Fatalf("login user = %d, want %d", userID, account.ID)
	}
	if credentialID == "" {
		t.Fatalf("login returned no credential id")
	}

	// Replaying the login ceremony fails (single use).
	if _, _, err := svc.FinishLogin(ctx, loginOptions.Token, postJSON(t, assertion)); err == nil {
		t.Fatalf("replayed login ceremony was accepted")
	}

	// --- Wrong origin is rejected ----------------------------------------
	badOptions, err := svc.BeginRegistration(ctx, &account)
	if err != nil {
		t.Fatalf("begin registration 2: %v", err)
	}
	rawBad, _ := json.Marshal(map[string]any{"publicKey": badOptions.PublicKey})
	badAttOptions, err := virtualwebauthn.ParseAttestationOptions(string(rawBad))
	if err != nil {
		t.Fatal(err)
	}
	badRP := virtualwebauthn.RelyingParty{ID: "localhost", Name: "Disjoint", Origin: "https://evil.example"}
	badAttestation := virtualwebauthn.CreateAttestationResponse(badRP, authenticator, virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2), *badAttOptions)
	if _, err := svc.FinishRegistration(ctx, &account, badOptions.Token, postJSON(t, badAttestation)); err == nil {
		t.Fatalf("registration from a foreign origin was accepted")
	}
}

// TestSignCountRegression checks the clone-detection policy: a stored counter
// that is higher than the authenticator's is a hard failure.
func TestSignCountRegression(t *testing.T) {
	svc, queries, account := testService(t)
	ctx := context.Background()

	options, err := svc.BeginRegistration(ctx, &account)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"publicKey": options.PublicKey})
	attOptions, err := virtualwebauthn.ParseAttestationOptions(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	auth := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{
		UserHandle: userHandle(account.ID),
	})
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	attestation := virtualwebauthn.CreateAttestationResponse(rp(), auth, cred, *attOptions)
	if _, err := svc.FinishRegistration(ctx, &account, options.Token, postJSON(t, attestation)); err != nil {
		t.Fatal(err)
	}

	// Simulate a credential that has been used with a higher counter.
	stored, err := queries.ListWebAuthnCredentialsByUser(ctx, account.ID)
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored = %d (err=%v)", len(stored), err)
	}
	if err := queries.UpdateWebAuthnCredentialUsage(ctx, database.UpdateWebAuthnCredentialUsageParams{
		SignCount:   99,
		BackupState: 0,
		ID:          stored[0].ID,
	}); err != nil {
		t.Fatal(err)
	}

	loginOptions, err := svc.BeginLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rawLogin, _ := json.Marshal(map[string]any{"publicKey": loginOptions.PublicKey})
	assertOptions, err := virtualwebauthn.ParseAssertionOptions(string(rawLogin))
	if err != nil {
		t.Fatal(err)
	}
	assertion := virtualwebauthn.CreateAssertionResponse(rp(), auth, cred, *assertOptions)
	if _, _, err := svc.FinishLogin(ctx, loginOptions.Token, postJSON(t, assertion)); err == nil {
		t.Fatalf("login with a regressed sign count was accepted")
	}
}

// Expired ceremonies must not be usable even before the prune runs.
func TestCeremonyExpiry(t *testing.T) {
	svc, _, account := testService(t)
	ctx := context.Background()

	options, err := svc.BeginRegistration(ctx, &account)
	if err != nil {
		t.Fatal(err)
	}
	// Push the stored expiry into the past.
	if _, err := testDB.ExecContext(ctx, `UPDATE webauthn_ceremonies SET expires_at = datetime('now', '-1 minute')`); err != nil {
		t.Fatal(err)
	}
	_, err = svc.FinishRegistration(ctx, &account, options.Token, postJSON(t, "{}"))
	if err == nil || !strings.Contains(err.Error(), ErrCeremony.Error()) {
		t.Fatalf("expired ceremony error = %v, want ErrCeremony", err)
	}
}

// TestPasskeySignup drives the whole passkey-only signup: invite validation,
// pending account creation, attestation, invite consumption, activation and a
// subsequent discoverable login with the same credential.
func TestPasskeySignup(t *testing.T) {
	svc, queries, admin := testService(t)
	ctx := context.Background()

	if _, err := queries.CreateInvite(ctx, database.CreateInviteParams{Code: "signup-invite", CreatedBy: admin.ID}); err != nil {
		t.Fatal(err)
	}

	// Unknown/used invites and taken usernames fail before any ceremony exists.
	if _, err := svc.BeginSignup(ctx, "not-an-invite", "newbie"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("bad invite err = %v, want ErrInviteInvalid", err)
	}
	if _, err := svc.BeginSignup(ctx, "signup-invite", "passkey_user"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("taken username err = %v, want ErrUsernameTaken", err)
	}

	options, err := svc.BeginSignup(ctx, "signup-invite", "newbie")
	if err != nil {
		t.Fatalf("begin signup: %v", err)
	}
	pending, err := queries.GetUserByUsernameFold(ctx, "newbie")
	if err != nil {
		t.Fatalf("pending user missing: %v", err)
	}
	if !pending.PendingUntil.Valid || pending.PasswordSet != 0 {
		t.Fatalf("pending user state = %+v", pending)
	}
	// The pending row holds the username for other ceremonies too.
	if _, err := svc.BeginSignup(ctx, "signup-invite", "newbie"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("pending username reuse err = %v, want ErrUsernameTaken", err)
	}

	raw, _ := json.Marshal(map[string]any{"publicKey": options.PublicKey})
	attOptions, err := virtualwebauthn.ParseAttestationOptions(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	authenticator := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{
		UserHandle: userHandle(pending.ID),
	})
	credential := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	attestation := virtualwebauthn.CreateAttestationResponse(rp(), authenticator, credential, *attOptions)

	account, err := svc.FinishSignup(ctx, options.Token, postJSON(t, attestation))
	if err != nil {
		t.Fatalf("finish signup: %v", err)
	}
	if account.ID != pending.ID || account.PendingUntil.Valid || account.PasswordSet != 0 {
		t.Fatalf("activated account = %+v", account)
	}
	invite, err := queries.GetInviteByCode(ctx, "signup-invite")
	if err == nil {
		t.Fatalf("invite still redeemable after signup: %+v", invite)
	}
	stored, err := queries.ListWebAuthnCredentialsByUser(ctx, account.ID)
	if err != nil || len(stored) != 1 {
		t.Fatalf("credentials = %d (err=%v), want 1", len(stored), err)
	}

	// The ceremony is single use and the invite cannot be redeemed again.
	if _, err := svc.FinishSignup(ctx, options.Token, postJSON(t, attestation)); err == nil {
		t.Fatalf("replayed signup ceremony was accepted")
	}

	// The new credential signs in discoverably.
	loginOptions, err := svc.BeginLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rawLogin, _ := json.Marshal(map[string]any{"publicKey": loginOptions.PublicKey})
	assertOptions, err := virtualwebauthn.ParseAssertionOptions(string(rawLogin))
	if err != nil {
		t.Fatal(err)
	}
	assertion := virtualwebauthn.CreateAssertionResponse(rp(), authenticator, credential, *assertOptions)
	userID, _, err := svc.FinishLogin(ctx, loginOptions.Token, postJSON(t, assertion))
	if err != nil || userID != account.ID {
		t.Fatalf("passkey login after signup = %d (err=%v), want %d", userID, err, account.ID)
	}
}

// A failed attestation must delete the pending row and leave the invite usable.
func TestPasskeySignupFailureRollsBack(t *testing.T) {
	svc, queries, admin := testService(t)
	ctx := context.Background()
	if _, err := queries.CreateInvite(ctx, database.CreateInviteParams{Code: "rollback-invite", CreatedBy: admin.ID}); err != nil {
		t.Fatal(err)
	}
	options, err := svc.BeginSignup(ctx, "rollback-invite", "rollback_user")
	if err != nil {
		t.Fatal(err)
	}
	// Garbage attestation: no credential, no account.
	if _, err := svc.FinishSignup(ctx, options.Token, postJSON(t, "{}")); err == nil {
		t.Fatalf("garbage attestation was accepted")
	}
	if _, err := queries.GetUserByUsernameFold(ctx, "rollback_user"); err == nil {
		t.Fatalf("pending user survived a failed signup")
	}
	if _, err := queries.GetInviteByCode(ctx, "rollback-invite"); err != nil {
		t.Fatalf("invite was consumed by a failed signup: %v", err)
	}
}

// Re-authentication is what allows a passkey-only account to set a password.
func TestReauthCeremony(t *testing.T) {
	svc, queries, account := testService(t)
	ctx := context.Background()

	// No credentials yet: reauth cannot start.
	if _, err := svc.BeginReauth(ctx, &account); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("BeginReauth without credentials = %v, want ErrNoCredentials", err)
	}

	// Register one credential through the normal flow.
	options, err := svc.BeginRegistration(ctx, &account)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"publicKey": options.PublicKey})
	attOptions, err := virtualwebauthn.ParseAttestationOptions(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	authenticator := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{
		UserHandle: userHandle(account.ID),
	})
	credential := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	attestation := virtualwebauthn.CreateAttestationResponse(rp(), authenticator, credential, *attOptions)
	if _, err := svc.FinishRegistration(ctx, &account, options.Token, postJSON(t, attestation)); err != nil {
		t.Fatal(err)
	}

	reauthOptions, err := svc.BeginReauth(ctx, &account)
	if err != nil {
		t.Fatal(err)
	}
	rawLogin, _ := json.Marshal(map[string]any{"publicKey": reauthOptions.PublicKey})
	assertOptions, err := virtualwebauthn.ParseAssertionOptions(string(rawLogin))
	if err != nil {
		t.Fatal(err)
	}
	assertion := virtualwebauthn.CreateAssertionResponse(rp(), authenticator, credential, *assertOptions)
	if err := svc.FinishReauth(ctx, &account, reauthOptions.Token, postJSON(t, assertion)); err != nil {
		t.Fatalf("finish reauth: %v", err)
	}
	// Single use.
	if err := svc.FinishReauth(ctx, &account, reauthOptions.Token, postJSON(t, assertion)); err == nil {
		t.Fatalf("replayed reauth ceremony was accepted")
	}
	_ = queries
}
