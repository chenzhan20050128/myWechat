//go:build integration

package message

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/wechat/internal/conversation"
	"github.com/example/wechat/internal/platform/clock"
	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

type fakeFriend struct{}

func (fakeFriend) CanSendMessage(_ context.Context, from, to int64) (bool, string, error) {
	if from == 0 || to == 0 {
		return false, "no friendship", nil
	}
	return true, "", nil
}

type fakeGroup struct{}

func (fakeGroup) CanSend(_ context.Context, groupID, userID int64) error { return nil }
func (fakeGroup) IsModerator(_ context.Context, groupID, userID int64) (bool, error) {
	return true, nil
}
func (fakeGroup) GroupByConversation(_ context.Context, conversationID int64) (GroupRowView, error) {
	return GroupRowView{GroupID: conversationID, Name: "g", Status: "active", ConversationID: conversationID}, nil
}
func (fakeGroup) WasMemberAt(_ context.Context, groupID, userID int64, at time.Time) (bool, error) {
	return true, nil
}
func (fakeGroup) ListGroupConversationsForUser(_ context.Context, userID int64) ([]GroupConvView, error) {
	return nil, nil
}

type fakeMedia struct{}

func (fakeMedia) AssertReady(_ context.Context, ownerID, objectID int64, kind string) error { return nil }
func (fakeMedia) OnReferencesRemoved(_ context.Context, objectIDs []int64) error             { return nil }

type env struct {
	db   *sql.DB
	svc  *Service
	clk  *clock.Fake
	conv *conversation.Service
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
	convs := conversation.New(db, clk)
	svc := New(db, fakeFriend{}, fakeGroup{}, fakeMedia{}, convs, clk.Now)
	return &env{db: db, svc: svc, clk: clk, conv: convs}
}

var seq atomic.Int64

func (e *env) newUser(t *testing.T) int64 {
	t.Helper()
	phone := fmt.Sprintf("+86%08d%04d", time.Now().UnixNano()%100000000, seq.Add(1))
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

func (e *env) newDirect(t *testing.T, a, b int64) int64 {
	t.Helper()
	var id int64
	err := mysqlx.WithinTx(context.Background(), e.db, func(tx mysqlx.Tx) error {
		var err error
		id, err = e.conv.CreateDirectTx(context.Background(), tx, a, b)
		return err
	})
	if err != nil {
		t.Fatalf("create direct: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM messages WHERE conversation_id = ?`, id)
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM conversation_members WHERE conversation_id = ?`, id)
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM conversations WHERE id = ?`, id)
	})
	return id
}

func textPayload(s string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"content": s})
	return b
}

// T1: duplicate client_msg_id returns the same row.
func TestT1IdempotentSend(t *testing.T) {
	e := newEnv(t)
	a, b := e.newUser(t), e.newUser(t)
	cid := e.newDirect(t, a, b)
	in := SendInput{ClientMsgID: "abc-1", Type: TypeText, Payload: textPayload("hello")}
	r1, err := e.svc.Send(context.Background(), a, cid, in)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	r2, err := e.svc.Send(context.Background(), a, cid, in)
	if err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if r1.MessageID != r2.MessageID || r1.ConversationSeq != r2.ConversationSeq {
		t.Fatalf("idempotency broken: %+v vs %+v", r1, r2)
	}
	var n int
	_ = e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, cid).Scan(&n)
	if n != 1 {
		t.Fatalf("want 1 message, got %d", n)
	}
}

// T2: empty/unknown type invalid.
func TestT2InvalidPayload(t *testing.T) {
	e := newEnv(t)
	a, b := e.newUser(t), e.newUser(t)
	cid := e.newDirect(t, a, b)
	if _, err := e.svc.Send(context.Background(), a, cid, SendInput{ClientMsgID: "t2a", Type: TypeText, Payload: textPayload("")}); err == nil {
		t.Fatal("empty text should fail")
	} else if ae := apperrors.AsApp(err); ae.Code != apperrors.InvalidArgument {
		t.Fatalf("want INVALID_ARGUMENT, got %s", ae.Code)
	}
	if _, err := e.svc.Send(context.Background(), a, cid, SendInput{ClientMsgID: "t2b", Type: "weird", Payload: textPayload("x")}); err == nil {
		t.Fatal("unknown type should fail")
	}
}

