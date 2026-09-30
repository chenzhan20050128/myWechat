//go:build integration

package auth

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"fmt"
	"math/big"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/example/wechat/internal/audit"
	"github.com/example/wechat/internal/conversation"
	"github.com/example/wechat/internal/device"
	"github.com/example/wechat/internal/platform/argon"
	"github.com/example/wechat/internal/platform/cache"
	"github.com/example/wechat/internal/platform/clock"
	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/ids"
	"github.com/example/wechat/internal/platform/logger"
	"github.com/example/wechat/internal/platform/mysqlx"
	"github.com/example/wechat/internal/user"
)

type env struct {
	db  *sql.DB
	svc *Service
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
	clk := clock.NewFake(time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC))
	aud := audit.New(clk)
	log := logger.New("error", "text")
	convs := conversation.New(db, clk)
	devices := device.New(db, cache.NewMemory(), clk)
	users := user.New(db, clk.Now, nil)
	svc := New(db, devices, users, convs, aud, cache.NewMemory(), log, clk, Config{
		AccessTTL:     30 * time.Minute,
		RefreshTTL:    30 * 24 * time.Hour,
		LoginMaxFails: 3,
		LoginFreeze:   10 * time.Minute,
		Argon: argon.Params{
			Time: 1, Memory: 8 * 1024, Threads: 1, KeyLen: 32, SaltLen: 16,
		},
	})
	return &env{db: db, svc: svc}
}

func uniqueTag(t *testing.T) string {
	t.Helper()
	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(100000000))
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%08d", n.Int64())
}

func (e *env) register(t *testing.T, tag string) TokenPair {
	t.Helper()
	pair, err := e.svc.Register(context.Background(), RegisterRequest{
		Phone:       "+86" + tag,
		AccountName: "auth" + tag,
		Password:    "password123",
		Nickname:    "Auth Test " + tag,
		Device:      DeviceInfo{DeviceID: ids.New(), DeviceName: "test", Platform: "web"},
	}, "192.0.2.10")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, e.db, pair.UserID) })
	return pair
}

func cleanupUser(t *testing.T, db *sql.DB, userID int64) {
	t.Helper()
	for _, query := range []string{
		`DELETE FROM user_sessions WHERE user_id = ?`,
		`DELETE FROM user_devices WHERE user_id = ?`,
		`DELETE FROM conversation_members WHERE user_id = ?`,
		`DELETE FROM conversations WHERE transfer_owner_id = ?`,
		`DELETE FROM user_profiles WHERE user_id = ?`,
		`DELETE FROM user_moment_settings WHERE user_id = ?`,
		`DELETE FROM audit_logs WHERE actor_id = ?`,
		`DELETE FROM users WHERE id = ?`,
	} {
		if _, err := db.ExecContext(context.Background(), query, userID); err != nil {
			t.Logf("cleanup %q: %v", query, err)
		}
	}
}

