//go:build integration

package content

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

type fakeOperator struct{ ids map[int64]bool }

func (f fakeOperator) IsOperator(_ context.Context, userID int64) bool { return f.ids[userID] }

type fakeConversation struct{ nextID atomic.Int64 }

func (f *fakeConversation) CreateOfficialServiceTx(_ context.Context, _ mysqlx.Tx, userID int64) (int64, error) {
	return f.nextID.Add(1) + 100000, nil
}

type env struct {
	db   *sql.DB
	svc  *Service
	clk  *fakeClock
	conv *fakeConversation
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newEnv(t *testing.T, operatorIDs map[int64]bool) *env {
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
	conv := &fakeConversation{}
	svc := New(db, fakeOperator{ids: operatorIDs}, conv, nil, clk.Now)
	return &env{db: db, svc: svc, clk: clk, conv: conv}
}

var userSeq atomic.Int64

func (e *env) newUser(t *testing.T) int64 {
	t.Helper()
	random, _ := cryptorand.Int(cryptorand.Reader, big.NewInt(100000000))
	phone := fmt.Sprintf("+86%08d%04d", random.Int64(), userSeq.Add(1))
	account := "u" + phone[3:]
	res, err := e.db.ExecContext(context.Background(),
		`INSERT INTO users (phone, account_name, password_hash, created_at, updated_at)
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

func (e *env) newAccount(t *testing.T, operatorID int64) int64 {
	t.Helper()
	name := fmt.Sprintf("acct-%d-%d", time.Now().UnixNano(), userSeq.Add(1))
	id, err := e.svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name: name, Intro: "hello", OperatorID: operatorID,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM official_accounts WHERE id = ?`, id)
	})
	return id
}