// T3: seq continuity under concurrent sends.
func TestT6SeqContinuity(t *testing.T) {
	e := newEnv(t)
	a, b := e.newUser(t), e.newUser(t)
	cid := e.newDirect(t, a, b)
	done := make(chan error, 50)
	for i := 0; i < 50; i++ {
		go func(n int) {
			_, err := e.svc.Send(context.Background(), a, cid, SendInput{
				ClientMsgID: fmt.Sprintf("seq-%d", n), Type: TypeText, Payload: textPayload("x"),
			})
			done <- err
		}(i)
	}
	for i := 0; i < 50; i++ {
		if err := <-done; err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	rows, err := e.db.QueryContext(context.Background(), `SELECT conversation_seq FROM messages WHERE conversation_id = ? ORDER BY conversation_seq`, cid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seqs []uint64
	for rows.Next() {
		var s uint64
		_ = rows.Scan(&s)
		seqs = append(seqs, s)
	}
	if len(seqs) != 50 {
		t.Fatalf("want 50 messages, got %d", len(seqs))
	}
	for i, s := range seqs {
		if s != uint64(i+1) {
			t.Fatalf("hole at %d: got %d", i, s)
		}
	}
}

// T7: recall within 2 minutes; after 2 minutes conflict.
func TestT7RecallWindow(t *testing.T) {
	e := newEnv(t)
	a, b := e.newUser(t), e.newUser(t)
	cid := e.newDirect(t, a, b)
	r, err := e.svc.Send(context.Background(), a, cid, SendInput{ClientMsgID: "r1", Type: TypeText, Payload: textPayload("hi")})
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(119 * time.Second)
	if err := e.svc.Recall(context.Background(), a, r.MessageID); err != nil {
		t.Fatalf("recall within window: %v", err)
	}
	r2, _ := e.svc.Send(context.Background(), a, cid, SendInput{ClientMsgID: "r2", Type: TypeText, Payload: textPayload("hi")})
	e.clk.Advance(121 * time.Second)
	if err := e.svc.Recall(context.Background(), a, r2.MessageID); err == nil {
		t.Fatal("recall after 2min should fail")
	}
}

// T8: recall masks payload in history.
func TestT8RecallMasked(t *testing.T) {
	e := newEnv(t)
	a, b := e.newUser(t), e.newUser(t)
	cid := e.newDirect(t, a, b)
	r, _ := e.svc.Send(context.Background(), a, cid, SendInput{ClientMsgID: "m1", Type: TypeText, Payload: textPayload("secret")})
	_ = e.svc.Recall(context.Background(), a, r.MessageID)
	h, err := e.svc.History(context.Background(), a, cid, HistoryInput{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(h.Messages))
	}
	if h.Messages[0].Status != StatusRecalled || len(h.Messages[0].Payload) != 0 {
		t.Fatalf("recalled message should have null payload: %+v", h.Messages[0])
	}
}

// T12: read cursor only advances.
func TestT12ReadCursorForward(t *testing.T) {
	e := newEnv(t)
	a, b := e.newUser(t), e.newUser(t)
	cid := e.newDirect(t, a, b)
	for i := 0; i < 3; i++ {
		_, _ = e.svc.Send(context.Background(), a, cid, SendInput{ClientMsgID: fmt.Sprintf("c%d", i), Type: TypeText, Payload: textPayload("x")})
	}
	if _, err := e.svc.ReadSeq(context.Background(), b, cid, 3); err != nil {
		t.Fatal(err)
	}
	last, err := e.svc.ReadSeq(context.Background(), b, cid, 1)
	if err != nil {
		t.Fatal(err)
	}
	if last != 3 {
		t.Fatalf("cursor should not retreat, got %d", last)
	}
}

// T13: mark-unread does not move last_read_seq.
func TestT13MarkUnreadKeepsCursor(t *testing.T) {
	e := newEnv(t)
	a, b := e.newUser(t), e.newUser(t)
	cid := e.newDirect(t, a, b)
	_, _ = e.svc.Send(context.Background(), a, cid, SendInput{ClientMsgID: "mu1", Type: TypeText, Payload: textPayload("x")})
	_, _ = e.svc.ReadSeq(context.Background(), b, cid, 1)
	if err := e.svc.MarkUnread(context.Background(), b, cid); err != nil {
		t.Fatal(err)
	}
	st, err := loadSettings(context.Background(), e.db, cid, b)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastReadSeq != 1 || !st.IsMarkedUnread {
		t.Fatalf("expected cursor=1 unread=true, got %+v", st)
	}
}

// T15: retention worker flips expired rows and removes assets.
func TestT15Retention(t *testing.T) {
	e := newEnv(t)
	a, b := e.newUser(t), e.newUser(t)
	cid := e.newDirect(t, a, b)
	r, _ := e.svc.Send(context.Background(), a, cid, SendInput{ClientMsgID: "exp1", Type: TypeText, Payload: textPayload("bye")})
	e.clk.Advance(181 * 24 * time.Hour)
	n, err := e.svc.SweepExpired(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 expired, got %d", n)
	}
	var status string
	_ = e.db.QueryRowContext(context.Background(), `SELECT status FROM messages WHERE id = ?`, r.MessageID).Scan(&status)
	if status != StatusExpired {
		t.Fatalf("want expired, got %s", status)
	}
}

// T10: pin quota.
func TestT10PinQuota(t *testing.T) {
	e := newEnv(t)
	a, b := e.newUser(t), e.newUser(t)
	cid := e.newDirect(t, a, b)
	var ids []int64
	for i := 0; i < MaxMessagePins+1; i++ {
		r, err := e.svc.Send(context.Background(), a, cid, SendInput{ClientMsgID: fmt.Sprintf("p%d", i), Type: TypeText, Payload: textPayload("x")})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.MessageID)
	}
	for i := 0; i < MaxMessagePins; i++ {
		if err := e.svc.Pin(context.Background(), a, cid, ids[i]); err != nil {
			t.Fatalf("pin %d: %v", i, err)
		}
	}
	err := e.svc.Pin(context.Background(), a, cid, ids[MaxMessagePins])
	if err == nil {
		t.Fatal("expected quota exceeded")
	}
	if ae := apperrors.AsApp(err); ae.Code != apperrors.QuotaExceeded {
		t.Fatalf("want QUOTA_EXCEEDED, got %s", ae.Code)
	}
}

// T18: markDelivered idempotent.
func TestT18MarkDelivered(t *testing.T) {
	e := newEnv(t)
	a, b := e.newUser(t), e.newUser(t)
	cid := e.newDirect(t, a, b)
	r, _ := e.svc.Send(context.Background(), a, cid, SendInput{ClientMsgID: "d1", Type: TypeText, Payload: textPayload("x")})
	if err := e.svc.MarkDelivered(context.Background(), r.MessageID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.MarkDelivered(context.Background(), r.MessageID); err != nil {
		t.Fatalf("idempotent ack: %v", err)
	}
	var status string
	_ = e.db.QueryRowContext(context.Background(), `SELECT status FROM messages WHERE id = ?`, r.MessageID).Scan(&status)
	if status != StatusDelivered {
		t.Fatalf("want delivered, got %s", status)
	}
}
