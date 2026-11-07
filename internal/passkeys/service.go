package passkeys

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"disjoint/internal/auth"
	"disjoint/internal/database"
)

// authHash keeps the bcrypt dependency in one place (the stored hash for a
// passkey-only account is a digest of random bytes that nothing knows).
func authHash(secret string) (string, error) {
	return auth.HashPassword(secret)
}

// Service performs the WebAuthn ceremonies and persists their results. Handlers
// depend on the Service interface below so the HTTP layer can be tested without
// a real authenticator.
type Service struct {
	wa    *webauthn.WebAuthn
	store *Store
	q     *database.Queries
}

// Options are the JSON-serializable creation/assertion options handed to the
// browser, plus the opaque ceremony token the browser sends back on finish.
type Options struct {
	PublicKey any    `json:"publicKey"`
	Token     string `json:"token"`
}

// NewService builds the Relying Party and its store.
func NewService(cfg Config, q *database.Queries) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          cfg.RPID,
		RPDisplayName: cfg.RPDisplayName,
		RPOrigins:     cfg.RPOrigins,
		// Attestation stays "none": there is no trust policy or admin approval
		// flow, and "none" keeps registration fast on every platform.
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			// Discoverable credentials make username-less sign-in possible;
			// "preferred" degrades gracefully on old security keys.
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationPreferred,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: 60 * time.Second, TimeoutUVD: 120 * time.Second},
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: 60 * time.Second, TimeoutUVD: 120 * time.Second},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("webauthn setup: %w", err)
	}
	return &Service{wa: wa, store: NewStore(q, nil), q: q}, nil
}

// Store exposes the persistence layer for the account page and tests.
func (s *Service) Store() *Store { return s.store }

// BeginRegistration returns creation options for an authenticated user and
// stores the ceremony.
func (s *Service) BeginRegistration(ctx context.Context, account *database.User) (Options, error) {
	u, err := s.loadUser(ctx, account)
	if err != nil {
		return Options{}, err
	}
	count, err := s.store.CredentialCount(ctx, account.ID)
	if err != nil {
		return Options{}, err
	}
	if count >= MaxCredentialsPerUser {
		return Options{}, ErrCredentialLimit
	}

	s.store.PruneCeremonies(ctx)
	creation, session, err := s.wa.BeginRegistration(u)
	if err != nil {
		return Options{}, fmt.Errorf("begin registration: %w", err)
	}
	if session.UserID == nil {
		session.UserID = u.WebAuthnID()
	}
	token, err := s.store.CreateCeremony(ctx, CeremonyRegister, sql.NullInt64{Int64: account.ID, Valid: true}, session, "")
	if err != nil {
		return Options{}, err
	}
	return Options{PublicKey: creation.Response, Token: token}, nil
}

// FinishRegistration verifies the attestation and stores the credential. It
// returns the default label assigned to the new passkey.
func (s *Service) FinishRegistration(ctx context.Context, account *database.User, token string, r *http.Request) (string, error) {
	session, userID, _, err := s.store.TakeCeremony(ctx, CeremonyRegister, token)
	if err != nil {
		return "", err
	}
	if !userID.Valid || userID.Int64 != account.ID {
		// A ceremony belongs to the account that started it; never let another
		// session finish it.
		return "", ErrCeremony
	}
	count, err := s.store.CredentialCount(ctx, account.ID)
	if err != nil {
		return "", err
	}
	if count >= MaxCredentialsPerUser {
		return "", ErrCredentialLimit
	}

	u, err := s.loadUser(ctx, account)
	if err != nil {
		return "", err
	}
	cred, err := s.wa.FinishRegistration(u, *session, r)
	if err != nil {
		return "", fmt.Errorf("verify registration: %w", err)
	}

	name := fmt.Sprintf("Passkey %d", count+1)
	if err := s.store.SaveCredential(ctx, account.ID, cred, name); err != nil {
		return "", err
	}
	return name, nil
}

// BeginLogin returns assertion options for a discoverable (username-less)
// sign-in and stores the ceremony. No user is known at this point.
func (s *Service) BeginLogin(ctx context.Context) (Options, error) {
	s.store.PruneCeremonies(ctx)
	assertion, session, err := s.wa.BeginDiscoverableLogin()
	if err != nil {
		return Options{}, fmt.Errorf("begin passkey login: %w", err)
	}
	token, err := s.store.CreateCeremony(ctx, CeremonyLogin, sql.NullInt64{}, session, "")
	if err != nil {
		return Options{}, err
	}
	return Options{PublicKey: assertion.Response, Token: token}, nil
}

