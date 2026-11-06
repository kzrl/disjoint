package passkeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"disjoint/internal/database"
)

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
	token, err := s.store.CreateCeremony(ctx, CeremonyRegister, sql.NullInt64{Int64: account.ID, Valid: true}, session)
	if err != nil {
		return Options{}, err
	}
	return Options{PublicKey: creation.Response, Token: token}, nil
}

// FinishRegistration verifies the attestation and stores the credential. It
// returns the default label assigned to the new passkey.
func (s *Service) FinishRegistration(ctx context.Context, account *database.User, token string, r *http.Request) (string, error) {
	session, userID, err := s.store.TakeCeremony(ctx, CeremonyRegister, token)
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
	token, err := s.store.CreateCeremony(ctx, CeremonyLogin, sql.NullInt64{}, session)
	if err != nil {
		return Options{}, err
	}
	return Options{PublicKey: assertion.Response, Token: token}, nil
}

// FinishLogin verifies an assertion and returns the account it belongs to.
// credentialID is the stored row id (base64url) for auditing.
func (s *Service) FinishLogin(ctx context.Context, token string, r *http.Request) (int64, string, error) {
	session, _, err := s.store.TakeCeremony(ctx, CeremonyLogin, token)
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

func (s *Service) loadUser(ctx context.Context, account *database.User) (*waUser, error) {
	creds, err := s.store.CredentialsForUser(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	return &waUser{id: account.ID, username: account.Username, credentials: creds}, nil
}
