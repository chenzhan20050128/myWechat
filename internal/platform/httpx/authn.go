package httpx

import (
	"context"
	"net/http"
	"strings"

	apperrors "github.com/example/wechat/internal/platform/errors"
)

// Principal is the authenticated caller identity placed in request context.
// Filled by the auth module's Authenticator — platform defines the port,
// auth implements it (dependency inversion, ADR-013).
type Principal struct {
	UserID            int64
	DeviceID          string
	SessionID         int64
	MustChangePassword bool // temporary-password sessions see almost nothing (SPEC-01 R12)
}

type principalKey struct{}

// Authenticator validates a bearer token and returns the principal.
// Implementation lives in internal/auth (SPEC-01).
type Authenticator interface {
	Authenticate(ctx context.Context, accessToken string) (Principal, error)
}

// RequireAuth wraps routes that need a logged-in device session. Sessions in
// the must-change-password state are rejected here (SPEC-01 R12) unless the
// route chain also contains AllowMustChangePassword.
func RequireAuth(authn Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				Error(w, r, apperrors.Unauth("missing bearer token"))
				return
			}
			p, err := authn.Authenticate(r.Context(), token)
			if err != nil {
				Error(w, r, err)
				return
			}
			if p.MustChangePassword && !mustChangeAllowed(r.Context()) {
				Error(w, r, apperrors.New(apperrors.MustChangePassword,
					"password must be changed before using this endpoint"))
				return
			}
			ctx := context.WithValue(r.Context(), principalKey{}, p)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type mustChangeKey struct{}

// AllowMustChangePassword marks a route chain usable by temporary-password
// sessions (password change, logout).
func AllowMustChangePassword(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), mustChangeKey{}, true)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func mustChangeAllowed(ctx context.Context) bool {
	v, _ := ctx.Value(mustChangeKey{}).(bool)
	return v
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// PrincipalFrom extracts the caller identity; ok=false on public routes.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// MustPrincipal is PrincipalFrom with a typed error for misuse.
func MustPrincipal(ctx context.Context) (Principal, error) {
	if p, ok := PrincipalFrom(ctx); ok {
		return p, nil
	}
	return Principal{}, apperrors.Unauth("not authenticated")
}
