// Package httpx implements the unified HTTP contract: response envelope,
// request-id / recovery middleware, cursor codec. SPEC-00 §2.3.
package httpx

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strings"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/ids"
)

// Envelope is the single response shape (ADR-007).
type Envelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Data      any    `json:"data,omitempty"`
	RequestID string `json:"request_id"`
}

type requestIDKey struct{}
type clientIPKey struct{}

// RequestID middleware: generate (or propagate) a request id (R7).
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-Id")
		if rid == "" {
			rid = ids.New()
		}
		ctx := context.WithValue(r.Context(), requestIDKey{}, rid)
		w.Header().Set("X-Request-Id", rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Recover converts panics into INTERNAL_ERROR responses (R9).
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				Error(w, r, apperrors.New(apperrors.InternalError, "internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// JSON writes a success envelope.
func JSON(w http.ResponseWriter, r *http.Request, status int, data any) {
	write(w, r, status, Envelope{
		Code:      string(apperrors.OK),
		Message:   "ok",
		Data:      data,
		RequestID: GetRequestID(r),
	})
}

// Error writes an error envelope with the mapped HTTP status (R5, R6).
func Error(w http.ResponseWriter, r *http.Request, err error) {
	ae := apperrors.AsApp(err)
	write(w, r, apperrors.HTTPStatus(ae.Code), Envelope{
		Code:      string(ae.Code),
		Message:   ae.Message,
		RequestID: GetRequestID(r),
	})
}

func write(w http.ResponseWriter, _ *http.Request, status int, env Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

// ResolveClientIP stores the caller IP after applying the trusted proxy policy.
// X-Forwarded-For is ignored unless the direct peer is explicitly trusted.
func ResolveClientIP(trusted []string) func(http.Handler) http.Handler {
	prefixes := parseTrustedPrefixes(trusted)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := remoteIP(r.RemoteAddr)
			if ip.IsValid() && trustedIP(prefixes, ip) {
				if resolved, ok := forwardedClientIP(r.Header.Get("X-Forwarded-For"), prefixes); ok {
					ip = resolved
				}
			}
			value := ""
			if ip.IsValid() {
				value = ip.String()
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, value)))
		})
	}
}

// ClientIP returns the caller address for audit and rate-limit keys.
func ClientIP(r *http.Request) string {
	if v, ok := r.Context().Value(clientIPKey{}).(string); ok && v != "" {
		return v
	}
	if ip := remoteIP(r.RemoteAddr); ip.IsValid() {
		return ip.String()
	}
	return r.RemoteAddr
}

func parseTrustedPrefixes(values []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		if prefix, err := netip.ParsePrefix(value); err == nil {
			out = append(out, prefix)
			continue
		}
		if addr, err := netip.ParseAddr(value); err == nil {
			addr = addr.Unmap()
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
	return out
}

func remoteIP(remoteAddr string) netip.Addr {
	host := remoteAddr
	if value, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = value
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

func trustedIP(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func forwardedClientIP(header string, trusted []netip.Prefix) (netip.Addr, bool) {
	parts := strings.Split(header, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			continue
		}
		addr = addr.Unmap()
		if !trustedIP(trusted, addr) {
			return addr, true
		}
	}
	return netip.Addr{}, false
}

// GetRequestID returns the id set by the RequestID middleware.
func GetRequestID(r *http.Request) string {
	if v, ok := r.Context().Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

// DecodeBody parses a JSON body with strict field checking.
func DecodeBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apperrors.Wrap(apperrors.InvalidArgument, "malformed JSON body", err)
	}
	return nil
}

// EncodeCursor serializes an opaque page cursor (R10).
func EncodeCursor(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeCursor deserializes an opaque page cursor; bad input → INVALID_ARGUMENT.
func DecodeCursor(s string, dst any) error {
	if s == "" {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return apperrors.Wrap(apperrors.InvalidArgument, "invalid cursor", err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return apperrors.Wrap(apperrors.InvalidArgument, "invalid cursor", err)
	}
	return nil
}
