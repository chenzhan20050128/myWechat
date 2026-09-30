package cache

import (
	"context"
	"testing"
	"time"
)

func TestNewRejectsUnsupportedDriver(t *testing.T) {
	if _, err := New("memcached", "127.0.0.1:11211", 0); err == nil {
		t.Fatal("unsupported cache driver must fail startup")
	}
}

func TestMemoryTTLAndAtomicOperations(t *testing.T) {
	c := NewMemoryWithClock(func() time.Time { return time.Unix(100, 0) })
	ctx := context.Background()

	if _, ok := c.Get(ctx, "missing"); ok {
		t.Fatal("missing key returned a value")
	}
	c.Set(ctx, "ttl", "value", time.Second)
	if got, ok := c.Get(ctx, "ttl"); !ok || got != "value" {
		t.Fatalf("Get() = %q, %v", got, ok)
	}

	if got := c.Incr(ctx, "count", time.Minute); got != 1 {
		t.Fatalf("first Incr() = %d", got)
	}
	if got := c.Incr(ctx, "count", time.Minute); got != 2 {
		t.Fatalf("second Incr() = %d", got)
	}
	if c.SetNX(ctx, "nx", "one", time.Minute) != true {
		t.Fatal("first SetNX() did not win")
	}
	if c.SetNX(ctx, "nx", "two", time.Minute) {
		t.Fatal("second SetNX() overwrote an existing key")
	}
	c.Del(ctx, "ttl", "count", "nx")
	for _, key := range []string{"ttl", "count", "nx"} {
		if _, ok := c.Get(ctx, key); ok {
			t.Fatalf("%s was not deleted", key)
		}
	}
}
