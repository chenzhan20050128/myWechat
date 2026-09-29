// Package ids generates identifiers and tokens. SPEC-00 §2.8.
package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
)

// New returns a UUIDv4 string (event IDs, upload session IDs, device IDs).
func New() string { return uuid.NewString() }

// NewToken returns n crypto-random bytes, base64url-encoded (auth tokens).
func NewToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("ids: rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// MustToken is NewToken or panic — only for paths where failure is fatal.
func MustToken(n int) string {
	t, err := NewToken(n)
	if err != nil {
		panic(err)
	}
	return t
}

// SHA256Hex hashes a token for at-rest storage (DB stores hashes only, ADR-003).
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// inviteAlphabet is Crockford Base32 (ambiguous I/L/O/U excluded) for group
// invite codes (SPEC-05 R11).
const inviteAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewInviteCode returns a 10-char Crockford Base32 group invite code.
func NewInviteCode() string {
	buf := make([]byte, 10)
	raw := make([]byte, 10)
	_, _ = rand.Read(raw)
	for i, b := range raw {
		buf[i] = inviteAlphabet[int(b)%len(inviteAlphabet)]
	}
	return string(buf)
}
