//go:build integration

package moment

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/wechat/internal/platform/clock"
	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// fakeContact is a stub contact port. Each test can override behavior by
// mutating the maps.
type fakeContact struct {
	friends    map[int64][]FriendEpoch
	epochOK    map[string]bool // "author|viewer|epoch" -> current
	permBlock  map[int64]bool  // blocked author->viewer
	permHidden map[int64]bool
	permNoMom  map[int64]bool
}

func newFakeContact() *fakeContact {
	return &fakeContact{
		friends:    map[int64][]FriendEpoch{},
		epochOK:    map[string]bool{},
		permBlock:  map[int64]bool{},
		permHidden: map[int64]bool{},
		permNoMom:  map[int64]bool{},
	}
}

func (f *fakeContact) ActiveFriendsWithEpoch(_ context.Context, authorID int64) ([]FriendEpoch, error) {
	return f.friends[authorID], nil
}

func (f *fakeContact) IsFriendCurrentEpoch(_ context.Context, authorID, viewerID, epoch int64) (bool, error) {
	return f.epochOK[fmt.Sprintf("%d|%d|%d", authorID, viewerID, epoch)], nil
}

func (f *fakeContact) MomentPerm(_ context.Context, authorID, viewerID int64) (bool, bool, bool, error) {
	return f.permHidden[authorID*100000+viewerID], f.permBlock[authorID*100000+viewerID], f.permNoMom[authorID*100000+viewerID], nil
}

func (f *fakeContact) ExpandTag(_ context.Context, ownerID, tagID int64) ([]FriendEpoch, error) {
	return f.friends[ownerID], nil
}

type fakeMedia struct{}

func (fakeMedia) AssertReady(_ context.Context, _, _ int64, _ string) error { return nil }

