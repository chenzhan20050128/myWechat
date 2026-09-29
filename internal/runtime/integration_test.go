//go:build integration

package runtime

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/wechat/internal/platform/ids"
	"github.com/example/wechat/internal/platform/mq"
	"github.com/example/wechat/internal/platform/mysqlx"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

type fakePublisher struct {
	mu     sync.Mutex
	events []mq.Event
	fail   error
}

func (f *fakePublisher) Publish(_ context.Context, _ string, e mq.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.events = append(f.events, e)
	return nil
}

func (f *fakePublisher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

type env struct {
	db   *sql.DB
	clk  *fakeClock
	pub  *fakePublisher
	relay *Relay
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("WECHAT_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("WECHAT_TEST_MYSQL_DSN is not set")
	}
	db, err := mysqlx.Open(dsn, 8, 4, time.Minute)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := &fakeClock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	pub := &fakePublisher{}
	relay := NewRelay(db, pub, "test-relay", clk.Now)
	return &env{db: db, clk: clk, pub: pub, relay: relay}
}

func (e *env) insertPending(t *testing.T, n int) []string {
	t.Helper()
	inserted := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := ids.New()
		_, err := e.db.ExecContext(context.Background(), `
			INSERT INTO outbox_events (event_id, type, aggregate_id, version, queue, status, created_at, updated_at)
			VALUES (?, 'message.stored', '42', 0, 'wechat.message.push', 'pending', ?, ?)`,
			id, e.clk.Now(), e.clk.Now())
		if err != nil {
			t.Fatalf("seed outbox: %v", err)
		}
		inserted = append(inserted, id)
	}
	t.Cleanup(func() {
		for _, id := range inserted {
			_, _ = e.db.ExecContext(context.Background(), `DELETE FROM outbox_events WHERE event_id = ?`, id)
		}
	})
	return inserted
}

// T1: relay lease → publish → confirm → published.
func TestT1_RelayPublishFlow(t *testing.T) {
	e := newEnv(t)
	ids := e.insertPending(t, 3)
	n, err := e.relay.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if n < 3 {
		t.Fatalf("expected at least 3 leased, got %d", n)
	}
	for _, id := range ids {
		var status string
		if err := e.db.QueryRowContext(context.Background(),
			`SELECT status FROM outbox_events WHERE event_id = ?`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "published" {
			t.Fatalf("event %s status = %s", id, status)
		}
	}
}

// T2: failed publish marks failed but does not block the next batch.
func TestT2_FailedPublish(t *testing.T) {
	e := newEnv(t)
	e.pub.fail = errors.New("broker down")
	ids := e.insertPending(t, 1)
	_, _ = e.relay.RunOnce(context.Background())
	var status string
	if err := e.db.QueryRowContext(context.Background(),
		`SELECT status FROM outbox_events WHERE event_id = ?`, ids[0]).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("expected failed, got %s", status)
	}
}

// T3: stale publishing event gets re-leased.
func TestT3_StaleLeaseReclaimed(t *testing.T) {
	e := newEnv(t)
	id := ids.New()
	_, err := e.db.ExecContext(context.Background(), `
		INSERT INTO outbox_events (event_id, type, aggregate_id, version, queue, status, lease_owner, lease_expires_at, created_at, updated_at)
		VALUES (?, 'message.stored', '42', 0, 'wechat.message.push', 'publishing', 'old', UTC_TIMESTAMP(6) - INTERVAL 1 MINUTE, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6))`,
		id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM outbox_events WHERE event_id = ?`, id)
	})
	rows, err := e.relay.LeaseOutbound(context.Background())
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.EventID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected stale event to be re-leased, got %d rows", len(rows))
	}
}

// T4: inbox dedupe — same event processed twice only runs handler once.
func TestT4_InboxDedupe(t *testing.T) {
	e := newEnv(t)
	called := int32(0)
	c := NewInbox(e.db, "test-consumer", func(err error) FailureClass { return Transient }, e.clk.Now)
	ev := mq.Event{EventID: ids.New(), Type: "message.stored", AggregateID: "1"}
	handler := func(_ context.Context, _ mq.Event) error {
		atomic.AddInt32(&called, 1)
		return nil
	}
	t.Cleanup(func() {
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM inbox_events WHERE consumer_name = 'test-consumer' AND event_id = ?`, ev.EventID)
	})
	if err := c.Dispatch(context.Background(), ev, handler); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	if err := c.Dispatch(context.Background(), ev, handler); !errors.Is(err, ErrDuplicateEvent) {
		t.Fatalf("second dispatch should be duplicate, got %v", err)
	}
	if called != 1 {
		t.Fatalf("handler called %d times, want 1", called)
	}
}

// T5: transient failure enqueues a retry task.
func TestT5_TransientRetry(t *testing.T) {
	e := newEnv(t)
	c := NewInbox(e.db, "test-consumer", func(err error) FailureClass { return Transient }, e.clk.Now)
	ev := mq.Event{EventID: ids.New(), Type: "message.stored", AggregateID: "2"}
	t.Cleanup(func() {
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM inbox_events WHERE consumer_name = 'test-consumer' AND event_id = ?`, ev.EventID)
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM async_retry_tasks WHERE consumer_name = 'test-consumer' AND event_id = ?`, ev.EventID)
	})
	err := c.Dispatch(context.Background(), ev, func(_ context.Context, _ mq.Event) error {
		return errors.New("boom")
	})
	if err != nil {
		t.Fatalf("dispatch should swallow retry: %v", err)
	}
	var attempt int
	err = e.db.QueryRowContext(context.Background(),
		`SELECT attempt_count FROM async_retry_tasks WHERE consumer_name = 'test-consumer' AND event_id = ?`, ev.EventID).Scan(&attempt)
	if err != nil || attempt != 1 {
		t.Fatalf("attempt=%d err=%v", attempt, err)
	}
}

// T6: permanent failure marks inbox failed and inserts dead letter.
func TestT6_PermanentDead(t *testing.T) {
	e := newEnv(t)
	c := NewInbox(e.db, "test-consumer", func(err error) FailureClass { return Permanent }, e.clk.Now)
	ev := mq.Event{EventID: ids.New(), Type: "message.stored", AggregateID: "3"}
	t.Cleanup(func() {
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM inbox_events WHERE consumer_name = 'test-consumer' AND event_id = ?`, ev.EventID)
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM async_dead_letters WHERE event_id = ? AND consumer_name = 'test-consumer'`, ev.EventID)
	})
	if err := c.Dispatch(context.Background(), ev, func(_ context.Context, _ mq.Event) error {
		return errors.New("bad payload")
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var status string
	if err := e.db.QueryRowContext(context.Background(),
		`SELECT status FROM inbox_events WHERE consumer_name = 'test-consumer' AND event_id = ?`, ev.EventID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("expected failed, got %s", status)
	}
	var one int
	if err := e.db.QueryRowContext(context.Background(),
		`SELECT 1 FROM async_dead_letters WHERE event_id = ? AND consumer_name = 'test-consumer'`, ev.EventID).Scan(&one); err != nil {
		t.Fatalf("dead letter missing: %v", err)
	}
}

// T7: lifecycle task runner executes each task.
func TestT7_LifecycleRunner(t *testing.T) {
	var calls int32
	l := NewLifecycle(
		LifecycleTask{Name: "a", Run: func(_ context.Context) (int, error) { atomic.AddInt32(&calls, 1); return 1, nil }},
		LifecycleTask{Name: "b", Run: func(_ context.Context) (int, error) { atomic.AddInt32(&calls, 1); return 1, nil }},
	)
	if errs := l.RunOnce(context.Background()); len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
}
