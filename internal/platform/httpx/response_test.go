package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIPByDefaultIgnoresForwardedHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.10:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.1, 198.51.100.2")

	if got := ClientIP(r); got != "203.0.113.10" {
		t.Fatalf("ClientIP() = %q, want direct peer", got)
	}
}

func TestResolveClientIPTrustedProxyParsesRightmostUntrusted(t *testing.T) {
	handler := ResolveClientIP([]string{"127.0.0.1", "10.0.0.0/8"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ClientIP(r); got != "198.51.100.7" {
			t.Fatalf("ClientIP() = %q", got)
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.3:443"
	req.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.2, bad-value, 10.0.0.1")
	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestResolveClientIPDoesNotTrustForwardedPeerByDefault(t *testing.T) {
	handler := ResolveClientIP(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ClientIP(r); got != "198.51.100.9" {
			t.Fatalf("ClientIP() = %q", got)
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.9:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.99")
	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestResolveClientIPFallsBackToPeerWhenChainIsAllTrusted(t *testing.T) {
	handler := ResolveClientIP([]string{"10.0.0.0/8"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ClientIP(r); got != "10.0.0.1" {
			t.Fatalf("ClientIP() = %q", got)
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:80"
	req.Header.Set("X-Forwarded-For", "10.0.0.2, 10.0.0.3")
	handler.ServeHTTP(httptest.NewRecorder(), req)
}
