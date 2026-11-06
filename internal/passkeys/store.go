package passkeys

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"disjoint/internal/database"
)

// CeremonyKind separates the two ceremonies that share the state table.
type CeremonyKind string

const (
	CeremonyRegister CeremonyKind = "register"
	CeremonyLogin    CeremonyKind = "login"
	// ceremonyTTL is how long a challenge stays usable. Short on purpose: the
	// user is expected to touch the authenticator immediately.
	ceremonyTTL = 5 * time.Minute
	// maxCredentialsPerUser bounds rows and confusion per account.
	MaxCredentialsPerUser = 10
	// maxCredentialNameLength bounds the user-facing label.
	maxCredentialNameLength = 40
)

var (
	// ErrCeremony is returned when the ceremony token is unknown, expired,
	// already used, or of the wrong kind.
	ErrCeremony = errors.New("passkey ceremony expired or invalid")
	// ErrCredentialLimit is returned when an account already has the maximum
	// number of registered passkeys.
	ErrCredentialLimit = errors.New("passkey limit reached")
	// ErrCredentialNotFound is returned when no stored credential matches.
	ErrCredentialNotFound = errors.New("passkey not found")
	// ErrSignCountRegression indicates a possible cloned authenticator.
	ErrSignCountRegression = errors.New("passkey signature counter regressed")
)

// Store owns persistence for passkeys: credentials plus the short-lived
// ceremony rows. It is deliberately dumb so the protocol logic stays in
// Service.
type Store struct {
	q   *database.Queries
	now func() time.Time
}

// NewStore wraps the query set. now is injectable for expiry tests.
func NewStore(q *database.Queries, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{q: q, now: now}
}

// CreateCeremony stores sessionData under a fresh random id and returns it.
func (s *Store) CreateCeremony(ctx context.Context, kind CeremonyKind, userID sql.NullInt64, sessionData *webauthn.SessionData) (string, error) {
	id, err := randomToken()
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(sessionData)
	if err != nil {
		return "", fmt.Errorf("encode ceremony: %w", err)
	}
	if err := s.q.CreateWebAuthnCeremony(ctx, database.CreateWebAuthnCeremonyParams{
		ID:          id,
		UserID:      userID,
		Kind:        string(kind),
		SessionData: payload,
		ExpiresAt:   s.now().UTC().Add(ceremonyTTL).Round(0),
	}); err != nil {
		return "", fmt.Errorf("store ceremony: %w", err)
	}
	return id, nil
}

// TakeCeremony consumes a ceremony exactly once and decodes its session data.
func (s *Store) TakeCeremony(ctx context.Context, kind CeremonyKind, id string) (*webauthn.SessionData, sql.NullInt64, error) {
	if id == "" {
		return nil, sql.NullInt64{}, ErrCeremony
	}
	row, err := s.q.TakeWebAuthnCeremony(ctx, database.TakeWebAuthnCeremonyParams{
		ID:   id,
		Kind: string(kind),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.NullInt64{}, ErrCeremony
	}
	if err != nil {
		return nil, sql.NullInt64{}, fmt.Errorf("take ceremony: %w", err)
	}
	var session webauthn.SessionData
	if err := json.Unmarshal(row.SessionData, &session); err != nil {
		return nil, sql.NullInt64{}, fmt.Errorf("decode ceremony: %w", err)
	}
	return &session, row.UserID, nil
}

// PruneCeremonies drops expired rows; best-effort, called on every begin.
func (s *Store) PruneCeremonies(ctx context.Context) {
	_ = s.q.DeleteExpiredWebAuthnCeremonies(ctx)
}

// CredentialsForUser loads the stored credentials for the webauthn.User
// adapter.
func (s *Store) CredentialsForUser(ctx context.Context, userID int64) ([]webauthn.Credential, error) {
	rows, err := s.q.ListWebAuthnCredentialsByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list passkeys: %w", err)
	}
	out := make([]webauthn.Credential, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToCredential(row))
	}
	return out, nil
}

// CredentialCount reports how many passkeys an account has.
func (s *Store) CredentialCount(ctx context.Context, userID int64) (int64, error) {
	n, err := s.q.CountWebAuthnCredentialsByUser(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("count passkeys: %w", err)
	}
	return n, nil
}