// FinishLogin verifies an assertion and returns the account it belongs to.
// credentialID is the stored row id (base64url) for auditing.
func (s *Service) FinishLogin(ctx context.Context, token string, r *http.Request) (int64, string, error) {
	session, _, _, err := s.store.TakeCeremony(ctx, CeremonyLogin, token)
	if err != nil {
		return 0, "", err
	}

	handler := func(rawID, handle []byte) (webauthn.User, error) {
		row, err := s.store.LookupCredential(ctx, rawID)
		if err != nil {
			return nil, err
		}
		// The user handle is our own stable id; a credential claiming a
		// different account must not be accepted.
		if id, ok := userIDFromHandle(handle); ok && id != row.UserID {
			return nil, ErrCredentialNotFound
		}
		account, err := s.q.GetUserByID(ctx, row.UserID)
		if err != nil {
			return nil, ErrCredentialNotFound
		}
		return s.loadUser(ctx, &account)
	}

	waUserOut, cred, err := s.wa.FinishPasskeyLogin(handler, *session, r)
	if err != nil {
		return 0, "", fmt.Errorf("verify passkey login: %w", err)
	}
	u, ok := waUserOut.(*waUser)
	if !ok {
		return 0, "", errors.New("unexpected user type from webauthn")
	}

	stored, err := s.store.LookupCredential(ctx, cred.ID)
	if err != nil {
		return 0, "", err
	}
	if cred.Authenticator.CloneWarning {
		return 0, "", ErrSignCountRegression
	}
	if err := s.store.TouchCredential(ctx, cred, stored.SignCount); err != nil {
		return 0, "", err
	}
	return u.id, stored.ID, nil
}

// ListCredentials returns the account page's rows.
func (s *Service) ListCredentials(ctx context.Context, userID int64) ([]database.WebauthnCredential, error) {
	return s.store.List(ctx, userID)
}

// DeleteCredential removes one of the user's passkeys.
func (s *Service) DeleteCredential(ctx context.Context, userID int64, id string) (bool, error) {
	return s.store.Delete(ctx, userID, id)
}

// RenameCredential relabels one of the user's passkeys.
func (s *Service) RenameCredential(ctx context.Context, userID int64, id, name string) (bool, error) {
	return s.store.Rename(ctx, userID, id, name)
}

// BeginSignup starts a passkey-only account creation: it validates the invite
// and username, creates an invisible pending user (WebAuthn needs a handle up
// front), and returns creation options. The invite is not consumed yet.
func (s *Service) BeginSignup(ctx context.Context, code, username string) (Options, error) {
	if _, err := s.q.GetInviteByCode(ctx, code); err != nil {
		return Options{}, ErrInviteInvalid
	}
	// GetUserByUsernameFold matches pending rows too, so two ceremonies cannot
	// claim the same name.
	if _, err := s.q.GetUserByUsernameFold(ctx, username); err == nil {
		return Options{}, ErrUsernameTaken
	}

	// The stored hash is a bcrypt digest of random bytes that nothing knows;
	// password_set=0 is what tells the login path to refuse, and keeping the
	// column non-NULL avoids a users-table rebuild.
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return Options{}, err
	}
	unusableHash, err := authHash(hex.EncodeToString(secret))
	if err != nil {
		return Options{}, err
	}

	pending, err := s.q.CreatePendingUser(ctx, database.CreatePendingUserParams{
		Username:     username,
		PasswordHash: unusableHash,
		PendingUntil: sql.NullTime{Time: time.Now().UTC().Add(pendingAccountTTL).Round(0), Valid: true},
	})
	if err != nil {
		// A UNIQUE constraint violation here means the name was taken between
		// the check and the insert.
		return Options{}, ErrUsernameTaken
	}

	cleanup := func() { _, _ = s.q.DeletePendingUser(ctx, pending.ID) }

	u := &waUser{id: pending.ID, username: pending.Username}
	s.store.PruneCeremonies(ctx)
	creation, session, err := s.wa.BeginRegistration(u)
	if err != nil {
		cleanup()
		return Options{}, fmt.Errorf("begin signup: %w", err)
	}
	if session.UserID == nil {
		session.UserID = u.WebAuthnID()
	}
	ceremonyContext, _ := json.Marshal(map[string]string{"invite": code})
	token, err := s.store.CreateCeremony(ctx, CeremonySignup, sql.NullInt64{Int64: pending.ID, Valid: true}, session, string(ceremonyContext))
	if err != nil {
		cleanup()
		return Options{}, err
	}
	return Options{PublicKey: creation.Response, Token: token}, nil
}

