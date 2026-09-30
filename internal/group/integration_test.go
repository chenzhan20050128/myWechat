//go:build integration

package group

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/wechat/internal/conversation"
	"github.com/example/wechat/internal/platform/clock"
	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

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
	svc := New(db, convs, nil, nil, clk.Now)
	return &env{db: db, svc: svc, clk: clk, conv: convs}
}

var seq atomic.Int64

func (e *env) newUser(t *testing.T) int64 {
	t.Helper()
	phone := fmt.Sprintf("+86%08d%04d", time.Now().UnixNano()%100000000, seq.Add(1))
	account := "g" + phone[3:]
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

func (e *env) newGroup(t *testing.T, owner int64) int64 {
	t.Helper()
	id, err := e.svc.CreateGroup(context.Background(), owner, CreateInput{Name: "g" + fmt.Sprintf("%d", seq.Add(1))})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM group_members WHERE group_id = ?`, id)
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM group_mutes WHERE group_id = ?`, id)
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM group_events WHERE group_id = ?`, id)
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM group_invite_codes WHERE group_id = ?`, id)
		_, _ = e.db.ExecContext(context.Background(), `DELETE FROM group_todos WHERE group_id = ?`, id)
		_, _ = e.db.ExecContext(context.Background(), "DELETE FROM `groups` WHERE id = ?", id)
	})
	return id
}

func TestT1CreateGroup(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	gid, err := e.svc.CreateGroup(context.Background(), owner, CreateInput{Name: "t1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	g, role, err := e.svc.GetGroup(context.Background(), gid, owner)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if role != RoleOwner {
		t.Fatalf("want owner role, got %s", role)
	}
	if g.MemberCount != 1 {
		t.Fatalf("want member_count=1, got %d", g.MemberCount)
	}
	if g.Status != StatusActive {
		t.Fatalf("want active, got %s", g.Status)
	}
}

func TestT2CreateGroupInvalidName(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	_, err := e.svc.CreateGroup(context.Background(), owner, CreateInput{Name: ""})
	if err == nil {
		t.Fatal("want error for empty name")
	}
}

func TestT3InviteAndMembers(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	member := e.newUser(t)
	gid := e.newGroup(t, owner)
	added, skipped, err := e.svc.Invite(context.Background(), gid, owner, []int64{member})
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if len(added) != 1 || added[0] != member {
		t.Fatalf("want added=[%d], got %v/%v", member, added, skipped)
	}
	members, err := e.svc.ListMembers(context.Background(), gid, member)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("want 2 members, got %d", len(members))
	}
}

func TestT4InviteIdempotent(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	member := e.newUser(t)
	gid := e.newGroup(t, owner)
	if _, _, err := e.svc.Invite(context.Background(), gid, owner, []int64{member}); err != nil {
		t.Fatal(err)
	}
	added, skipped, err := e.svc.Invite(context.Background(), gid, owner, []int64{member})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 || len(skipped) != 1 {
		t.Fatalf("want 0 added, 1 skipped, got added=%v skipped=%v", added, skipped)
	}
}

func TestT5JoinByInviteCode(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	stranger := e.newUser(t)
	gid := e.newGroup(t, owner)
	code, _, err := e.svc.NewInviteCode(context.Background(), gid, owner)
	if err != nil {
		t.Fatalf("new code: %v", err)
	}
	got, err := e.svc.Join(context.Background(), stranger, code)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if got != gid {
		t.Fatalf("want group %d, got %d", gid, got)
	}
}

func TestT6JoinExpiredCode(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	stranger := e.newUser(t)
	gid := e.newGroup(t, owner)
	code, _, err := e.svc.NewInviteCode(context.Background(), gid, owner)
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(25 * time.Hour)
	_, err = e.svc.Join(context.Background(), stranger, code)
	if err == nil {
		t.Fatal("want error for expired code")
	}
	if ae, ok := err.(*apperrors.AppError); !ok || ae.Code != apperrors.ResourceUnavailable {
		t.Fatalf("want RESOURCE_UNAVAILABLE, got %T %v", err, err)
	}
}

