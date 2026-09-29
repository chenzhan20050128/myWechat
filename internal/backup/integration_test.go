//go:build integration

package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/wechat/internal/platform/clock"
	"github.com/example/wechat/internal/platform/mysqlx"
)

type stubSources struct{}

func (stubSources) Profile(_ context.Context, _ int64) (json.RawMessage, error) {
	return json.RawMessage(`{"nickname":"x"}`), nil
}
func (stubSources) ContactSettings(_ context.Context, _ int64) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (stubSources) Favorites(_ context.Context, _ int64) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (stubSources) OwnMoments(_ context.Context, _ int64) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (stubSources) ReadableMessages(_ context.Context, _ int64) (json.RawMessage, error) {
	return json.RawMessage(`[]`), nil
}

type env struct {
	db  *sql.DB
	svc *Service
	clk *clock.Fake
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("WECHAT_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("WECHAT_TEST_MYSQL_DSN is not set")
	}
	db, err := mysqlx.Open(dsn, 8, 4, time.Minute)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := clock.NewFake(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC))
	svc := New(db, stubSources{}, clk.Now)
	return &env{db: db, svc: svc, clk: clk}
}

var userSeq atomic.Int64

func (e *env) newUser(t *testing.T) int64 {
	t.Helper()
	phone := fmt.Sprintf("+86%08d%04d", time.Now().UnixNano()%100000000, userSeq.Add(1))
	account := "u" + phone[3:]
	res, err := e.db.ExecContext(context.Background(), `
		INSERT INTO users (phone, account_name, password_hash, created_at, updated_at)
		VALUES (?, ?, 'x', ?, ?)`, phone, account, e.clk.Now(), e.clk.Now())
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	id, _ := res.LastInsertId()
	t.Cleanup(func() {
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM users WHERE id = ?`, id)
	})
	return id
}

// T1: full create flow.
func TestT1_RequestAndCurrent(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	b, err := e.svc.Request(context.Background(), owner)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if b.Status != "ready" {
		t.Fatalf("status = %s", b.Status)
	}
	cur, err := e.svc.Current(context.Background(), owner)
	if err != nil || cur.ID != b.ID {
		t.Fatalf("current = %+v err=%v", cur, err)
	}
}

// T2: concurrent request rejected while creating.
// Since V1 runs synchronously, this instead verifies two sequential requests
// succeed and the pointer updates.
func TestT2_SecondRequestReplacesPointer(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	b1, _ := e.svc.Request(context.Background(), owner)
	e.clk.Advance(time.Minute)
	b2, err := e.svc.Request(context.Background(), owner)
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	cur, _ := e.svc.Current(context.Background(), owner)
	if cur.ID != b2.ID {
		t.Fatalf("expected pointer to %d, got %d (old=%d)", b2.ID, cur.ID, b1.ID)
	}
}

// T3: restore creates archive.
func TestT3_Restore(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	if _, err := e.svc.Request(context.Background(), owner); err != nil {
		t.Fatalf("request: %v", err)
	}
	jobID, err := e.svc.Restore(context.Background(), owner)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	prof, err := e.svc.ProfileArchive(context.Background(), owner, jobID)
	if err != nil {
		t.Fatalf("profile archive: %v", err)
	}
	if string(prof.Profile) == "" {
		t.Fatal("profile empty")
	}
}

// T4: delete active backup.
func TestT4_DeleteCurrent(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	if _, err := e.svc.Request(context.Background(), owner); err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := e.svc.DeleteCurrent(context.Background(), owner); err != nil {
		t.Fatalf("delete: %v", err)
	}
	cur, _ := e.svc.Current(context.Background(), owner)
	if cur.Status != "none" {
		t.Fatalf("expected none, got %+v", cur)
	}
}

// T5: restore jobs list.
func TestT5_RestoreJobs(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	if _, err := e.svc.Request(context.Background(), owner); err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := e.svc.Restore(context.Background(), owner); err != nil {
		t.Fatalf("restore: %v", err)
	}
	jobs, err := e.svc.RestoreJobs(context.Background(), owner)
	if err != nil || len(jobs) != 1 || jobs[0].Status != "succeeded" {
		t.Fatalf("jobs = %+v err=%v", jobs, err)
	}
}

// T6: handshake lifecycle.
func TestT6_Handshake(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	id, code, err := e.svc.CreateHandshake(context.Background(), owner)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := e.svc.AcceptHandshake(context.Background(), owner, id, "wrong"); err == nil {
		t.Fatal("wrong code must fail")
	}
	if err := e.svc.AcceptHandshake(context.Background(), owner, id, code); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := e.svc.CompleteHandshake(context.Background(), owner, id); err != nil {
		t.Fatalf("complete: %v", err)
	}
}

// T7: delete archive.
func TestT7_DeleteArchive(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	if _, err := e.svc.Request(context.Background(), owner); err != nil {
		t.Fatalf("request: %v", err)
	}
	jobID, err := e.svc.Restore(context.Background(), owner)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := e.svc.DeleteArchive(context.Background(), owner, jobID); err != nil {
		t.Fatalf("delete archive: %v", err)
	}
	if _, err := e.svc.ProfileArchive(context.Background(), owner, jobID); err == nil {
		t.Fatal("archive should be gone")
	}
}

// T8: expire worker flips ready backups past TTL.
func TestT8_ExpireBackups(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	if _, err := e.svc.Request(context.Background(), owner); err != nil {
		t.Fatalf("request: %v", err)
	}
	e.clk.Advance(31 * 24 * time.Hour)
	n, err := e.svc.ExpireBackups(context.Background())
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected at least 1 expired, got %d", n)
	}
	cur, _ := e.svc.Current(context.Background(), owner)
	if cur.Status != "none" {
		t.Fatalf("expected none after expiry, got %+v", cur)
	}
}