// FinishSignup verifies the attestation, then activates the pending account,
// consumes the invite and stores the credential in one transaction. Any failure
// deletes the pending row, so a half-finished signup never squats a username or
// burns an invite.
func (s *Service) FinishSignup(ctx context.Context, token string, r *http.Request) (database.User, error) {
	session, userID, ceremonyContext, err := s.store.TakeCeremony(ctx, CeremonySignup, token)
	if err != nil {
		return database.User{}, err
	}
	if !userID.Valid {
		return database.User{}, ErrCeremony
	}
	pending, err := s.q.GetUserByID(ctx, userID.Int64)
	if err != nil || !pending.PendingUntil.Valid {
		return database.User{}, ErrPendingAccount
	}
	var ctxData struct {
		Invite string `json:"invite"`
	}
	if err := json.Unmarshal([]byte(ceremonyContext), &ctxData); err != nil || ctxData.Invite == "" {
		_, _ = s.q.DeletePendingUser(ctx, pending.ID)
		return database.User{}, ErrCeremony
	}

	u, err := s.loadUser(ctx, &pending)
	if err != nil {
		_, _ = s.q.DeletePendingUser(ctx, pending.ID)
		return database.User{}, err
	}
	cred, err := s.wa.FinishRegistration(u, *session, r)
	if err != nil {
		_, _ = s.q.DeletePendingUser(ctx, pending.ID)
		return database.User{}, fmt.Errorf("verify signup: %w", err)
	}

	tx, err := s.q.DB().BeginTx(ctx, nil)
	if err != nil {
		_, _ = s.q.DeletePendingUser(ctx, pending.ID)
		return database.User{}, err
	}
	qtx := s.q.WithTx(tx)
	txStore := NewStore(qtx, s.store.now)

	rows, err := qtx.ConsumeInvite(ctx, database.ConsumeInviteParams{
		UsedBy: sql.NullInt64{Int64: pending.ID, Valid: true},
		Code:   ctxData.Invite,
	})
	if err == nil {
		var activated int64
		if rows > 0 {
			activated, err = qtx.ActivatePendingUser(ctx, pending.ID)
		}
		if err == nil && rows > 0 && activated == 0 {
			err = ErrPendingAccount
		}
	}
	if err == nil {
		err = txStore.SaveCredential(ctx, pending.ID, cred, "Passkey 1")
	}
	if err != nil {
		_ = tx.Rollback()
		_, _ = s.q.DeletePendingUser(ctx, pending.ID)
		return database.User{}, err
	}
	if err := tx.Commit(); err != nil {
		_, _ = s.q.DeletePendingUser(ctx, pending.ID)
		return database.User{}, err
	}

	account, err := s.q.GetUserByID(ctx, pending.ID)
	if err != nil {
		return database.User{}, err
	}
	return account, nil
}

// BeginReauth returns assertion options restricted to the signed-in account's
// own passkeys. It is the proof required before a passkey-only account may set
// a password.
func (s *Service) BeginReauth(ctx context.Context, account *database.User) (Options, error) {
	u, err := s.loadUser(ctx, account)
	if err != nil {
		return Options{}, err
	}
	if len(u.credentials) == 0 {
		return Options{}, ErrNoCredentials
	}
	s.store.PruneCeremonies(ctx)
	assertion, session, err := s.wa.BeginLogin(u)
	if err != nil {
		return Options{}, fmt.Errorf("begin reauth: %w", err)
	}
	token, err := s.store.CreateCeremony(ctx, CeremonyReauth, sql.NullInt64{Int64: account.ID, Valid: true}, session, "")
	if err != nil {
		return Options{}, err
	}
	return Options{PublicKey: assertion.Response, Token: token}, nil
}

// FinishReauth verifies an assertion for the signed-in account. The caller only
// then applies the password change (never before).
func (s *Service) FinishReauth(ctx context.Context, account *database.User, token string, r *http.Request) error {
	session, userID, _, err := s.store.TakeCeremony(ctx, CeremonyReauth, token)
	if err != nil {
		return err
	}
	if !userID.Valid || userID.Int64 != account.ID {
		return ErrCeremony
	}
	u, err := s.loadUser(ctx, account)
	if err != nil {
		return err
	}
	cred, err := s.wa.FinishLogin(u, *session, r)
	if err != nil {
		return fmt.Errorf("verify reauth: %w", err)
	}
	stored, err := s.store.LookupCredential(ctx, cred.ID)
	if err != nil {
		return err
	}
	if cred.Authenticator.CloneWarning {
		return ErrSignCountRegression
	}
	return s.store.TouchCredential(ctx, cred, stored.SignCount)
}

func (s *Service) loadUser(ctx context.Context, account *database.User) (*waUser, error) {
	creds, err := s.store.CredentialsForUser(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	return &waUser{id: account.ID, username: account.Username, credentials: creds}, nil
}
