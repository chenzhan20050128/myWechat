// Package ratelimit implements fixed-window rate limiting on top of the
// cache interface (design-review D9). Fail-open on driver errors: limits
// protect against abuse, not correctness.
package ratelimit

import (
	"context"
	"time"

	"github.com/example/wechat/internal/platform/cache"
)

// Limiter is a named fixed-window limit.
type Limiter struct {
	cache   cache.Cache
	name    string
	limit   int64
	window  time.Duration
}

// New builds a limiter keyed `rl:<name>:<subject>`.
func New(c cache.Cache, name string, limit int64, window time.Duration) *Limiter {
	return &Limiter{cache: c, name: name, limit: limit, window: window}
}

// Allow consumes one unit for subject. Returns (ok, retryAfter).
// When the cache fails (Incr == -1) the call is allowed (fail-open).
func (l *Limiter) Allow(ctx context.Context, subject string) (bool, time.Duration) {
	key := "rl:" + l.name + ":" + subject
	n := l.cache.Incr(ctx, key, l.window)
	if n < 0 {
		return true, 0
	}
	if n > l.limit {
		// Compute approximate time left in the window: we cannot read the
		// TTL back portably, so report the full window as retry-after hint.
		return false, l.window
	}
	return true, 0
}
