// Package sigtoken signs and verifies opaque payloads with HMAC-SHA256, so
// every module hands out tamper-proof tokens in one format (personal QR codes,
// local-driver download URLs) instead of inventing its own (SPEC-00 §2.6/§2.9).
package sigtoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// ErrInvalid reports a malformed or tampered token. Callers map it to their
// own domain error (never to INTERNAL_ERROR).
var ErrInvalid = errors.New("sigtoken: invalid token")

// Codec signs payloads with a single secret.
type Codec struct{ secret []byte }

// New builds a codec. An empty secret is a config error, validated at load.
func New(secret string) *Codec { return &Codec{secret: []byte(secret)} }

// MAC returns the hex HMAC-SHA256 of payload.
func (c *Codec) MAC(payload string) string {
	m := hmac.New(sha256.New, c.secret)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// EqualMAC compares two MACs in constant time.
func (c *Codec) EqualMAC(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

// Sign returns "base64url(payload).hexMAC" — URL-safe, single-field.
func (c *Codec) Sign(payload []byte) string {
	enc := base64.RawURLEncoding.EncodeToString(payload)
	return enc + "." + c.MAC(enc)
}

// Verify returns the payload when the MAC matches.
func (c *Codec) Verify(token string) ([]byte, error) {
	i := strings.LastIndexByte(token, '.')
	if i <= 0 || i == len(token)-1 {
		return nil, ErrInvalid
	}
	enc, mac := token[:i], token[i+1:]
	if !c.EqualMAC(c.MAC(enc), mac) {
		return nil, ErrInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return nil, ErrInvalid
	}
	return payload, nil
}
