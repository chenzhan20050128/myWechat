//go:build integration

package favorite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/wechat/internal/platform/clock"
	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

type fakeMedia struct{}

func (fakeMedia) OnReferencesRemoved(_ context.Context, _ []int64) error { return nil }
func (fakeMedia) EnqueueGCTx(_ context.Context, _ mysqlx.Tx, _ []int64, _ time.Time) error {
	return nil
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
	svc := New(db, fakeMedia{}, nil, clk.Now)
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

// T1: create a text favorite.
func TestT1_CreateText(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	content := json.RawMessage(`{"content":"hello"}`)
	id, dup, err := e.svc.Create(context.Background(), &CreateInput{OwnerID: owner, Kind: "text", Content: content})
	if err != nil || dup || id == 0 {
		t.Fatalf("create: id=%d dup=%v err=%v", id, dup, err)
	}
	f, err := e.svc.Get(context.Background(), owner, id)
	if err != nil || f.Kind != "text" {
		t.Fatalf("get: %+v err=%v", f, err)
	}
}

// T2: idempotent dedupe on source_message_id.
func TestT2_IdempotentSourceMessage(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	msgID := int64(424242)
	content := json.RawMessage(`{"content":"hi"}`)
	id1, dup1, err := e.svc.Create(context.Background(), &CreateInput{OwnerID: owner, Kind: "text", Content: content, SourceMessageID: &msgID})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	id2, dup2, err := e.svc.Create(context.Background(), &CreateInput{OwnerID: owner, Kind: "text", Content: content, SourceMessageID: &msgID})
	if err != nil || !dup2 || id1 != id2 {
		t.Fatalf("second: id1=%d id2=%d dup=%v err=%v", id1, id2, dup2, err)
	}
	_ = dup1
}

func TestConcurrentSourceFavoritesDedupe(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	msgID := int64(424243)
	content := json.RawMessage(`{"content":"concurrent"}`)
	input := &CreateInput{OwnerID: owner, Kind: "text", Content: content, SourceMessageID: &msgID}

	const creators = 12
	ids := make([]int64, creators)
	dups := make([]bool, creators)
	errs := make([]error, creators)
	var wg sync.WaitGroup
	for i := 0; i < creators; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], dups[i], errs[i] = e.svc.Create(context.Background(), input)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("creator %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("creator %d got favorite %d, want %d", i, ids[i], ids[0])
		}
	}
	successes := 0
	for _, dup := range dups {
		if dup {
			successes++
		}
	}
	if successes != creators-1 {
		t.Fatalf("duplicate responses = %d, want %d", successes, creators-1)
	}
}

// T3: invalid kind rejected.
func TestT3_InvalidKind(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	_, _, err := e.svc.Create(context.Background(), &CreateInput{OwnerID: owner, Kind: "nope"})
	if err == nil {
		t.Fatal("expected invalid kind error")
	}
}

// T4: create tag then apply to favorite.
func TestT4_Tags(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	id, _, err := e.svc.Create(context.Background(), &CreateInput{OwnerID: owner, Kind: "text", Content: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := e.svc.CreateTag(context.Background(), owner, "work"); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := e.svc.ApplyTags(context.Background(), owner, id, []string{"work"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	tags, err := e.svc.ListTags(context.Background(), owner)
	if err != nil || len(tags) != 1 || tags[0].Tag != "work" {
		t.Fatalf("tags = %+v err=%v", tags, err)
	}
}

// T5: tag quota.
func TestT5_TagQuota(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	for i := 0; i < MaxTagsPerUser; i++ {
		if err := e.svc.CreateTag(context.Background(), owner, fmt.Sprintf("t%d", i)); err != nil {
			t.Fatalf("seed tag %d: %v", i, err)
		}
	}
	err := e.svc.CreateTag(context.Background(), owner, "overflow")
	if err == nil {
		t.Fatal("expected quota exceeded")
	}
	if appErr := apperrors.AsApp(err); appErr == nil || appErr.Code != apperrors.QuotaExceeded {
		t.Fatalf("expected QUOTA_EXCEEDED, got %v", err)
	}
}

// T6: non-owner cannot read.
func TestT6_NonOwnerForbidden(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	intruder := e.newUser(t)
	id, _, _ := e.svc.Create(context.Background(), &CreateInput{OwnerID: owner, Kind: "text", Content: json.RawMessage(`{}`)})
	if _, err := e.svc.Get(context.Background(), intruder, id); err == nil {
		t.Fatal("intruder should not read")
	}
}

// T7: delete soft-deletes.
func TestT7_Delete(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	id, _, _ := e.svc.Create(context.Background(), &CreateInput{OwnerID: owner, Kind: "text", Content: json.RawMessage(`{}`)})
	if err := e.svc.Delete(context.Background(), owner, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := e.svc.Get(context.Background(), owner, id); err == nil {
		t.Fatal("deleted should not be readable")
	}
}

// T8: list pagination.
func TestT8_List(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	for i := 0; i < 5; i++ {
		_, _, _ = e.svc.Create(context.Background(), &CreateInput{OwnerID: owner, Kind: "text", Content: json.RawMessage(`{}`)})
	}
	items, err := e.svc.List(context.Background(), owner, 0, "", "", 10)
	if err != nil || len(items) != 5 {
		t.Fatalf("list = %d err=%v", len(items), err)
	}
}

// T9: cleanup preview requires user references; with none, empty preview.
func TestT9_PreviewEmpty(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	res, err := e.svc.Preview(context.Background(), &PreviewInput{UserID: owner, Scope: "user_references"})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if res.Count != 0 {
		t.Fatalf("expected 0 items, got %d", res.Count)
	}
}

// T10: confirm on previewed job transitions to done.
func TestT10_ConfirmDone(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	res, err := e.svc.Preview(context.Background(), &PreviewInput{UserID: owner, Scope: "user_references"})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	success, failed, err := e.svc.Confirm(context.Background(), owner, res.CleanupID)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if success != 0 || failed != 0 {
		t.Fatalf("success=%d failed=%d (expected 0/0)", success, failed)
	}
	job, items, err := e.svc.Job(context.Background(), owner, res.CleanupID)
	if err != nil {
		t.Fatalf("job: %v", err)
	}
	if job.Status != "done" {
		t.Fatalf("status = %s", job.Status)
	}
	_ = items
}
