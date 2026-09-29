//go:build integration

package operator

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

type fakeIdentity struct{ ids map[int64]bool }

func (f fakeIdentity) IsOperator(_ context.Context, userID int64) bool { return f.ids[userID] }

type env struct {
	db  *sql.DB
	svc *Service
	clk time.Time
}

func newEnv(t *testing.T, ops map[int64]bool) *env {
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
	clk := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	svc := New(db, fakeIdentity{ids: ops}, func() time.Time { return clk })
	return &env{db: db, svc: svc, clk: clk}
}

var userSeq atomic.Int64

func (e *env) newUser(t *testing.T) int64 {
	t.Helper()
	phone := fmt.Sprintf("+86%08d%04d", time.Now().UnixNano()%100000000, userSeq.Add(1))
	account := "u" + phone[3:]
	res, err := e.db.ExecContext(context.Background(),
		`INSERT INTO users (phone, account_name, password_hash, created_at, updated_at)
		 VALUES (?, ?, 'x', ?, ?)`, phone, account, e.clk, e.clk)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	id, _ := res.LastInsertId()
	t.Cleanup(func() {
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM users WHERE id = ?`, id)
	})
	return id
}

// T1: non-operator cannot list reports.
func TestT1_NonOperatorForbidden(t *testing.T) {
	e := newEnv(t, map[int64]bool{})
	u := e.newUser(t)
	_, err := e.svc.ListReports(context.Background(), u, "", 0, 10)
	if ae := apperrors.AsApp(err); ae == nil || ae.Code != apperrors.Forbidden {
		t.Fatalf("expected forbidden, got %v", err)
	}
}

// T2: duplicate report returns same id.
func TestT2_DuplicateReport(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	u := e.newUser(t)
	id1, err := e.svc.SubmitReport(context.Background(), u, "message", 9999, "spam", "")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := e.svc.SubmitReport(context.Background(), u, "message", 9999, "spam", "")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("expected same id, got %d vs %d", id1, id2)
	}
	t.Cleanup(func() {
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM reports WHERE id = ?`, id1)
	})
}

// T3: resolve report transitions status.
func TestT3_ResolveReport(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	u := e.newUser(t)
	id, _ := e.svc.SubmitReport(context.Background(), u, "moment", 8888, "abuse", "")
	t.Cleanup(func() {
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM reports WHERE id = ?`, id)
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM moderation_actions WHERE target_id = ?`, id)
	})
	if err := e.svc.ResolveReport(context.Background(), 1, id, "confirmed"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ResolveReport(context.Background(), 1, id, "confirmed"); apperrors.AsApp(err).Code != apperrors.StateConflict {
		t.Fatalf("expected conflict on re-resolve, got %v", err)
	}
}

// T4: queue stats returns counts.
func TestT4_QueueStats(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	stats, err := e.svc.QueueStats(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if stats.OutboxPending < 0 || stats.RetryPending < 0 || stats.DeadLetters < 0 {
		t.Fatalf("negative counts: %+v", stats)
	}
}
