//go:build integration

package contact

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/wechat/internal/audit"
	"github.com/example/wechat/internal/conversation"
	"github.com/example/wechat/internal/platform/cache"
	"github.com/example/wechat/internal/platform/clock"
	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/httpx"
	"github.com/example/wechat/internal/platform/mysqlx"
	"github.com/example/wechat/internal/platform/sigtoken"
	"github.com/example/wechat/internal/user"
)

// env is one live contact stack: real MySQL, real sibling modules, fake clock.
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
	// contact never touches avatars, so the user media port stays unwired here.
	users := user.New(db, clk.Now, nil)
	convs := conversation.New(db, clk)
	aud := audit.New(clk)
	svc := New(db, users, convs, aud, cache.NewMemory(), sigtoken.New("contact-test-signing-secret"), clk)
	return &env{db: db, svc: svc, clk: clk}
}

// testUser is a freshly created account with its identifiers.
type testUser struct {
	ID       int64
	Phone    string
	Account  string
	Nickname string
}

var seq atomic.Int64

func (e *env) newUser(t *testing.T) testUser { return e.newUserNamed(t, "") }

func (e *env) newUserNamed(t *testing.T, nickname string) testUser {
	t.Helper()
	random, _ := cryptorand.Int(cryptorand.Reader, big.NewInt(100000000))
	tag := fmt.Sprintf("%08d%04d", random.Int64(), seq.Add(1))
	u := testUser{
		Phone:    "+86" + tag,
		Account:  "u" + tag,
		Nickname: nickname,
	}
	if u.Nickname == "" {
		u.Nickname = "nick" + tag
	}
	err := mysqlx.WithinTx(context.Background(), e.db, func(tx mysqlx.Tx) error {
		id, err := user.CreateAccountTx(context.Background(), tx, user.CreateAccountParams{
			Phone: u.Phone, AccountName: u.Account, PasswordHash: "x", Nickname: u.Nickname,
		}, e.clk.Now())
		u.ID = id
		return err
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// apply creates a friend request through the account_name entry point.
func (e *env) apply(t *testing.T, from, to testUser, verify string) RequestRow {
	t.Helper()
	row, err := e.svc.CreateRequest(context.Background(), CreateRequestInput{
		ApplicantID: from.ID, Source: SourceAccountName, AccountName: to.Account, VerifyText: verify,
	})
	if err != nil {
		t.Fatalf("apply %d→%d: %v", from.ID, to.ID, err)
	}
	return row
}

// friendUp drives the pair through apply + accept, ending at epoch 1.
func (e *env) friendUp(t *testing.T, a, b testUser) {
	t.Helper()
	row := e.apply(t, a, b, "hi")
	if _, err := e.svc.AcceptRequest(context.Background(), b.ID, row.ID, "127.0.0.1"); err != nil {
		t.Fatalf("accept %d: %v", row.ID, err)
	}
}

// A1: all five entry points resolve a target and open a pending application.
func TestA1EntryPoints(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	t.Run("phone", func(t *testing.T) {
		a, b := e.newUser(t), e.newUser(t)
		row, err := e.svc.CreateRequest(ctx, CreateRequestInput{
			ApplicantID: a.ID, Source: SourcePhone, Phone: b.Phone, VerifyText: "from phone",
		})
		if err != nil {
			t.Fatalf("phone entry: %v", err)
		}
		if row.TargetID != b.ID || row.Status != RequestPending || row.Source != SourcePhone {
			t.Fatalf("row = %+v", row)
		}
	})

	t.Run("account_name", func(t *testing.T) {
		a, b := e.newUser(t), e.newUser(t)
		row, err := e.svc.CreateRequest(ctx, CreateRequestInput{
			ApplicantID: a.ID, Source: SourceAccountName, AccountName: strings.ToUpper(b.Account),
		})
		if err != nil {
			t.Fatalf("account_name entry: %v", err)
		}
		if row.TargetID != b.ID {
			t.Fatalf("target = %d, want %d", row.TargetID, b.ID)
		}
	})

	t.Run("qrcode", func(t *testing.T) {
		a, b := e.newUser(t), e.newUser(t)
		token, exp, err := e.svc.IssueQRCode(ctx, b.ID)
		if err != nil {
			t.Fatalf("issue qr: %v", err)
		}
		if !exp.Equal(e.clk.Now().Add(QRTokenTTL)) {
			t.Fatalf("expiry = %v", exp)
		}
		row, err := e.svc.CreateRequest(ctx, CreateRequestInput{
			ApplicantID: a.ID, Source: SourceQRCode, QRToken: token,
		})
		if err != nil {
			t.Fatalf("qrcode entry: %v", err)
		}
		if row.TargetID != b.ID {
			t.Fatalf("target = %d, want %d", row.TargetID, b.ID)
		}
	})

	t.Run("group is unavailable in phase 1", func(t *testing.T) {
		a := e.newUser(t)
		_, err := e.svc.CreateRequest(ctx, CreateRequestInput{ApplicantID: a.ID, Source: SourceGroup})
		if code := codeOf(t, err); code != apperrors.ResourceUnavailable {
			t.Fatalf("code = %s, want RESOURCE_UNAVAILABLE", code)
		}
	})

	t.Run("card", func(t *testing.T) {
		a, voucher, subject := e.newUser(t), e.newUser(t), e.newUser(t)
		e.friendUp(t, a, voucher)
		row, err := e.svc.CreateRequest(ctx, CreateRequestInput{
			ApplicantID: a.ID, Source: SourceCard, CardOwnerID: voucher.ID, TargetID: subject.ID,
		})
		if err != nil {
			t.Fatalf("card entry: %v", err)
		}
		if row.TargetID != subject.ID {
			t.Fatalf("target = %d, want %d", row.TargetID, subject.ID)
		}
	})

	t.Run("card from a stranger is refused", func(t *testing.T) {
		a, stranger, subject := e.newUser(t), e.newUser(t), e.newUser(t)
		_, err := e.svc.CreateRequest(ctx, CreateRequestInput{
			ApplicantID: a.ID, Source: SourceCard, CardOwnerID: stranger.ID, TargetID: subject.ID,
		})
		if code := codeOf(t, err); code != apperrors.ResourceUnavailable {
			t.Fatalf("code = %s, want RESOURCE_UNAVAILABLE", code)
		}
	})

	t.Run("unknown and self targets", func(t *testing.T) {
		a := e.newUser(t)
		if code := codeOf(t, mustCreateErr(e, ctx, CreateRequestInput{
			ApplicantID: a.ID, Source: SourcePhone, Phone: "+86130000000000",
		})); code != apperrors.ResourceUnavailable {
			t.Fatalf("unknown phone: code = %s", code)
		}
		if code := codeOf(t, mustCreateErr(e, ctx, CreateRequestInput{
			ApplicantID: a.ID, Source: SourcePhone, Phone: a.Phone,
		})); code != apperrors.InvalidArgument {
			t.Fatalf("self request: code = %s", code)
		}
	})
}

func mustCreateErr(e *env, ctx context.Context, in CreateRequestInput) error {
	_, err := e.svc.CreateRequest(ctx, in)
	return err
}

// A2 / R3: repeat applications collapse into one pending row and refresh it.
func TestA2RepeatApplicationMerges(t *testing.T) {
	e := newEnv(t)
	a, b := e.newUser(t), e.newUser(t)

	first, err := e.svc.CreateRequest(context.Background(), CreateRequestInput{
		ApplicantID: a.ID, Source: SourcePhone, Phone: b.Phone, VerifyText: "first",
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := e.svc.CreateRequest(context.Background(), CreateRequestInput{
		ApplicantID: a.ID, Source: SourceAccountName, AccountName: b.Account, VerifyText: "second",
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("ids differ: %d vs %d", first.ID, second.ID)
	}
	if second.VerifyText != "second" || second.Source != SourceAccountName {
		t.Fatalf("merged row = %+v", second)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM friend_requests WHERE applicant_id=? AND target_id=? AND status='pending'`,
		a.ID, b.ID); n != 1 {
		t.Fatalf("pending rows = %d, want 1", n)
	}

	// The reverse direction is a different application, not a merge.
	if _, err := e.svc.CreateRequest(context.Background(), CreateRequestInput{
		ApplicantID: b.ID, Source: SourcePhone, Phone: a.Phone,
	}); err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM friend_requests WHERE status='pending' AND (applicant_id=? OR applicant_id=?)`,
		a.ID, b.ID); n != 2 {
		t.Fatalf("pending rows = %d, want 2", n)
	}
}

// A3 / R8: accepting opens epoch 1, both settings rows and the direct conversation.
func TestA3AcceptCreatesFriendshipSettingsAndConversation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b := e.newUser(t), e.newUser(t)
	row := e.apply(t, a, b, "please")

	res, err := e.svc.AcceptRequest(ctx, b.ID, row.ID, "127.0.0.1")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if res.Status != RequestAccepted || res.Epoch != 1 || res.ConversationID <= 0 {
		t.Fatalf("result = %+v", res)
	}

	if epoch, ok, err := e.svc.GetActiveFriendship(ctx, a.ID, b.ID); err != nil || !ok || epoch != 1 {
		t.Fatalf("friendship = (%d,%v,%v)", epoch, ok, err)
	}
	if epoch, ok, _ := e.svc.GetActiveFriendship(ctx, b.ID, a.ID); !ok || epoch != 1 {
		t.Fatal("friendship must be visible from both sides")
	}
	if n := e.count(t, `SELECT COUNT(*) FROM conversation_members WHERE conversation_id=? AND user_id IN (?,?)`,
		res.ConversationID, a.ID, b.ID); n != 2 {
		t.Fatalf("conversation members = %d, want 2", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM friend_settings WHERE owner_id IN (?,?) AND friend_id IN (?,?)`,
		a.ID, b.ID, a.ID, b.ID); n != 2 {
		t.Fatalf("settings rows = %d, want 2", n)
	}

	// SPEC-02 §5: repeating the accept reports the current state, no error.
	again, err := e.svc.AcceptRequest(ctx, b.ID, row.ID, "127.0.0.1")
	if err != nil {
		t.Fatalf("re-accept: %v", err)
	}
	if again.Epoch != 1 || again.ConversationID != res.ConversationID || again.Status != RequestAccepted {
		t.Fatalf("re-accept = %+v, want the same state", again)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM friendships WHERE user_low=? AND user_high=?`, a.ID, b.ID); n != 1 {
		t.Fatalf("friendship rows = %d, want 1", n)
	}
}

// A4 / R9-R13: reject and cancel are terminal, and a re-add opens epoch 2.
func TestA4RejectCancelAndEpochBump(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	t.Run("reject imposes a cooldown", func(t *testing.T) {
		a, b := e.newUser(t), e.newUser(t)
		row := e.apply(t, a, b, "")
		if err := e.svc.RejectRequest(ctx, b.ID, row.ID); err != nil {
			t.Fatalf("reject: %v", err)
		}
		if code := codeOf(t, mustCreateErr(e, ctx, CreateRequestInput{
			ApplicantID: a.ID, Source: SourcePhone, Phone: b.Phone,
		})); code != apperrors.StateConflict {
			t.Fatalf("during cooldown: code = %s, want STATE_CONFLICT", code)
		}
		e.clk.Advance(RejectCooldown + time.Minute)
		if _, err := e.svc.CreateRequest(ctx, CreateRequestInput{
			ApplicantID: a.ID, Source: SourcePhone, Phone: b.Phone,
		}); err != nil {
			t.Fatalf("after cooldown: %v", err)
		}
	})

	t.Run("only the recipient may reject", func(t *testing.T) {
		a, b, c := e.newUser(t), e.newUser(t), e.newUser(t)
		row := e.apply(t, a, b, "")
		if code := codeOf(t, e.svc.RejectRequest(ctx, c.ID, row.ID)); code != apperrors.Forbidden {
			t.Fatalf("code = %s, want FORBIDDEN", code)
		}
	})

	t.Run("cancel then delete and re-add", func(t *testing.T) {
		a, b := e.newUser(t), e.newUser(t)
		row := e.apply(t, a, b, "")
		if err := e.svc.CancelRequest(ctx, a.ID, row.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if code := codeOf(t, e.svc.RejectRequest(ctx, b.ID, row.ID)); code != apperrors.StateConflict {
			t.Fatalf("reject after cancel: code = %s", code)
		}
		if _, err := e.svc.CreateRequest(ctx, CreateRequestInput{
			ApplicantID: a.ID, Source: SourcePhone, Phone: b.Phone, VerifyText: "again",
		}); err != nil {
			t.Fatalf("re-apply after cancel: %v", err)
		}
	})

	t.Run("epoch is never reused", func(t *testing.T) {
		a, b := e.newUser(t), e.newUser(t)
		e.friendUp(t, a, b)

		if _, err := e.svc.UpdateFriendSettings(ctx, a.ID, b.ID,
			SettingsPatch{Remark: ptr("bestie")}, "127.0.0.1"); err != nil {
			t.Fatalf("set remark: %v", err)
		}
		if err := e.svc.DeleteFriend(ctx, a.ID, b.ID, "127.0.0.1"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, ok, _ := e.svc.GetActiveFriendship(ctx, a.ID, b.ID); ok {
			t.Fatal("friendship must be gone after delete")
		}
		if n := e.count(t, `SELECT COUNT(*) FROM friendships WHERE user_low=? AND user_high=? AND status='deleted'`,
			a.ID, b.ID); n != 1 {
			t.Fatalf("history rows = %d, want 1", n)
		}
		if code := codeOf(t, e.svc.DeleteFriend(ctx, a.ID, b.ID, "")); code != apperrors.ResourceUnavailable {
			t.Fatalf("double delete: code = %s", code)
		}

		row := e.apply(t, b, a, "second round")
		res, err := e.svc.AcceptRequest(ctx, a.ID, row.ID, "")
		if err != nil {
			t.Fatalf("re-accept: %v", err)
		}
		if res.Epoch != 2 {
			t.Fatalf("epoch = %d, want 2", res.Epoch)
		}
		if n := e.count(t, `SELECT COUNT(*) FROM friendships WHERE user_low=? AND user_high=?`, a.ID, b.ID); n != 2 {
			t.Fatalf("friendship rows = %d, want 2 (history preserved)", n)
		}
		entries, _, err := e.svc.ListFriends(ctx, a.ID, "", 10)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(entries) != 1 || entries[0].Settings.Remark != "" {
			t.Fatalf("settings were not reset on the new epoch: %+v", entries)
		}
	})
}

// A5 / R5, R18: a block forbids new applications and silences messages.
func TestA5BlockSemantics(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b, c, stranger := e.newUser(t), e.newUser(t), e.newUser(t), e.newUser(t)
	e.friendUp(t, a, b)
	e.friendUp(t, a, c)

	if _, err := e.svc.UpdateFriendSettings(ctx, a.ID, c.ID,
		SettingsPatch{MessagePerm: ptr(PermBlocked)}, "127.0.0.1"); err != nil {
		t.Fatalf("block: %v", err)
	}
	if ok, reason, _ := e.svc.CanSendMessage(ctx, a.ID, c.ID); ok || reason != ReasonSenderBlocked {
		t.Fatalf("a→c = (%v,%q)", ok, reason)
	}
	if ok, reason, _ := e.svc.CanSendMessage(ctx, c.ID, a.ID); ok || reason != ReasonBlockedByRecipient {
		t.Fatalf("c→a = (%v,%q)", ok, reason)
	}
	if ok, reason, _ := e.svc.CanSendMessage(ctx, a.ID, b.ID); !ok || reason != "" {
		t.Fatalf("a→b = (%v,%q), a block must not leak to other friends", ok, reason)
	}
	if ok, reason, _ := e.svc.CanSendMessage(ctx, a.ID, 999999999); ok || reason != ReasonNotFriend {
		t.Fatalf("stranger = (%v,%q)", ok, reason)
	}

	if err := e.svc.DeleteFriend(ctx, a.ID, c.ID, ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if code := codeOf(t, mustCreateErr(e, ctx, CreateRequestInput{
		ApplicantID: c.ID, Source: SourcePhone, Phone: a.Phone,
	})); code != apperrors.ResourceUnavailable {
		t.Fatalf("blocked applicant: code = %s, want RESOURCE_UNAVAILABLE", code)
	}
	if code := codeOf(t, mustCreateErr(e, ctx, CreateRequestInput{
		ApplicantID: a.ID, Source: SourcePhone, Phone: c.Phone,
	})); code != apperrors.ResourceUnavailable {
		t.Fatalf("blocker applying: code = %s, want RESOURCE_UNAVAILABLE", code)
	}
	if code := codeOf(t, mustSettingsErr(e.svc.UpdateFriendSettings(ctx, a.ID, stranger.ID,
		SettingsPatch{MessagePerm: ptr(PermBlocked)}, ""))); code != apperrors.ResourceUnavailable {
		t.Fatalf("settings for a stranger: code = %s, want RESOURCE_UNAVAILABLE", code)
	}

	// R17 + R5: the block outlives the friendship, so releasing it must too —
	// otherwise the pair is stuck forever with no re-add path (R13).
	if _, err := e.svc.UpdateFriendSettings(ctx, a.ID, c.ID,
		SettingsPatch{MessagePerm: ptr(PermNormal)}, ""); err != nil {
		t.Fatalf("unblock after delete: %v", err)
	}
	if _, err := e.svc.CreateRequest(ctx, CreateRequestInput{
		ApplicantID: c.ID, Source: SourcePhone, Phone: a.Phone,
	}); err != nil {
		t.Fatalf("apply after unblock: %v", err)
	}
}

// A5b / R18: no_message silences one direction only.
func TestA5NoMessageIsDirectional(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b := e.newUser(t), e.newUser(t)
	e.friendUp(t, a, b)
	if _, err := e.svc.UpdateFriendSettings(ctx, b.ID, a.ID,
		SettingsPatch{MessagePerm: ptr(PermNoMessage)}, ""); err != nil {
		t.Fatalf("set no_message: %v", err)
	}
	if ok, reason, _ := e.svc.CanSendMessage(ctx, a.ID, b.ID); ok || reason != ReasonRecipientNoMessage {
		t.Fatalf("a→b = (%v,%q)", ok, reason)
	}
	if ok, reason, _ := e.svc.CanSendMessage(ctx, b.ID, a.ID); !ok || reason != "" {
		t.Fatalf("b→a = (%v,%q), the sender's own permission must not block them", ok, reason)
	}
}

// A6 / R20-R23: tags are owner-scoped, idempotent and never outlive their owner's right to them.
func TestA6Tags(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b, c, stranger := e.newUser(t), e.newUser(t), e.newUser(t), e.newUser(t)
	e.friendUp(t, a, b)
	e.friendUp(t, a, c)

	tag, err := e.svc.CreateTag(ctx, a.ID, "family")
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if code := codeOf(t, mustTagErr(e.svc.CreateTag(ctx, a.ID, "family"))); code != apperrors.StateConflict {
		t.Fatalf("duplicate name: code = %s", code)
	}
	if _, err := e.svc.CreateTag(ctx, a.ID, "work"); err != nil {
		t.Fatalf("second tag: %v", err)
	}
	if code := codeOf(t, e.svc.RenameTag(ctx, a.ID, tag.ID, "work")); code != apperrors.StateConflict {
		t.Fatalf("rename onto existing: code = %s", code)
	}
	if code := codeOf(t, e.svc.RenameTag(ctx, stranger.ID, tag.ID, "mine")); code != apperrors.ResourceUnavailable {
		t.Fatalf("foreign rename: code = %s", code)
	}

	if err := e.svc.AddTagMembers(ctx, a.ID, tag.ID, []int64{b.ID, c.ID, b.ID}); err != nil {
		t.Fatalf("add members: %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM contact_tag_members WHERE tag_id=?`, tag.ID); n != 2 {
		t.Fatalf("members = %d, want 2", n)
	}
	if code := codeOf(t, e.svc.AddTagMembers(ctx, a.ID, tag.ID, []int64{stranger.ID})); code != apperrors.ResourceUnavailable {
		t.Fatalf("tagging a non-friend: code = %s", code)
	}

	entries, _, err := e.svc.ListFriends(ctx, a.ID, "", 10)
	if err != nil {
		t.Fatalf("list friends: %v", err)
	}
	seen := map[int64]int{}
	for _, en := range entries {
		seen[en.UserID] = len(en.Tags)
		if len(en.Tags) == 1 && (en.Tags[0].ID != tag.ID || en.Tags[0].Name != "family") {
			t.Fatalf("tag ref = %+v", en.Tags[0])
		}
	}
	if seen[b.ID] != 1 || seen[c.ID] != 1 {
		t.Fatalf("tags not attached: %v", seen)
	}

	if err := e.svc.RemoveTagMembers(ctx, a.ID, tag.ID, []int64{b.ID}); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if err := e.svc.RemoveTagMembers(ctx, a.ID, tag.ID, []int64{b.ID}); err != nil {
		t.Fatalf("removing an absent member must be a no-op: %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM contact_tag_members WHERE tag_id=?`, tag.ID); n != 1 {
		t.Fatalf("members = %d, want 1", n)
	}

	if err := e.svc.DeleteTag(ctx, a.ID, tag.ID); err != nil {
		t.Fatalf("delete tag: %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM contact_tag_members WHERE tag_id=?`, tag.ID); n != 0 {
		t.Fatalf("orphan members = %d", n)
	}
	if _, ok, _ := e.svc.GetActiveFriendship(ctx, a.ID, b.ID); !ok {
		t.Fatal("deleting a tag must not touch the friendship")
	}
	if code := codeOf(t, e.svc.DeleteTag(ctx, a.ID, tag.ID)); code != apperrors.ResourceUnavailable {
		t.Fatalf("double delete: code = %s", code)
	}
}

func mustTagErr(_ Tag, err error) error { return err }

func mustSettingsErr(_ Settings, err error) error { return err }

// R2: the worker-driven expiry sweep flips only pending rows past the TTL.
// The sweep is global by design, so this asserts on the rows it created rather
// than on table-wide counts shared with other tests.
func TestR2ExpireStaleRequests(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b := e.newUser(t), e.newUser(t)
	row := e.apply(t, a, b, "")

	e.clk.Advance(RequestTTL - time.Hour)
	if _, err := e.svc.ExpireStaleRequests(ctx); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if got, err := findRequest(ctx, e.db, row.ID); err != nil {
		t.Fatalf("find: %v", err)
	} else if got.Status != RequestPending {
		t.Fatalf("status = %s an hour before the TTL", got.Status)
	}

	e.clk.Advance(2 * time.Hour)
	if _, err := e.svc.ExpireStaleRequests(ctx); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if got, err := findRequest(ctx, e.db, row.ID); err != nil {
		t.Fatalf("find: %v", err)
	} else if got.Status != RequestExpired {
		t.Fatalf("status = %s past the TTL, want expired", got.Status)
	}

	// An expired request no longer occupies the pending slot (R3).
	if _, err := e.svc.CreateRequest(ctx, CreateRequestInput{
		ApplicantID: a.ID, Source: SourcePhone, Phone: b.Phone, VerifyText: "retry",
	}); err != nil {
		t.Fatalf("apply after expiry: %v", err)
	}
}

// R25/R26: the address book pages by ascending id and searches nickname/remark.
func TestR25R26ListAndSearchFriends(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.newUser(t)
	friends := make([]testUser, 0, 3)
	for i := 0; i < 3; i++ {
		f := e.newUserNamed(t, fmt.Sprintf("friend-%d-%d", i, time.Now().UnixNano()))
		e.friendUp(t, owner, f)
		friends = append(friends, f)
	}

	first, next, err := e.svc.ListFriends(ctx, owner.ID, "", 2)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(first) != 2 || next == "" {
		t.Fatalf("page 1 = %d rows, cursor %q", len(first), next)
	}
	if first[0].UserID >= first[1].UserID {
		t.Fatalf("page is not ascending: %d,%d", first[0].UserID, first[1].UserID)
	}
	second, next2, err := e.svc.ListFriends(ctx, owner.ID, next, 2)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(second) != 1 || next2 != "" {
		t.Fatalf("page 2 = %d rows, cursor %q", len(second), next2)
	}
	if second[0].UserID <= first[1].UserID {
		t.Fatal("pages overlap or are out of order")
	}

	if _, err := e.svc.UpdateFriendSettings(ctx, owner.ID, friends[0].ID,
		SettingsPatch{Remark: ptr("Zebra")}, ""); err != nil {
		t.Fatalf("remark: %v", err)
	}
	byRemark, _, err := e.svc.SearchFriends(ctx, owner.ID, "zeb", "", 10)
	if err != nil {
		t.Fatalf("search by remark: %v", err)
	}
	if len(byRemark) != 1 || byRemark[0].UserID != friends[0].ID {
		t.Fatalf("search by remark = %+v", byRemark)
	}
	byNick, _, err := e.svc.SearchFriends(ctx, owner.ID, "FRIEND-1", "", 10)
	if err != nil {
		t.Fatalf("search by nickname: %v", err)
	}
	if len(byNick) != 1 || byNick[0].UserID != friends[1].ID {
		t.Fatalf("search by nickname = %+v", byNick)
	}
	if code := codeOf(t, mustSearchErr(e.svc.SearchFriends(ctx, owner.ID, "", "", 10))); code != apperrors.InvalidArgument {
		t.Fatalf("empty q: code = %s", code)
	}
}

func mustSearchErr(_ []FriendEntry, _ string, err error) error { return err }

// R10: the mailbox is per-side and newest-first.
func TestR10ListRequests(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b, c := e.newUser(t), e.newUser(t), e.newUser(t)
	e.apply(t, a, b, "one")
	e.apply(t, a, c, "two")

	sent, _, err := e.svc.ListRequests(ctx, a.ID, "sent", "", 10)
	if err != nil {
		t.Fatalf("sent: %v", err)
	}
	if len(sent) != 2 || sent[0].Request.VerifyText != "two" {
		t.Fatalf("sent box = %+v", sent)
	}
	received, _, err := e.svc.ListRequests(ctx, b.ID, "received", "", 10)
	if err != nil {
		t.Fatalf("received: %v", err)
	}
	if len(received) != 1 || received[0].Request.ApplicantID != a.ID {
		t.Fatalf("received box = %+v", received)
	}
	if received[0].Applicant.Nickname == "" || received[0].Target.Nickname == "" {
		t.Fatal("both parties must be hydrated")
	}
	if code := codeOf(t, mustListErr(e.svc.ListRequests(ctx, a.ID, "inbox", "", 10))); code != apperrors.InvalidArgument {
		t.Fatalf("bad box: code = %s", code)
	}
}

func mustListErr(_ []RequestEntry, _ string, err error) error { return err }

// R24/R27: the stranger lookup is rate limited and hides private fields.
func TestR24LookupAndRateLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	me, target := e.newUser(t), e.newUser(t)

	res, err := e.svc.Lookup(ctx, me.ID, target.Phone, "")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if res.Identity.UserID != target.ID || res.IsFriend {
		t.Fatalf("lookup = %+v", res)
	}
	e.friendUp(t, me, target)
	if res, err = e.svc.Lookup(ctx, me.ID, "", target.Account); err != nil {
		t.Fatalf("lookup by account: %v", err)
	} else if !res.IsFriend {
		t.Fatal("is_friend must be true for an active friendship")
	}

	var last error
	for i := 0; i < LookupPerMinute+1; i++ {
		_, last = e.svc.Lookup(ctx, me.ID, target.Phone, "")
	}
	if code := codeOf(t, last); code != apperrors.RateLimited {
		t.Fatalf("after %d lookups: code = %s, want RATE_LIMITED", LookupPerMinute+1, code)
	}
}

// ---------------------------------------------------------------------------
// HTTP surface (SPEC-02 §4): the handler + auth middleware + real DB.

type fakeAuthn struct{}

func (fakeAuthn) Authenticate(_ context.Context, token string) (httpx.Principal, error) {
	var id int64
	if _, err := fmt.Sscanf(token, "%d", &id); err != nil || id <= 0 {
		return httpx.Principal{}, apperrors.Unauth("bad test token")
	}
	return httpx.Principal{UserID: id, DeviceID: "test", SessionID: 1}, nil
}

func (e *env) router() http.Handler {
	h := NewHandler(e.svc)
	auth := httpx.RequireAuth(fakeAuthn{})
	mux := http.NewServeMux()
	route := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, auth(fn)) }
	route("GET /api/v1/contacts/lookup", h.Lookup)
	route("GET /api/v1/contacts/friends", h.ListFriends)
	route("GET /api/v1/contacts/friends/search", h.SearchFriends)
	route("POST /api/v1/contacts/requests", h.CreateRequest)
	route("GET /api/v1/contacts/requests", h.ListRequests)
	route("POST /api/v1/contacts/requests/{id}/accept", h.AcceptRequest)
	route("POST /api/v1/contacts/requests/{id}/reject", h.RejectRequest)
	route("POST /api/v1/contacts/requests/{id}/cancel", h.CancelRequest)
	route("DELETE /api/v1/contacts/friends/{friendID}", h.DeleteFriend)
	route("PATCH /api/v1/contacts/friends/{friendID}/settings", h.UpdateFriendSettings)
	route("GET /api/v1/contacts/qrcode", h.QRCode)
	route("POST /api/v1/contacts/tags", h.CreateTag)
	route("GET /api/v1/contacts/tags", h.ListTags)
	route("PATCH /api/v1/contacts/tags/{id}", h.RenameTag)
	route("DELETE /api/v1/contacts/tags/{id}", h.DeleteTag)
	route("POST /api/v1/contacts/tags/{id}/members", h.AddTagMembers)
	route("DELETE /api/v1/contacts/tags/{id}/members", h.RemoveTagMembers)
	return mux
}

type apiResp struct {
	status int
	code   string
	raw    string
	data   json.RawMessage
}

func (e *env) do(t *testing.T, method, path string, asUser int64, body string) apiResp {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+fmt.Sprint(asUser))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.router().ServeHTTP(rec, req)

	var env struct {
		Code string          `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s: bad envelope %q: %v", method, path, rec.Body.String(), err)
	}
	return apiResp{status: rec.Code, code: env.Code, raw: rec.Body.String(), data: env.Data}
}

func TestHTTPLookupHidesPrivateFields(t *testing.T) {
	e := newEnv(t)
	me, target := e.newUser(t), e.newUser(t)

	res := e.do(t, http.MethodGet, "/api/v1/contacts/lookup?phone="+url.QueryEscape(target.Phone), me.ID, "")
	if res.status != http.StatusOK || res.code != "OK" {
		t.Fatalf("status=%d code=%s body=%s", res.status, res.code, res.raw)
	}
	var view map[string]json.RawMessage
	if err := json.Unmarshal(res.data, &view); err != nil {
		t.Fatalf("data: %v", err)
	}
	want := map[string]bool{"user_id": true, "nickname": true, "account_name": true, "is_friend": true}
	for k := range view {
		if !want[k] {
			t.Errorf("lookup leaked field %q", k)
		}
	}
	for k := range want {
		if _, ok := view[k]; !ok {
			t.Errorf("lookup is missing %q", k)
		}
	}
	for _, forbidden := range []string{"phone", "region", "signature", "password"} {
		if strings.Contains(res.raw, `"`+forbidden+`"`) {
			t.Errorf("response body contains %q: %s", forbidden, res.raw)
		}
	}
}

func TestHTTPRequestLifecycle(t *testing.T) {
	e := newEnv(t)
	a, b := e.newUser(t), e.newUser(t)

	created := e.do(t, http.MethodPost, "/api/v1/contacts/requests", a.ID,
		fmt.Sprintf(`{"source":"account_name","account_name":%q,"verify_text":"hi"}`, b.Account))
	if created.status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", created.status, created.raw)
	}
	var made struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(created.data, &made); err != nil {
		t.Fatalf("create data: %v", err)
	}
	if made.Status != RequestPending || made.ID == "" {
		t.Fatalf("create = %+v", made)
	}

	inbox := e.do(t, http.MethodGet, "/api/v1/contacts/requests?box=received", b.ID, "")
	if inbox.status != http.StatusOK || !strings.Contains(inbox.raw, `"has_more":false`) {
		t.Fatalf("inbox: status=%d body=%s", inbox.status, inbox.raw)
	}

	accepted := e.do(t, http.MethodPost, "/api/v1/contacts/requests/"+made.ID+"/accept", b.ID, "")
	if accepted.status != http.StatusOK || !strings.Contains(accepted.raw, `"epoch":"1"`) {
		t.Fatalf("accept: status=%d body=%s", accepted.status, accepted.raw)
	}

	list := e.do(t, http.MethodGet, "/api/v1/contacts/friends", a.ID, "")
	if list.status != http.StatusOK || !strings.Contains(list.raw, `"account_name":`+strconv.Quote(b.Account)) {
		t.Fatalf("friends: status=%d body=%s", list.status, list.raw)
	}

	patched := e.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/contacts/friends/%d/settings", b.ID), a.ID,
		`{"remark":"buddy","message_perm":"blocked"}`)
	if patched.status != http.StatusOK || !strings.Contains(patched.raw, `"remark":"buddy"`) {
		t.Fatalf("patch: status=%d body=%s", patched.status, patched.raw)
	}

	if code := e.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/contacts/friends/%d/settings", b.ID), a.ID,
		`{"message_perm":"mute"}`); code.code != "INVALID_ARGUMENT" {
		t.Fatalf("bad perm: code=%s", code.code)
	}
	if res := e.do(t, http.MethodDelete, fmt.Sprintf("/api/v1/contacts/friends/%d", b.ID), a.ID, ""); res.status != http.StatusOK {
		t.Fatalf("delete: status=%d body=%s", res.status, res.raw)
	}
	if res := e.do(t, http.MethodGet, "/api/v1/contacts/friends", a.ID, ""); !strings.Contains(res.raw, `"items":[]`) {
		t.Fatalf("empty book: %s", res.raw)
	}
}

func TestHTTPRequiresAuth(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts/friends", nil)
	rec := httptest.NewRecorder()
	e.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHTTPTagsAndQRCode(t *testing.T) {
	e := newEnv(t)
	a, b := e.newUser(t), e.newUser(t)
	e.friendUp(t, a, b)

	qr := e.do(t, http.MethodGet, "/api/v1/contacts/qrcode", a.ID, "")
	if qr.status != http.StatusOK || !strings.Contains(qr.raw, `"qrcode_token":`) {
		t.Fatalf("qrcode: status=%d body=%s", qr.status, qr.raw)
	}
	var token struct {
		QRCodeToken string `json:"qrcode_token"`
	}
	if err := json.Unmarshal(qr.data, &token); err != nil {
		t.Fatalf("qrcode data: %v", err)
	}
	c := e.newUser(t)
	applied := e.do(t, http.MethodPost, "/api/v1/contacts/requests", c.ID,
		fmt.Sprintf(`{"source":"qrcode","qrcode_token":%s}`, strconv.Quote(token.QRCodeToken)))
	if applied.status != http.StatusCreated {
		t.Fatalf("qrcode apply: status=%d body=%s", applied.status, applied.raw)
	}

	tag := e.do(t, http.MethodPost, "/api/v1/contacts/tags", a.ID, `{"name":"close"}`)
	if tag.status != http.StatusCreated {
		t.Fatalf("create tag: status=%d body=%s", tag.status, tag.raw)
	}
	var made struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(tag.data, &made); err != nil {
		t.Fatalf("tag data: %v", err)
	}
	added := e.do(t, http.MethodPost, "/api/v1/contacts/tags/"+made.ID+"/members", a.ID,
		fmt.Sprintf(`{"friend_ids":[%q]}`, strconv.FormatInt(b.ID, 10)))
	if added.status != http.StatusOK {
		t.Fatalf("add members: status=%d body=%s", added.status, added.raw)
	}
	if res := e.do(t, http.MethodPost, "/api/v1/contacts/tags/"+made.ID+"/members", a.ID,
		`{"friend_ids":[123]}`); res.code != "INVALID_ARGUMENT" {
		t.Fatalf("bare number id: code=%s body=%s", res.code, res.raw)
	}
	if res := e.do(t, http.MethodGet, "/api/v1/contacts/friends", a.ID, ""); !strings.Contains(res.raw, `"name":"close"`) {
		t.Fatalf("tag not attached: %s", res.raw)
	}
	if res := e.do(t, http.MethodDelete, "/api/v1/contacts/tags/"+made.ID, a.ID, ""); res.status != http.StatusOK {
		t.Fatalf("delete tag: status=%d body=%s", res.status, res.raw)
	}
}

// R14: ListFriendIDs is the paging primitive the moment module snapshots.
func TestR14ListFriendIDsPages(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.newUser(t)
	ids := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		f := e.newUser(t)
		e.friendUp(t, owner, f)
		ids = append(ids, f.ID)
	}
	var got []int64
	cursor := int64(0)
	for {
		page, err := e.svc.ListFriendIDs(ctx, owner.ID, cursor, 2)
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, page...)
		cursor = page[len(page)-1]
	}
	if len(got) != 3 {
		t.Fatalf("ids = %v, want 3", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("ids not strictly ascending: %v", got)
		}
	}
}

func ptr[T any](v T) *T { return &v }
