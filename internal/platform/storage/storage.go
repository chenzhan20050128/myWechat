// Package storage defines the object storage port (S3-compatible in
// production, local-disk in dev) plus signed download URLs. Storage holds no
// authorization: callers must authenticate before signing (SPEC-00 §2.6).
package storage

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/example/wechat/internal/platform/sigtoken"
)

var ErrObjectNotFound = errors.New("storage: object not found")

// ObjectStore is the minimal port used by the media module.
type ObjectStore interface {
	// Put stores size bytes from r under key. Keys are caller-generated.
	Put(ctx context.Context, key string, r io.Reader, size int64, mime string) error
	// Get returns a reader for the stored object; caller closes it.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the object; missing keys are not an error.
	Delete(ctx context.Context, key string) error
	// Exists reports whether key exists.
	Exists(ctx context.Context, key string) (bool, error)
	// SignGetURL returns a short-lived, absolute, ready-to-use download URL for
	// key. Every driver returns a complete URL — S3 a pre-signed one, local one
	// pointing at the API's own proxy — so no caller ever branches on driver.
	SignGetURL(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// ProxyVerifier is implemented by drivers that serve bytes through the API
// itself (the local dev driver). Media's proxy endpoint needs it; S3 hands out
// pre-signed URLs and never does. The composition root decides which driver to
// pass, so domain code holds no driver switch.
type ProxyVerifier interface {
	VerifyToken(token string, now time.Time) (string, error)
}

// Signer issues and verifies HMAC tokens for local-driver downloads (R16).
// The MAC primitive is the shared platform one (sigtoken).
type Signer struct {
	codec *sigtoken.Codec
}

// NewSigner builds a Signer from the configured signing secret.
func NewSigner(secret string) *Signer { return &Signer{codec: sigtoken.New(secret)} }

// Sign produces a "key|expUnix|hexmac" token for the proxy endpoint.
func (s *Signer) Sign(key string, exp time.Time) string {
	es := strconv.FormatInt(exp.Unix(), 10)
	return key + "|" + es + "|" + s.codec.MAC(key+"|"+es)
}

// Verify checks a signed token; returns the key when valid.
func (s *Signer) Verify(token string, now time.Time) (string, error) {
	// split into exactly 3 fields: key|exp|mac (keys cannot contain '|' by
	// construction — see Local.sanitize)
	i1 := strings.LastIndexByte(token, '|')
	if i1 < 0 {
		return "", errors.New("storage: malformed token")
	}
	i0 := strings.LastIndexByte(token[:i1], '|')
	if i0 < 0 {
		return "", errors.New("storage: malformed token")
	}
	key, es, mac := token[:i0], token[i0+1:i1], token[i1+1:]
	exp, err := strconv.ParseInt(es, 10, 64)
	if err != nil {
		return "", errors.New("storage: malformed expiry")
	}
	if !s.codec.EqualMAC(mac, s.codec.MAC(key+"|"+es)) {
		return "", errors.New("storage: bad signature")
	}
	if now.Unix() > exp {
		return "", errors.New("storage: token expired")
	}
	return key, nil
}
