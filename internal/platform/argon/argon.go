// Package argon implements password hashing with Argon2id in PHC string
// format, with rehash-on-login support. SPEC-00; design-review D10.
package argon

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the Argon2id cost parameters. Defaults follow the conservative
// OWASP/RFC 9106 guidance (m=64MiB, t=3, p=2).
type Params struct {
	Time    uint32
	Memory  uint32 // KiB
	Threads uint8
	KeyLen  uint32
	SaltLen uint32
}

// Default returns the default production parameters.
func Default() Params {
	return Params{Time: 3, Memory: 64 * 1024, Threads: 2, KeyLen: 32, SaltLen: 16}
}

// Hash derives a PHC-format Argon2id string: $argon2id$v=19$m=..,t=..,p=..$salt$hash.
func Hash(password string, p Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("argon: salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify reports whether password matches a PHC-format hash.
func Verify(password, phc string) (bool, error) {
	salt, want, p, err := parse(phc)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether phc was produced with weaker/different
// parameters than the currently configured ones (triggers rehash-on-login).
func NeedsRehash(phc string, current Params) bool {
	_, _, p, err := parse(phc)
	if err != nil {
		return true
	}
	return p.Time != current.Time || p.Memory != current.Memory || p.Threads != current.Threads
}

func parse(phc string) (salt, key []byte, p Params, err error) {
	parts := strings.Split(phc, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, nil, p, fmt.Errorf("argon: malformed hash string")
	}
	var version int
	if _, err = fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != 19 {
		return nil, nil, p, fmt.Errorf("argon: unsupported version %q", parts[2])
	}
	if _, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return nil, nil, p, fmt.Errorf("argon: malformed params %q", parts[3])
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return nil, nil, p, fmt.Errorf("argon: salt: %w", err)
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return nil, nil, p, fmt.Errorf("argon: key: %w", err)
	}
	return salt, key, p, nil
}
