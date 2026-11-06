package passkeys

import (
	"encoding/binary"

	"github.com/go-webauthn/webauthn/webauthn"
)

// waUser is the webauthn.User adapter for an account. The user handle is the
// 8-byte big-endian user id: stable across logins (a discoverable credential is
// found by credential id, then checked against this handle) and it reveals
// nothing beyond an internal sequence number.
type waUser struct {
	id          int64
	username    string
	credentials []webauthn.Credential
}

func userHandle(userID int64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(userID))
	return buf
}

func userIDFromHandle(handle []byte) (int64, bool) {
	if len(handle) != 8 {
		return 0, false
	}
	return int64(binary.BigEndian.Uint64(handle)), true
}

func (u *waUser) WebAuthnID() []byte                         { return userHandle(u.id) }
func (u *waUser) WebAuthnName() string                       { return u.username }
func (u *waUser) WebAuthnDisplayName() string                { return u.username }
func (u *waUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }
