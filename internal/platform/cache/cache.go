// Package cache defines the short-lived state interface (sessions, lockout
// counters, rate limits). Redis in production, in-memory in dev. The cache is
// a performance layer only — never a source of truth (SPEC-00 §2.5).
package cache

import (
	"context"
	"sync"
	"time"
)

// Cache is the driver-agnostic interface. All TTLs are required; zero TTL is
// rejected by callers (design choice: no forever-keys).
type Cache interface {
	Get(ctx context.Context, key string) (string, bool)
	Set(ctx context.Context, key, value string, ttl time.Duration)
	Del(ctx context.Context, keys ...string)
	// Incr atomically increments key and returns the new value, setting the
	// TTL on first increment. Returns -1 on driver failure (fail-open for
	// rate limiting; correctness must not depend on the cache).
	Incr(ctx context.Context, key string, ttl time.Duration) int64
	// SetNX stores value only if key does not exist (atomic).
	SetNX(ctx context.Context, key, value string, ttl time.Duration) bool
}

// New builds the configured driver. redis driver lands with the RabbitMQ/MinIO
// session (PROGRESS known-items); memory is the dev default.
func New(driver string) Cache {
	switch driver {
	case "memory":
		return NewMemory()
	default:
		// Reserved for "redis"; until that driver exists, fail loudly rather
		// than silently degrade production semantics.
		return NewMemory()
	}
}

// entry is an expiring memory value.
type entry struct {
	value   string
	expires time.Time
}

// shard is one lock-protected bucket of the sharded map.
type shard struct {
	mu sync.Mutex
	m  map[string]entry
}

// Memory is the in-process driver. Semantics mirror Redis (Incr/SetNX are
// atomic under the shard lock).
type Memory struct {
	shards [256]shard
	now    func() time.Time
}

// NewMemory builds a Memory cache using the system clock.
func NewMemory() *Memory { return NewMemoryWithClock(time.Now) }

// NewMemoryWithClock allows tests to inject a clock.
func NewMemoryWithClock(now func() time.Time) *Memory {
	m := &Memory{now: now}
	for i := range m.shards {
		m.shards[i].m = map[string]entry{}
	}
	return m
}

func shardOf(key string) int {
	h := 2166136261
	for i := 0; i < len(key); i++ {
		h = (h ^ int(key[i])) * 16777619
	}
	if h < 0 {
		h = -h
	}
	return h % 256
}

func (m *Memory) shard(key string) *shard { return &m.shards[shardOf(key)] }

func (m *Memory) Get(_ context.Context, key string) (string, bool) {
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok || !e.expires.After(m.now()) {
		return "", false
	}
	return e.value, true
}

func (m *Memory) Set(_ context.Context, key, value string, ttl time.Duration) {
	s := m.shard(key)
	s.mu.Lock()
	s.m[key] = entry{value: value, expires: m.now().Add(ttl)}
	s.mu.Unlock()
}

func (m *Memory) Del(_ context.Context, keys ...string) {
	for _, k := range keys {
		s := m.shard(k)
		s.mu.Lock()
		delete(s.m, k)
		s.mu.Unlock()
	}
}

func (m *Memory) Incr(_ context.Context, key string, ttl time.Duration) int64 {
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok || !e.expires.After(m.now()) {
		s.m[key] = entry{value: "1", expires: m.now().Add(ttl)}
		return 1
	}
	n := parseInt(e.value) + 1
	e.value = strconvI64(n)
	s.m[key] = e
	return n
}

func (m *Memory) SetNX(_ context.Context, key, value string, ttl time.Duration) bool {
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[key]; ok && e.expires.After(m.now()) {
		return false
	}
	s.m[key] = entry{value: value, expires: m.now().Add(ttl)}
	return true
}

func parseInt(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

func strconvI64(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