type env struct {
	db    *sql.DB
	svc   *Service
	fake  *fakeContact
	clk   *clock.Fake
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
	fake := newFakeContact()
	svc := New(db, fake, fakeMedia{}, nil, clk.Now)
	return &env{db: db, svc: svc, fake: fake, clk: clk}
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

// makeFriend sets up friendship between a and b at epoch 1.
func (e *env) makeFriend(a, b int64) {
	e.fake.friends[a] = append(e.fake.friends[a], FriendEpoch{UserID: b, Epoch: 1})
	e.fake.friends[b] = append(e.fake.friends[b], FriendEpoch{UserID: a, Epoch: 1})
	e.fake.epochOK[fmt.Sprintf("%d|%d|%d", a, b, 1)] = true
	e.fake.epochOK[fmt.Sprintf("%d|%d|%d", b, a, 1)] = true
}

// T1: publish moment as all_friends, friend sees it in feed.
func TestT1_FriendSeesFeed(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	viewer := e.newUser(t)
	e.makeFriend(author, viewer)

	mid, err := e.svc.Publish(context.Background(), &PublishInput{
		AuthorID:   author,
		Content:    "hello world",
		Visibility: VisAllFriends,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	feed, err := e.svc.Feed(context.Background(), viewer, 0, 20)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	if len(feed) != 1 || feed[0].ID != mid {
		t.Fatalf("want 1 moment %d, got %+v", mid, feed)
	}
}

// T2: non-friend does NOT see moment.
func TestT2_NonFriendBlocked(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	stranger := e.newUser(t)
	e.makeFriend(author, stranger)
	// Drop stranger from author's snapshot.
	e.fake.friends[author] = nil
	delete(e.fake.epochOK, fmt.Sprintf("%d|%d|1", author, stranger))

	if _, err := e.svc.Publish(context.Background(), &PublishInput{
		AuthorID:   author,
		Content:    "secret",
		Visibility: VisAllFriends,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	feed, err := e.svc.Feed(context.Background(), stranger, 0, 20)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	if len(feed) != 0 {
		t.Fatalf("stranger should not see moment, got %+v", feed)
	}
}

// T3: empty moment rejected.
func TestT3_EmptyMomentRejected(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	_, err := e.svc.Publish(context.Background(), &PublishInput{AuthorID: author, Content: "  ", Visibility: VisAllFriends})
	if err == nil {
		t.Fatal("expected empty moment rejection")
	}
}

// T4: >9 assets rejected.
func TestT4_TooManyAssets(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	ids := make([]int64, 10)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	_, err := e.svc.Publish(context.Background(), &PublishInput{AuthorID: author, Content: "x", AssetIDs: ids, Visibility: VisAllFriends})
	if err == nil {
		t.Fatal("expected asset count rejection")
	}
}

// T5: self visibility only visible to self.
func TestT5_SelfOnly(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	friend := e.newUser(t)
	e.makeFriend(author, friend)
	mid, err := e.svc.Publish(context.Background(), &PublishInput{AuthorID: author, Content: "me only", Visibility: VisSelf})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := e.svc.Get(context.Background(), friend, mid); err == nil {
		t.Fatal("friend should not see self-only moment")
	}
	if _, err := e.svc.Get(context.Background(), author, mid); err != nil {
		t.Fatalf("author should see own moment: %v", err)
	}
}

// T6: like and unlike.
func TestT6_LikeUnlike(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	friend := e.newUser(t)
	e.makeFriend(author, friend)
	mid, _ := e.svc.Publish(context.Background(), &PublishInput{AuthorID: author, Content: "x", Visibility: VisAllFriends})
	if err := e.svc.Like(context.Background(), mid, friend); err != nil {
		t.Fatalf("like: %v", err)
	}
	likes, err := e.svc.Likes(context.Background(), friend, mid)
	if err != nil || len(likes) != 1 || likes[0] != friend {
		t.Fatalf("likes = %v err=%v", likes, err)
	}
	if err := e.svc.Unlike(context.Background(), mid, friend); err != nil {
		t.Fatalf("unlike: %v", err)
	}
	likes, _ = e.svc.Likes(context.Background(), friend, mid)
	if len(likes) != 0 {
		t.Fatalf("likes after unlike = %v", likes)
	}
}

// T7: comment with reply.
func TestT7_CommentReply(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	friend := e.newUser(t)
	e.makeFriend(author, friend)
	mid, _ := e.svc.Publish(context.Background(), &PublishInput{AuthorID: author, Content: "x", Visibility: VisAllFriends})
	c, err := e.svc.AddComment(context.Background(), mid, friend, nil, "nice")
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	reply, err := e.svc.AddComment(context.Background(), mid, author, &c.ID, "thanks")
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if reply.ReplyTo == nil || *reply.ReplyTo != c.ID {
		t.Fatalf("reply.ReplyTo = %v", reply.ReplyTo)
	}
	list, err := e.svc.Comments(context.Background(), friend, mid)
	if err != nil || len(list) != 2 {
		t.Fatalf("comments = %v err=%v", list, err)
	}
}

// T8: empty comment rejected.
func TestT8_EmptyComment(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	friend := e.newUser(t)
	e.makeFriend(author, friend)
	mid, _ := e.svc.Publish(context.Background(), &PublishInput{AuthorID: author, Content: "x", Visibility: VisAllFriends})
	if _, err := e.svc.AddComment(context.Background(), mid, friend, nil, "   "); err == nil {
		t.Fatal("expected empty comment rejection")
	}
}

// T9: delete comment as commenter and as author.
func TestT9_DeleteComment(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	friend := e.newUser(t)
	stranger := e.newUser(t)
	e.makeFriend(author, friend)
	e.makeFriend(author, stranger)
	mid, _ := e.svc.Publish(context.Background(), &PublishInput{AuthorID: author, Content: "x", Visibility: VisAllFriends})
	c, _ := e.svc.AddComment(context.Background(), mid, friend, nil, "hi")
	if err := e.svc.DeleteComment(context.Background(), c.ID, stranger); err == nil {
		t.Fatal("stranger should not delete")
	}
	if err := e.svc.DeleteComment(context.Background(), c.ID, friend); err != nil {
		t.Fatalf("commenter delete: %v", err)
	}
}

// T10: blocked user cannot see moment.
func TestT10_BlockedUser(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	friend := e.newUser(t)
	e.makeFriend(author, friend)
	mid, _ := e.svc.Publish(context.Background(), &PublishInput{AuthorID: author, Content: "x", Visibility: VisAllFriends})
	e.fake.permBlock[author*100000+friend] = true
	if _, err := e.svc.Get(context.Background(), friend, mid); err == nil {
		t.Fatal("blocked user should not see moment")
	}
}

// T11: epoch invalidated (unfriend) revokes visibility.
func TestT11_EpochRevoked(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	friend := e.newUser(t)
	e.makeFriend(author, friend)
	mid, _ := e.svc.Publish(context.Background(), &PublishInput{AuthorID: author, Content: "x", Visibility: VisAllFriends})
	// Friendship ended: epoch 1 no longer current for viewer.
	delete(e.fake.epochOK, fmt.Sprintf("%d|%d|1", author, friend))
	if _, err := e.svc.Get(context.Background(), friend, mid); err == nil {
		t.Fatal("viewer should lose visibility after epoch change")
	}
}

// T12: soft-deleted moment not visible.
func TestT12_DeleteMoment(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	friend := e.newUser(t)
	e.makeFriend(author, friend)
	mid, _ := e.svc.Publish(context.Background(), &PublishInput{AuthorID: author, Content: "x", Visibility: VisAllFriends})
	if err := e.svc.Delete(context.Background(), mid, author); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := e.svc.Get(context.Background(), author, mid); err == nil {
		t.Fatal("deleted moment should not be visible")
	}
}

// T13: notifications are emitted on like/comment.
func TestT13_Notifications(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	friend := e.newUser(t)
	e.makeFriend(author, friend)
	mid, _ := e.svc.Publish(context.Background(), &PublishInput{AuthorID: author, Content: "x", Visibility: VisAllFriends})
	_ = e.svc.Like(context.Background(), mid, friend)
	_, _ = e.svc.AddComment(context.Background(), mid, friend, nil, "hi")
	items, err := e.svc.Notifications(context.Background(), author, 50)
	if err != nil {
		t.Fatalf("notifications: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 notifications, got %d", len(items))
	}
	if err := e.svc.MarkNotificationsRead(context.Background(), author, nil, true); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	items, _ = e.svc.Notifications(context.Background(), author, 50)
	for _, n := range items {
		if n.ReadAt == nil {
			t.Fatal("all should be read")
		}
	}
}

// T14: scheduled moment not immediately visible, but becomes visible after publish.
func TestT14_SchedulePublish(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	friend := e.newUser(t)
	e.makeFriend(author, friend)
	runAt := e.clk.Now().Add(time.Hour)
	sid, err := e.svc.CreateSchedule(context.Background(), &PublishInput{
		AuthorID:   author,
		Content:    "scheduled",
		Visibility: VisAllFriends,
	}, runAt)
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	feed, _ := e.svc.Feed(context.Background(), friend, 0, 20)
	if len(feed) != 0 {
		t.Fatalf("scheduled moment should not be visible yet, got %+v", feed)
	}
	// Advance past run_at.
	e.clk.Advance(2 * time.Hour)
	_, err = e.svc.PublishDueSchedules(context.Background(), "test-worker")
	if err != nil {
		t.Fatalf("publish due: %v", err)
	}
	feed, _ = e.svc.Feed(context.Background(), friend, 0, 20)
	found := false
	for _, m := range feed {
		if m.Content == "scheduled" {
			found = true
		}
	}
	if !found {
		t.Fatalf("scheduled moment not in feed: %+v", feed)
	}
	_ = sid
}

// T15: cancel scheduled moment.
func TestT15_CancelSchedule(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	runAt := e.clk.Now().Add(time.Hour)
	sid, err := e.svc.CreateSchedule(context.Background(), &PublishInput{
		AuthorID:   author,
		Content:    "x",
		Visibility: VisSelf,
	}, runAt)
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	if err := e.svc.CancelSchedule(context.Background(), sid, author); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	e.clk.Advance(2 * time.Hour)
	n, _ := e.svc.PublishDueSchedules(context.Background(), "w")
	if n != 0 {
		t.Fatalf("canceled schedule should not publish, got %d", n)
	}
}

// T16: run_at in past rejected.
func TestT16_RunAtPastRejected(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	runAt := e.clk.Now().Add(-time.Hour)
	if _, err := e.svc.CreateSchedule(context.Background(), &PublishInput{
		AuthorID:   author,
		Content:    "x",
		Visibility: VisSelf,
	}, runAt); err == nil {
		t.Fatal("past run_at must be rejected")
	}
}

// T17: >30 days rejected.
func TestT17_RunAtTooFar(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	runAt := e.clk.Now().Add(31 * 24 * time.Hour)
	if _, err := e.svc.CreateSchedule(context.Background(), &PublishInput{
		AuthorID:   author,
		Content:    "x",
		Visibility: VisSelf,
	}, runAt); err == nil {
		t.Fatal(">30d run_at must be rejected")
	}
}

// T18: selected visibility only shows to allowed users.
func TestT18_SelectedVisibility(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	a := e.newUser(t)
	b := e.newUser(t)
	e.makeFriend(author, a)
	e.makeFriend(author, b)
	mid, _ := e.svc.Publish(context.Background(), &PublishInput{
		AuthorID:       author,
		Content:        "only a",
		Visibility:     VisSelected,
		AllowedUserIDs: []int64{a},
	})
	if _, err := e.svc.Get(context.Background(), a, mid); err != nil {
		t.Fatalf("allowed user should see: %v", err)
	}
	if _, err := e.svc.Get(context.Background(), b, mid); err == nil {
		t.Fatal("non-allowed friend should not see")
	}
}

// T19: exclude visibility hides excluded friends.
func TestT19_ExcludeVisibility(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	a := e.newUser(t)
	b := e.newUser(t)
	e.makeFriend(author, a)
	e.makeFriend(author, b)
	mid, _ := e.svc.Publish(context.Background(), &PublishInput{
		AuthorID:        author,
		Content:         "hide from b",
		Visibility:      VisExclude,
		ExcludedUserIDs: []int64{b},
	})
	if _, err := e.svc.Get(context.Background(), a, mid); err != nil {
		t.Fatalf("non-excluded friend should see: %v", err)
	}
	if _, err := e.svc.Get(context.Background(), b, mid); err == nil {
		t.Fatal("excluded friend should not see")
	}
}

// T20: author can't like own moment notification-less; no error.
func TestT20_AuthorLikeNoNotification(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	mid, _ := e.svc.Publish(context.Background(), &PublishInput{AuthorID: author, Content: "x", Visibility: VisSelf})
	if err := e.svc.Like(context.Background(), mid, author); err != nil {
		t.Fatalf("author like: %v", err)
	}
	items, _ := e.svc.Notifications(context.Background(), author, 10)
	if len(items) != 0 {
		t.Fatalf("self-like should not notify, got %d", len(items))
	}
}

// T21: likes disabled.
func TestT21_LikesDisabled(t *testing.T) {
	e := newEnv(t)
	author := e.newUser(t)
	friend := e.newUser(t)
	e.makeFriend(author, friend)
	f := false
	mid, _ := e.svc.Publish(context.Background(), &PublishInput{
		AuthorID: author, Content: "x", Visibility: VisAllFriends, AllowLikes: &f,
	})
	if err := e.svc.Like(context.Background(), mid, friend); err == nil {
		t.Fatal("likes should be disabled")
	} else if appErr := apperrors.AsApp(err); appErr == nil || appErr.Code != apperrors.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}