func TestRegisterAndAuthenticate(t *testing.T) {
	e := newEnv(t)
	pair := e.register(t, uniqueTag(t))
	principal, err := e.svc.Authenticate(context.Background(), pair.AccessToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if principal.UserID != pair.UserID || principal.DeviceID != pair.DeviceID {
		t.Fatalf("principal = %+v, want user/device from pair", principal)
	}
}

func TestRefreshRotationAndReplay(t *testing.T) {
	e := newEnv(t)
	pair := e.register(t, uniqueTag(t))
	rotated, err := e.svc.Refresh(context.Background(), pair.RefreshToken, "192.0.2.10")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if rotated.RefreshToken == pair.RefreshToken || rotated.AccessToken == pair.AccessToken {
		t.Fatal("tokens were not rotated")
	}
	if _, err := e.svc.Authenticate(context.Background(), pair.AccessToken); err == nil {
		t.Fatal("old access token survived rotation")
	}
	if _, err := e.svc.Authenticate(context.Background(), rotated.AccessToken); err != nil {
		t.Fatalf("new access token rejected: %v", err)
	}
	if _, err := e.svc.Refresh(context.Background(), pair.RefreshToken, "192.0.2.10"); err == nil {
		t.Fatal("replayed refresh token was accepted")
	}
}

func TestRefreshInvalidatesCachedAccess(t *testing.T) {
	e := newEnv(t)
	pair := e.register(t, uniqueTag(t))
	if _, err := e.svc.Authenticate(context.Background(), pair.AccessToken); err != nil {
		t.Fatalf("prime access cache: %v", err)
	}
	rotated, err := e.svc.Refresh(context.Background(), pair.RefreshToken, "192.0.2.10")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := e.svc.Authenticate(context.Background(), pair.AccessToken); err == nil {
		t.Fatal("cached old access token survived rotation")
	}
	if _, err := e.svc.Authenticate(context.Background(), rotated.AccessToken); err != nil {
		t.Fatalf("new access token rejected: %v", err)
	}
}

func TestRefreshRejectsRevokedCurrentRefresh(t *testing.T) {
	e := newEnv(t)
	pair := e.register(t, uniqueTag(t))
	principal, err := e.svc.Authenticate(context.Background(), pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Logout(context.Background(), principal, "192.0.2.10"); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := e.svc.Refresh(context.Background(), pair.RefreshToken, "192.0.2.10"); err == nil {
		t.Fatal("revoked session accepted its current refresh token")
	}
}

func TestConcurrentRefreshAllowsOneWinner(t *testing.T) {
	e := newEnv(t)
	pair := e.register(t, uniqueTag(t))
	const refreshes = 8
	errs := make([]error, refreshes)
	var wg sync.WaitGroup
	for i := 0; i < refreshes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.svc.Refresh(context.Background(), pair.RefreshToken, "192.0.2.10")
		}(i)
	}
	wg.Wait()
	successes := 0
	unauthenticated := 0
	for _, err := range errs {
		if err == nil {
			successes++
			continue
		}
		if ae := apperrors.AsApp(err); ae.Code == apperrors.Unauthenticated {
			unauthenticated++
		} else {
			t.Fatalf("unexpected refresh error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("refresh successes = %d, want 1", successes)
	}
	if unauthenticated != refreshes-1 {
		t.Fatalf("lost refreshes = %d, want %d", unauthenticated, refreshes-1)
	}
}

func TestLoginLockoutSurvivesCorrectPassword(t *testing.T) {
	e := newEnv(t)
	tag := uniqueTag(t)
	pair := e.register(t, tag)
	req := LoginRequest{LoginID: pair.DeviceID, Password: "wrong-password", Device: DeviceInfo{DeviceID: ids.New()}}
	// LoginID must be phone/account name, not device ID.
	req.LoginID = "+86" + tag
	for i := 0; i < 3; i++ {
		if _, err := e.svc.Login(context.Background(), req, "192.0.2.10"); err == nil {
			t.Fatalf("wrong password %d was accepted", i+1)
		}
	}
	req.Password = "password123"
	if _, err := e.svc.Login(context.Background(), req, "192.0.2.10"); err == nil {
		t.Fatal("locked account accepted the correct password")
	}
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	e := newEnv(t)
	tag := uniqueTag(t)
	current := e.register(t, tag)
	other, err := e.svc.Login(context.Background(), LoginRequest{
		LoginID: "+86" + tag, Password: "password123",
		Device: DeviceInfo{DeviceID: ids.New(), Platform: "ios"},
	}, "192.0.2.11")
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	principal, err := e.svc.Authenticate(context.Background(), current.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ChangePassword(context.Background(), principal, "password123", "newpassword456", "192.0.2.10"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, err := e.svc.Authenticate(context.Background(), other.AccessToken); err == nil {
		t.Fatal("other session survived password change")
	} else if ae := apperrors.AsApp(err); ae.Code != apperrors.Unauthenticated {
		t.Fatalf("other session error = %v, want unauthenticated", err)
	}
	if _, err := e.svc.Authenticate(context.Background(), current.AccessToken); err != nil {
		t.Fatalf("current session rejected: %v", err)
	}
}
