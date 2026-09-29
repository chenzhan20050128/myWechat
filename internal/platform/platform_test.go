package platform_test

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example/wechat/internal/platform/argon"
	"github.com/example/wechat/internal/platform/cache"
	"github.com/example/wechat/internal/platform/pagination"
	"github.com/example/wechat/internal/platform/ratelimit"
	"github.com/example/wechat/internal/platform/storage"
	"github.com/example/wechat/internal/platform/validate"
)

// SPEC-00 A3: memory SetNX is atomic under concurrency.
func TestMemorySetNXConcurrency(t *testing.T) {
	m := cache.NewMemory()
	var mu sync.Mutex
	winners := 0
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.SetNX(context.Background(), "k", "v", time.Minute) {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("SetNX winners = %d, want 1", winners)
	}
}

func TestMemoryIncrWindow(t *testing.T) {
	now := time.Now()
	m := cache.NewMemoryWithClock(func() time.Time { return now })
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		if got := m.Incr(ctx, "c", time.Minute); got != int64(i) {
			t.Fatalf("Incr = %d, want %d", got, i)
		}
	}
	now = now.Add(2 * time.Minute) // window expired
	if got := m.Incr(ctx, "c", time.Minute); got != 1 {
		t.Fatalf("Incr after expiry = %d, want 1", got)
	}
}

func TestRatelimitFixedWindow(t *testing.T) {
	c := cache.NewMemory()
	l := ratelimit.New(c, "login", 3, time.Minute)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow(context.Background(), "u1"); !ok {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if ok, _ := l.Allow(context.Background(), "u1"); ok {
		t.Fatal("4th request should be limited")
	}
	if ok, _ := l.Allow(context.Background(), "u2"); !ok {
		t.Fatal("different subject must have own window")
	}
}

// SPEC-00 A4: local storage returns a ready-to-use proxy URL whose token
// round-trips through verify; tampering and expiry fail.
func TestLocalStorageSignVerify(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	s, err := storage.NewLocal(dir, "https://api.example.test", storage.NewSigner("secret"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Put(ctx, "obj/2026/09/abc.bin", strings.NewReader("hello"), 5, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	raw, err := s.SignGetURL(ctx, "obj/2026/09/abc.bin", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("SignGetURL must return a URL: %v", err)
	}
	if u.Scheme != "https" || u.Host != "api.example.test" || u.Path != storage.ProxyPath {
		t.Fatalf("URL = %q, want the configured origin + %s", raw, storage.ProxyPath)
	}
	token := u.Query().Get("token")
	if token == "" {
		t.Fatal("URL carries no token")
	}
	key, err := s.VerifyToken(token, time.Now().Add(30*time.Second))
	if err != nil || key != "obj/2026/09/abc.bin" {
		t.Fatalf("verify = %q, %v", key, err)
	}
	// expired
	if _, err := s.VerifyToken(token, time.Now().Add(2*time.Minute)); err == nil {
		t.Fatal("expired token must fail")
	}
	// tampered
	if _, err := s.VerifyToken(token+"x", time.Now().Add(30*time.Second)); err == nil {
		t.Fatal("tampered token must fail")
	}
	// traversal key rejected
	if err := s.Put(ctx, "../escape.bin", strings.NewReader("x"), 1, ""); err == nil {
		t.Fatal("traversal key must be rejected")
	}
}

func TestArgonHashVerifyRehash(t *testing.T) {
	p := argon.Params{Time: 1, Memory: 8 * 1024, Threads: 1, KeyLen: 16, SaltLen: 8}
	h, err := argon.Hash("passw0rd123", p)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := argon.Verify("passw0rd123", h)
	if err != nil || !ok {
		t.Fatalf("verify = %v, %v", ok, err)
	}
	ok, _ = argon.Verify("wrong", h)
	if ok {
		t.Fatal("wrong password must fail")
	}
	stronger := argon.Params{Time: 2, Memory: 16 * 1024, Threads: 1, KeyLen: 16, SaltLen: 8}
	if !argon.NeedsRehash(h, stronger) {
		t.Fatal("NeedsRehash should detect parameter drift")
	}
	if argon.NeedsRehash(h, p) {
		t.Fatal("NeedsRehash false positive")
	}
}

func TestValidateRules(t *testing.T) {
	cases := []struct {
		name string
		got  bool
		want bool
	}{
		{"password ok", validate.Password("abc12345"), true},
		{"password no digit", validate.Password("abcdefgh"), false},
		{"password no letter", validate.Password("12345678"), false},
		{"password too short", validate.Password("ab12345"), false},
		{"account ok", validate.AccountName("abc_123"), true},
		{"account too short", validate.AccountName("ab12"), false},
		{"account uppercase", validate.AccountName("Abc123"), false},
		{"account reserved", validate.AccountName("admin1"), true},
		{"account reserved exact", validate.AccountName("admin"), false},
		{"phone ok", validate.Phone("+8613800138000"), true},
		{"phone letters", validate.Phone("138001abc"), false},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %v want %v", c.name, c.got, c.want)
		}
	}
	if validate.NormalizePhone("+86 138-0013-8000") != "+8613800138000" {
		t.Fatal("phone normalization broken")
	}
	if validate.NormalizeAccountName("  AbC_99 ") != "abc_99" {
		t.Fatal("account normalization broken")
	}
	if validate.LowerHex64("not-hex!") {
		t.Fatal("hex64 false positive")
	}
	if !validate.LowerHex64("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef") {
		t.Fatal("hex64 false negative")
	}
}

func TestPaginationCursor(t *testing.T) {
	c, err := pagination.Encode(pagination.Page{Kind: "req", Sort: 123, ID: 456})
	if err != nil {
		t.Fatal(err)
	}
	p, err := pagination.Decode(c)
	if err != nil || p.Kind != "req" || p.Sort != 123 || p.ID != 456 {
		t.Fatalf("roundtrip failed: %+v %v", p, err)
	}
	if _, err := pagination.Decode("!!!not-base64!!!"); err == nil {
		t.Fatal("bad cursor must error")
	}
	if pagination.Limit(0) != pagination.DefaultLimit || pagination.Limit(500) != pagination.MaxLimit {
		t.Fatal("limit clamp broken")
	}
}
