package sigtoken

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	c := New("secret-a")
	payload := []byte(`{"u":42,"e":1893456000}`)
	token := c.Sign(payload)
	got, err := c.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: %s", got)
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	token := New("secret-a").Sign([]byte("payload"))
	if _, err := New("secret-b").Verify(token); err != ErrInvalid {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	c := New("secret-a")
	token := c.Sign([]byte(`{"u":42}`))
	i := strings.LastIndexByte(token, '.')
	tampered := base64.RawURLEncoding.EncodeToString([]byte(`{"u":43}`)) + token[i:]
	if _, err := c.Verify(tampered); err != ErrInvalid {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	c := New("secret-a")
	for _, tok := range []string{"", ".", "abc", "abc.", ".deadbeef", "a.b.c", "!!!.deadbeef"} {
		if _, err := c.Verify(tok); err != ErrInvalid {
			t.Fatalf("token %q: want ErrInvalid, got %v", tok, err)
		}
	}
}

func TestSignIsURLSafeAndDeterministic(t *testing.T) {
	c := New("secret-a")
	tok := c.Sign([]byte("payload"))
	if strings.ContainsAny(tok, "+/=") {
		t.Fatalf("token is not URL-safe: %q", tok)
	}
	if tok != c.Sign([]byte("payload")) {
		t.Fatal("signing is not deterministic")
	}
}

func TestEqualMAC(t *testing.T) {
	c := New("secret-a")
	if !c.EqualMAC("ab", "ab") || c.EqualMAC("ab", "ac") {
		t.Fatal("EqualMAC misbehaves")
	}
}