func TestT7KickAndBanWindow(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	member := e.newUser(t)
	gid := e.newGroup(t, owner)
	if _, _, err := e.svc.Invite(context.Background(), gid, owner, []int64{member}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Kick(context.Background(), gid, owner, member); err != nil {
		t.Fatalf("kick: %v", err)
	}
	code, _, err := e.svc.NewInviteCode(context.Background(), gid, owner)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Join(context.Background(), member, code)
	if err == nil {
		t.Fatal("want ban-window error")
	}
	if ae, ok := err.(*apperrors.AppError); !ok || ae.Code != apperrors.StateConflict {
		t.Fatalf("want STATE_CONFLICT, got %T %v", err, err)
	}
}

func TestT8QuitOwnerBlocked(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	gid := e.newGroup(t, owner)
	err := e.svc.Quit(context.Background(), gid, owner)
	if err == nil {
		t.Fatal("want error when owner quits")
	}
	if ae, ok := err.(*apperrors.AppError); !ok || ae.Code != apperrors.StateConflict {
		t.Fatalf("want STATE_CONFLICT, got %T %v", err, err)
	}
}

func TestT9QuitAndRejoinAllowed(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	member := e.newUser(t)
	gid := e.newGroup(t, owner)
	if _, _, err := e.svc.Invite(context.Background(), gid, owner, []int64{member}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Quit(context.Background(), gid, member); err != nil {
		t.Fatalf("quit: %v", err)
	}
	code, _, err := e.svc.NewInviteCode(context.Background(), gid, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Join(context.Background(), member, code); err != nil {
		t.Fatalf("rejoin after quit should be allowed: %v", err)
	}
}

func TestT10Transfer(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	newOwner := e.newUser(t)
	gid := e.newGroup(t, owner)
	if _, _, err := e.svc.Invite(context.Background(), gid, owner, []int64{newOwner}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Transfer(context.Background(), gid, owner, newOwner); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	g, role, err := e.svc.GetGroup(context.Background(), gid, newOwner)
	if err != nil {
		t.Fatal(err)
	}
	if role != RoleOwner {
		t.Fatalf("want new owner role, got %s", role)
	}
	if g.OwnerID != newOwner {
		t.Fatalf("want owner_id=%d, got %d", newOwner, g.OwnerID)
	}
}

func TestT11Dissolve(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	gid := e.newGroup(t, owner)
	if err := e.svc.Dissolve(context.Background(), gid, owner); err != nil {
		t.Fatalf("dissolve: %v", err)
	}
	g, _, err := e.svc.GetGroup(context.Background(), gid, owner)
	if err == nil {
		t.Fatalf("dissolved group should be inaccessible, got %+v", g)
	}
}

func TestT12PromoteAndDemoteAdmin(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	member := e.newUser(t)
	gid := e.newGroup(t, owner)
	if _, _, err := e.svc.Invite(context.Background(), gid, owner, []int64{member}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PromoteAdmin(context.Background(), gid, owner, member); err != nil {
		t.Fatalf("promote: %v", err)
	}
	m, role, err := activeMember(context.Background(), e.db, gid, member)
	_ = m
	if err != nil {
		t.Fatal(err)
	}
	if role != RoleAdmin {
		t.Fatalf("want admin, got %s", role)
	}
	if err := e.svc.DemoteAdmin(context.Background(), gid, owner, member); err != nil {
		t.Fatalf("demote: %v", err)
	}
}

func TestT13AdminCannotKickAdmin(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	a := e.newUser(t)
	b := e.newUser(t)
	gid := e.newGroup(t, owner)
	if _, _, err := e.svc.Invite(context.Background(), gid, owner, []int64{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PromoteAdmin(context.Background(), gid, owner, a); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PromoteAdmin(context.Background(), gid, owner, b); err != nil {
		t.Fatal(err)
	}
	err := e.svc.Kick(context.Background(), gid, a, b)
	if err == nil {
		t.Fatal("admin cannot kick admin")
	}
	if ae, ok := err.(*apperrors.AppError); !ok || ae.Code != apperrors.Forbidden {
		t.Fatalf("want FORBIDDEN, got %T %v", err, err)
	}
}

func TestT14SetMuteAndCanSend(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	member := e.newUser(t)
	gid := e.newGroup(t, owner)
	if _, _, err := e.svc.Invite(context.Background(), gid, owner, []int64{member}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetMute(context.Background(), gid, owner, member, "10m"); err != nil {
		t.Fatalf("mute: %v", err)
	}
	if err := e.svc.CanSend(context.Background(), gid, member); err == nil {
		t.Fatal("muted member cannot send")
	}
	e.clk.Advance(11 * time.Minute)
	if err := e.svc.CanSend(context.Background(), gid, member); err != nil {
		t.Fatalf("after expiry member can send: %v", err)
	}
}

func TestT15AdminCannotMuteAdmin(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	a := e.newUser(t)
	b := e.newUser(t)
	gid := e.newGroup(t, owner)
	if _, _, err := e.svc.Invite(context.Background(), gid, owner, []int64{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PromoteAdmin(context.Background(), gid, owner, a); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PromoteAdmin(context.Background(), gid, owner, b); err != nil {
		t.Fatal(err)
	}
	err := e.svc.SetMute(context.Background(), gid, a, b, "10m")
	if err == nil {
		t.Fatal("admin cannot mute admin")
	}
}

func TestT16Unmute(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	member := e.newUser(t)
	gid := e.newGroup(t, owner)
	if _, _, err := e.svc.Invite(context.Background(), gid, owner, []int64{member}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetMute(context.Background(), gid, owner, member, "forever"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Unmute(context.Background(), gid, owner, member); err != nil {
		t.Fatalf("unmute: %v", err)
	}
	if err := e.svc.CanSend(context.Background(), gid, member); err != nil {
		t.Fatalf("after unmute member can send: %v", err)
	}
}

func TestT17NonMemberForbidden(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	stranger := e.newUser(t)
	gid := e.newGroup(t, owner)
	_, _, err := e.svc.GetGroup(context.Background(), gid, stranger)
	if err == nil {
		t.Fatal("stranger should not see group")
	}
	if ae, ok := err.(*apperrors.AppError); !ok || ae.Code != apperrors.Forbidden {
		t.Fatalf("want FORBIDDEN, got %T %v", err, err)
	}
}

func TestT18CreateTodoAndAssigneeCompletion(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	assignee := e.newUser(t)
	gid := e.newGroup(t, owner)
	if _, _, err := e.svc.Invite(context.Background(), gid, owner, []int64{assignee}); err != nil {
		t.Fatal(err)
	}
	tid, err := e.svc.CreateTodo(context.Background(), gid, owner, CreateTodoInput{
		Title:       "pickup milk",
		AssigneeIDs: []int64{assignee},
	})
	if err != nil {
		t.Fatalf("create todo: %v", err)
	}
	view, err := e.svc.GetTodo(context.Background(), tid, assignee)
	if err != nil {
		t.Fatalf("get todo: %v", err)
	}
	if view.Status != TodoActive {
		t.Fatalf("want active, got %s", view.Status)
	}
	if err := e.svc.CompleteTodo(context.Background(), tid, assignee); err != nil {
		t.Fatalf("complete: %v", err)
	}
	view, err = e.svc.GetTodo(context.Background(), tid, assignee)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != TodoCompleted {
		t.Fatalf("want completed, got %s", view.Status)
	}
}

func TestT19TodoAutoCancelWhenAllWaived(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	a := e.newUser(t)
	gid := e.newGroup(t, owner)
	if _, _, err := e.svc.Invite(context.Background(), gid, owner, []int64{a}); err != nil {
		t.Fatal(err)
	}
	tid, err := e.svc.CreateTodo(context.Background(), gid, owner, CreateTodoInput{
		Title: "task", AssigneeIDs: []int64{a},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Kick(context.Background(), gid, owner, a); err != nil {
		t.Fatal(err)
	}
	view, err := e.svc.GetTodo(context.Background(), tid, owner)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != TodoCancelled {
		t.Fatalf("want auto-cancelled, got %s", view.Status)
	}
}

func TestT20TodoVisibilitySnapshot(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	a := e.newUser(t)
	b := e.newUser(t)
	gid := e.newGroup(t, owner)
	if _, _, err := e.svc.Invite(context.Background(), gid, owner, []int64{a}); err != nil {
		t.Fatal(err)
	}
	tid, err := e.svc.CreateTodo(context.Background(), gid, owner, CreateTodoInput{
		Title: "task", AssigneeIDs: []int64{a},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Invite(context.Background(), gid, owner, []int64{b}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetTodo(context.Background(), tid, b); err == nil {
		t.Fatal("b joined after todo creation; should not see it")
	}
}

func TestT21CancelTodo(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	gid := e.newGroup(t, owner)
	tid, err := e.svc.CreateTodo(context.Background(), gid, owner, CreateTodoInput{
		Title: "task", AssigneeIDs: []int64{owner},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CancelTodo(context.Background(), tid, owner); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := e.svc.GetTodo(context.Background(), tid, owner); err == nil {
		t.Fatal("cancelled todo should be hidden")
	}
}

func TestT22ListMyGroups(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	gid := e.newGroup(t, owner)
	groups, err := e.svc.ListMyGroups(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range groups {
		if g.GroupID == gid {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected group %d in %+v", gid, groups)
	}
}

func TestConcurrentJoinsHonorInviteUseLimit(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	gid := e.newGroup(t, owner)
	code, _, err := e.svc.NewInviteCode(context.Background(), gid, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.ExecContext(context.Background(),
		`UPDATE group_invite_codes SET max_uses = 2 WHERE group_id = ? AND code = ?`, gid, code); err != nil {
		t.Fatal(err)
	}

	const candidates = 8
	ids := make([]int64, candidates)
	errs := make([]error, candidates)
	var wg sync.WaitGroup
	for i := 0; i < candidates; i++ {
		ids[i] = e.newUser(t)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.svc.Join(context.Background(), ids[i], code)
		}(i)
	}
	wg.Wait()

	successes := 0
	for i, err := range errs {
		if err == nil {
			successes++
			continue
		}
		if ae := apperrors.AsApp(err); ae == nil || ae.Code != apperrors.ResourceUnavailable {
			t.Fatalf("candidate %d: unexpected error %v", i, err)
		}
	}
	if successes != 2 {
		t.Fatalf("successful joins = %d, want 2", successes)
	}
	var members, uses int
	if err := e.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM group_members WHERE group_id = ? AND left_at IS NULL`, gid).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if err := e.db.QueryRowContext(context.Background(),
		`SELECT use_count FROM group_invite_codes WHERE group_id = ? AND code = ?`, gid, code).Scan(&uses); err != nil {
		t.Fatal(err)
	}
	if members != 3 || uses != 2 {
		t.Fatalf("members=%d invite uses=%d, want 3/2", members, uses)
	}
}