// SaveCredential persists a freshly registered credential.
func (s *Store) SaveCredential(ctx context.Context, userID int64, cred *webauthn.Credential, name string) error {
	return s.q.CreateWebAuthnCredential(ctx, database.CreateWebAuthnCredentialParams{
		ID:             encodeID(cred.ID),
		UserID:         userID,
		PublicKey:      cred.PublicKey,
		SignCount:      int64(cred.Authenticator.SignCount),
		Transports:     joinTransports(cred.Transport),
		Aaguid:         hex.EncodeToString(cred.Authenticator.AAGUID),
		BackupEligible: boolToInt(cred.Flags.BackupEligible),
		BackupState:    boolToInt(cred.Flags.BackupState),
		Name:           truncateName(name),
	})
}

// LookupCredential finds a credential by its raw id (used by the discoverable
// login handler).
func (s *Store) LookupCredential(ctx context.Context, rawID []byte) (database.WebauthnCredential, error) {
	row, err := s.q.GetWebAuthnCredential(ctx, encodeID(rawID))
	if errors.Is(err, sql.ErrNoRows) {
		return database.WebauthnCredential{}, ErrCredentialNotFound
	}
	if err != nil {
		return database.WebauthnCredential{}, fmt.Errorf("lookup passkey: %w", err)
	}
	return row, nil
}

// TouchCredential writes back the counter and backup state after a successful
// assertion. A regressed counter is reported to the caller as a clone warning
// rather than being silently accepted.
func (s *Store) TouchCredential(ctx context.Context, cred *webauthn.Credential, previousSignCount int64) error {
	if cred.Authenticator.SignCount < uint32(previousSignCount) {
		return ErrSignCountRegression
	}
	return s.q.UpdateWebAuthnCredentialUsage(ctx, database.UpdateWebAuthnCredentialUsageParams{
		SignCount:   int64(cred.Authenticator.SignCount),
		BackupState: boolToInt(cred.Flags.BackupState),
		ID:          encodeID(cred.ID),
	})
}

// Delete removes one of the user's credentials; false means it was not theirs
// (or did not exist).
func (s *Store) Delete(ctx context.Context, userID int64, id string) (bool, error) {
	rows, err := s.q.DeleteWebAuthnCredential(ctx, database.DeleteWebAuthnCredentialParams{
		ID:     id,
		UserID: userID,
	})
	if err != nil {
		return false, fmt.Errorf("delete passkey: %w", err)
	}
	return rows > 0, nil
}

// Rename relabels a credential owned by the user.
func (s *Store) Rename(ctx context.Context, userID int64, id, name string) (bool, error) {
	rows, err := s.q.UpdateWebAuthnCredentialName(ctx, database.UpdateWebAuthnCredentialNameParams{
		Name:   truncateName(name),
		ID:     id,
		UserID: userID,
	})
	if err != nil {
		return false, fmt.Errorf("rename passkey: %w", err)
	}
	return rows > 0, nil
}

// List returns the user's credentials, newest metadata for the account page.
func (s *Store) List(ctx context.Context, userID int64) ([]database.WebauthnCredential, error) {
	rows, err := s.q.ListWebAuthnCredentialsByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list passkeys: %w", err)
	}
	return rows, nil
}

func rowToCredential(row database.WebauthnCredential) webauthn.Credential {
	return webauthn.Credential{
		ID:        mustDecodeID(row.ID),
		PublicKey: row.PublicKey,
		Transport: parseTransports(row.Transports),
		Flags: webauthn.CredentialFlags{
			BackupEligible: row.BackupEligible != 0,
			BackupState:    row.BackupState != 0,
		},
		Authenticator: webauthn.Authenticator{
			AAGUID:    mustDecodeHex(row.Aaguid),
			SignCount: uint32(row.SignCount),
		},
	}
}

func encodeID(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

func mustDecodeID(encoded string) []byte {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil
	}
	return raw
}

func mustDecodeHex(encoded string) []byte {
	raw, err := hex.DecodeString(encoded)
	if err != nil {
		return nil
	}
	return raw
}

func joinTransports(transports []protocol.AuthenticatorTransport) string {
	parts := make([]string, 0, len(transports))
	for _, t := range transports {
		parts = append(parts, string(t))
	}
	return strings.Join(parts, ",")
}

func parseTransports(raw string) []protocol.AuthenticatorTransport {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]protocol.AuthenticatorTransport, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, protocol.AuthenticatorTransport(p))
		}
	}
	return out
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func truncateName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Passkey"
	}
	runes := []rune(name)
	if len(runes) > maxCredentialNameLength {
		name = strings.TrimSpace(string(runes[:maxCredentialNameLength]))
	}
	return name
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
