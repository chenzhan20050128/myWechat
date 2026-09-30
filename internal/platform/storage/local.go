package storage

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProxyPath is the API route that streams local-driver objects (R16). It lives
// here, next to the driver that emits it, so the URL and the route cannot drift.
const ProxyPath = "/api/v1/media/download"

// Local is the dev object store: files under a root directory, keys like
// "obj/2026/09/uuid.ext" map to root/obj/2026/09/uuid.ext. SignGetURL returns
// an absolute URL to the API's download proxy, carrying an HMAC token the
// proxy re-verifies (R14/R16).
type Local struct {
	root    string
	signer  *Signer
	baseURL string
	// now produces the signing time so URL expiry and proxy verification share
	// one clock — tests inject a fake, production injects clock.System.
	now func() time.Time
}

// NewLocal builds a Local store rooted at dir; baseURL is the externally
// reachable origin of this API (config.Storage.PublicBaseURL).
func NewLocal(dir, baseURL string, signer *Signer, now func() time.Time) (*Local, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &Local{root: abs, signer: signer, baseURL: strings.TrimSuffix(baseURL, "/"), now: now}, nil
}

// sanitize rejects traversal, absolute keys, backslashes and NUL bytes.
func sanitize(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") ||
		strings.Contains(key, "..") || strings.ContainsAny(key, "\\\x00") {
		return "", errors.New("storage: invalid key")
	}
	// keys use a restricted alphabet by construction (obj/|tmp/ + uuid + ext)
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '/', r == '.', r == '-', r == '_':
		default:
			return "", errors.New("storage: invalid key charset")
		}
	}
	return filepath.FromSlash(key), nil
}

func (l *Local) path(key string) (string, error) {
	clean, err := sanitize(key)
	if err != nil {
		return "", err
	}
	p := filepath.Join(l.root, clean)
	if !strings.HasPrefix(p, l.root+string(os.PathSeparator)) {
		return "", errors.New("storage: path escape")
	}
	return p, nil
}

func (l *Local) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), p); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	return nil
}

func (l *Local) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrObjectNotFound
	}
	return f, err
}

func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (l *Local) Exists(_ context.Context, key string) (bool, error) {
	p, err := l.path(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (l *Local) SignGetURL(_ context.Context, key string, ttl time.Duration) (string, error) {
	if _, err := l.path(key); err != nil {
		return "", err
	}
	token := l.signer.Sign(key, l.now().Add(ttl))
	return l.baseURL + ProxyPath + "?token=" + url.QueryEscape(token), nil
}

// VerifyToken exposes Signer.Verify for the media proxy endpoint.
func (l *Local) VerifyToken(token string, now time.Time) (string, error) {
	return l.signer.Verify(token, now)
}

var (
	_ ObjectStore   = (*Local)(nil)
	_ ProxyVerifier = (*Local)(nil)
)