func (e *env) activate(t *testing.T, operatorID, accountID int64) {
	t.Helper()
	if err := e.svc.UpdateAccount(context.Background(), &UpdateAccountInput{
		ID: accountID, OperatorID: operatorID, Status: "active",
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
}

func isCode(err error, code apperrors.Code) bool {
	ae := apperrors.AsApp(err)
	return ae != nil && ae.Code == code
}

// T1: frozen/closed accounts are invisible.
func TestT1_FrozenInvisible(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	op := e.newUser(t)
	_ = op
	accountID := e.newAccount(t, 1)
	if _, err := e.svc.LookupAccount(context.Background(), "acct"); err == nil {
		t.Fatal("draft account must not be visible")
	}
	e.activate(t, 1, accountID)
	acc, err := e.svc.LookupAccount(context.Background(), mustAccountName(e.db, accountID))
	if err != nil || acc.ID != accountID {
		t.Fatalf("active lookup: acc=%+v err=%v", acc, err)
	}
	if err := e.svc.UpdateAccount(context.Background(), &UpdateAccountInput{ID: accountID, OperatorID: 1, Status: "frozen"}); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if _, err := e.svc.LookupAccount(context.Background(), acc.Name); !isCode(err, apperrors.ResourceUnavailable) {
		t.Fatalf("frozen must be not found, got %v", err)
	}
}

func mustAccountName(db *sql.DB, id int64) string {
	var name string
	_ = db.QueryRowContext(context.Background(), `SELECT name FROM official_accounts WHERE id = ?`, id).Scan(&name)
	return name
}

// T2: follow creates welcome notification; repeat is idempotent.
func TestT2_FollowWelcome(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	u := e.newUser(t)
	if err := e.svc.Follow(context.Background(), accountID, u); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if err := e.svc.Follow(context.Background(), accountID, u); err != nil {
		t.Fatalf("repeat follow: %v", err)
	}
	items, err := e.svc.ListNotifications(context.Background(), u, 0, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	welcome := 0
	for _, n := range items {
		if n.Kind == "welcome" {
			welcome++
		}
	}
	if welcome != 1 {
		t.Fatalf("expected exactly 1 welcome, got %d", welcome)
	}
}

// T3: starting service session without follow is FORBIDDEN.
func TestT3_SessionWithoutFollow(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	u := e.newUser(t)
	_, err := e.svc.StartSession(context.Background(), accountID, u)
	if !isCode(err, apperrors.Forbidden) {
		t.Fatalf("expected forbidden, got %v", err)
	}
}

// T4: one active session per (account, user) is idempotent.
func TestT4_SingleActiveSession(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	u := e.newUser(t)
	if err := e.svc.Follow(context.Background(), accountID, u); err != nil {
		t.Fatal(err)
	}
	r1, err := e.svc.StartSession(context.Background(), accountID, u)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	r2, err := e.svc.StartSession(context.Background(), accountID, u)
	if err != nil {
		t.Fatalf("start 2: %v", err)
	}
	if r1.ID != r2.ID || !r2.Created == false || r1.Number != r2.Number {
		t.Fatalf("expected same session, got r1=%+v r2=%+v", r1, r2)
	}
	if r2.Created {
		t.Fatal("second start must not create")
	}
}

// T5: after close, sending is STATE_CONFLICT; new session increments number.
func TestT5_CloseThenRestart(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	u := e.newUser(t)
	if err := e.svc.Follow(context.Background(), accountID, u); err != nil {
		t.Fatal(err)
	}
	r1, _ := e.svc.StartSession(context.Background(), accountID, u)
	if err := e.svc.CloseSession(context.Background(), r1.ID, u); err != nil {
		t.Fatalf("close: %v", err)
	}
	err := e.svc.CanSendServiceMessage(context.Background(), r1.ConversationID, u, "user")
	if !isCode(err, apperrors.StateConflict) {
		t.Fatalf("expected state conflict, got %v", err)
	}
	r2, err := e.svc.StartSession(context.Background(), accountID, u)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if r2.Number != r1.Number+1 {
		t.Fatalf("expected session number %d, got %d", r1.Number+1, r2.Number)
	}
}

// T6: staff can only send text (handled at handler level; here StaffReply
// enforces assignment). The "image" case is enforced in the handler body.
func TestT6_StaffReplyOnlyAssigned(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	op := e.newUser(t)
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	u := e.newUser(t)
	if err := e.svc.Follow(context.Background(), accountID, u); err != nil {
		t.Fatal(err)
	}
	sess, _ := e.svc.StartSession(context.Background(), accountID, u)
	staff := e.newUser(t)
	if _, err := e.db.ExecContext(context.Background(),
		`INSERT INTO official_account_staff (account_id, user_id, created_at) VALUES (?, ?, ?)`,
		accountID, staff, e.clk.Now()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM official_account_staff WHERE account_id = ? AND user_id = ?`, accountID, staff)
	})
	if err := e.svc.StaffReply(context.Background(), sess.ID, staff); err != nil {
		t.Fatalf("staff first reply must assign: %v", err)
	}
	other := e.newUser(t)
	if _, err := e.db.ExecContext(context.Background(),
		`INSERT INTO official_account_staff (account_id, user_id, created_at) VALUES (?, ?, ?)`,
		accountID, other, e.clk.Now()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM official_account_staff WHERE account_id = ? AND user_id = ?`, accountID, other)
	})
	if err := e.svc.StaffReply(context.Background(), sess.ID, other); !isCode(err, apperrors.Forbidden) {
		t.Fatalf("other staff must be forbidden, got %v", err)
	}
	_ = op
}

// T7: non-staff cannot read staff sessions.
func TestT7_NonStaffForbidden(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	u := e.newUser(t)
	_, err := e.svc.ListStaffSessions(context.Background(), u, accountID, "", 0, 10)
	if !isCode(err, apperrors.Forbidden) {
		t.Fatalf("expected forbidden, got %v", err)
	}
}

// T8: menu with 4 top-level items is rejected.
func TestT8_MenuTooManyTopLevel(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	items := make([]MenuInput, 4)
	for i := range items {
		items[i] = MenuInput{Level: 1, Position: i + 1, Label: fmt.Sprintf("m%d", i), Action: "start_service"}
	}
	err := e.svc.SaveMenu(context.Background(), 1, accountID, items)
	if !isCode(err, apperrors.InvalidArgument) {
		t.Fatalf("expected invalid, got %v", err)
	}
}

// T9: menu pointing to an unpublished article is rejected.
func TestT9_MenuRejectsUnpublished(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	artID, err := e.svc.CreateArticle(context.Background(), &CreateArticleInput{
		OperatorID: 1, AccountID: accountID, Title: "t", Body: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = e.svc.SaveMenu(context.Background(), 1, accountID, []MenuInput{
		{Level: 1, Position: 1, Label: "a", Action: "open_article", ArticleID: &artID},
	})
	if !isCode(err, apperrors.InvalidArgument) {
		t.Fatalf("expected invalid, got %v", err)
	}
}

// T10: published article cannot be edited.
func TestT10_PublishedImmutable(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	artID, _ := e.svc.CreateArticle(context.Background(), &CreateArticleInput{
		OperatorID: 1, AccountID: accountID, Title: "t", Body: "b",
	})
	if err := e.svc.PublishArticle(context.Background(), 1, artID); err != nil {
		t.Fatal(err)
	}
	err := e.svc.UpdateArticle(context.Background(), &UpdateArticleInput{
		OperatorID: 1, ID: artID, Title: "new",
	})
	if !isCode(err, apperrors.StateConflict) {
		t.Fatalf("expected state conflict, got %v", err)
	}
}

// T11: unpublished article returns CONTENT_UNAVAILABLE.
func TestT11_UnpublishedArticle(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	artID, _ := e.svc.CreateArticle(context.Background(), &CreateArticleInput{
		OperatorID: 1, AccountID: accountID, Title: "t", Body: "b",
	})
	u := e.newUser(t)
	if _, err := e.svc.GetArticle(context.Background(), u, artID); !isCode(err, apperrors.ContentUnavailable) {
		t.Fatalf("draft must be unavailable, got %v", err)
	}
	if err := e.svc.PublishArticle(context.Background(), 1, artID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetArticle(context.Background(), u, artID); err != nil {
		t.Fatalf("published must be readable, got %v", err)
	}
	if err := e.svc.UnpublishArticle(context.Background(), 1, artID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetArticle(context.Background(), u, artID); !isCode(err, apperrors.ContentUnavailable) {
		t.Fatalf("unpublished must be unavailable, got %v", err)
	}
}

// T12: read record upsert refreshes last_read_at.
func TestT12_ReadUpsert(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	artID, _ := e.svc.CreateArticle(context.Background(), &CreateArticleInput{
		OperatorID: 1, AccountID: accountID, Title: "t", Body: "b",
	})
	if err := e.svc.PublishArticle(context.Background(), 1, artID); err != nil {
		t.Fatal(err)
	}
	u := e.newUser(t)
	first := e.clk.Now()
	if _, err := e.svc.GetArticle(context.Background(), u, artID); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Hour)
	if _, err := e.svc.GetArticle(context.Background(), u, artID); err != nil {
		t.Fatal(err)
	}
	var firstAt, lastAt time.Time
	if err := e.db.QueryRowContext(context.Background(),
		`SELECT first_read_at, last_read_at FROM official_article_reads WHERE article_id = ? AND user_id = ?`,
		artID, u).Scan(&firstAt, &lastAt); err != nil {
		t.Fatal(err)
	}
	if !firstAt.Equal(first) {
		t.Fatalf("first_read_at not stable: %v vs %v", firstAt, first)
	}
	if lastAt.Sub(firstAt) < time.Minute {
		t.Fatalf("last_read_at must advance: first=%v last=%v", firstAt, lastAt)
	}
}

// T13: 4 notifications/day limit — 4th skipped.
func TestT13_DailyLimit(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	u := e.newUser(t)
	if err := e.svc.Follow(context.Background(), accountID, u); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		artID, _ := e.svc.CreateArticle(context.Background(), &CreateArticleInput{
			OperatorID: 1, AccountID: accountID, Title: fmt.Sprintf("t%d", i), Body: "b",
		})
		if err := e.svc.PublishArticle(context.Background(), 1, artID); err != nil {
			t.Fatal(err)
		}
		n, err := e.svc.FanoutArticleNotifications(context.Background(), artID)
		if err != nil {
			t.Fatal(err)
		}
		if i < 3 && n != 1 {
			t.Fatalf("article %d: expected 1 notification, got %d", i, n)
		}
		if i == 3 && n != 0 {
			t.Fatalf("article 3: expected 0 notifications (4th/day), got %d", n)
		}
	}
}

// T14: muted users get no notifications but articles remain readable.
func TestT14_MutedNoNotification(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	u := e.newUser(t)
	if err := e.svc.Follow(context.Background(), accountID, u); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetMute(context.Background(), accountID, u, true); err != nil {
		t.Fatal(err)
	}
	artID, _ := e.svc.CreateArticle(context.Background(), &CreateArticleInput{
		OperatorID: 1, AccountID: accountID, Title: "t", Body: "b",
	})
	if err := e.svc.PublishArticle(context.Background(), 1, artID); err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.FanoutArticleNotifications(context.Background(), artID)
	if err != nil || n != 0 {
		t.Fatalf("muted fanout n=%d err=%v", n, err)
	}
	if _, err := e.svc.GetArticle(context.Background(), u, artID); err != nil {
		t.Fatalf("article must remain readable: %v", err)
	}
}

// T15: re-publishing same article is idempotent (no duplicate notifications).
func TestT15_RepublishIdempotent(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	_ = e.newUser(t)
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	u := e.newUser(t)
	if err := e.svc.Follow(context.Background(), accountID, u); err != nil {
		t.Fatal(err)
	}
	artID, _ := e.svc.CreateArticle(context.Background(), &CreateArticleInput{
		OperatorID: 1, AccountID: accountID, Title: "t", Body: "b",
	})
	if err := e.svc.PublishArticle(context.Background(), 1, artID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.FanoutArticleNotifications(context.Background(), artID); err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.FanoutArticleNotifications(context.Background(), artID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("second fanout must skip, got %d", n)
	}
}

// T16: non-operator cannot create account.
func TestT16_NonOperator(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	u := e.newUser(t)
	_, err := e.svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name: "x", OperatorID: u,
	})
	if !isCode(err, apperrors.Forbidden) {
		t.Fatalf("expected forbidden, got %v", err)
	}
}

func TestConcurrentStartSessionIsIdempotent(t *testing.T) {
	e := newEnv(t, map[int64]bool{1: true})
	accountID := e.newAccount(t, 1)
	e.activate(t, 1, accountID)
	userID := e.newUser(t)
	if err := e.svc.Follow(context.Background(), accountID, userID); err != nil {
		t.Fatal(err)
	}

	const starters = 12
	results := make([]*StartSessionResult, starters)
	errs := make([]error, starters)
	var wg sync.WaitGroup
	for i := 0; i < starters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = e.svc.StartSession(context.Background(), accountID, userID)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("starter %d: %v", i, err)
		}
		if results[i].ID != results[0].ID || results[i].Number != results[0].Number {
			t.Fatalf("starter %d got %+v, want %+v", i, results[i], results[0])
		}
	}
	var sessions int
	if err := e.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM service_sessions WHERE official_account_id = ? AND user_id = ?`,
		accountID, userID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Fatalf("sessions = %d, want 1", sessions)
	}
}

var _ = json.RawMessage{}
