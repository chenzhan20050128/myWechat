// Package auth orchestrates registration, login, token refresh, logout and
// password change (SPEC-01). It implements httpx.Authenticator (ADR-013) and
// depends on user/device/conversation services only through their exported
// transactional functions — never on their tables.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/example/wechat/internal/audit"
	"github.com/example/wechat/internal/conversation"
	"github.com/example/wechat/internal/device"
	"github.com/example/wechat/internal/platform/argon"
	"github.com/example/wechat/internal/platform/cache"
	"github.com/example/wechat/internal/platform/clock"
	"github.com/example/wechat/internal/platform/httpx"
	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/ids"
	"github.com/example/wechat/internal/platform/mysqlx"
	"github.com/example/wechat/internal/platform/ratelimit"
	"github.com/example/wechat/internal/platform/validate"
	"github.com/example/wechat/internal/user"
)

const (
	failKeyPrefix  = "fail:login:" // consecutive-failure counter
	lockKeyPrefix  = "lock:login:" // freeze marker
	refreshRLName  = "auth.refresh"
	dummyHash      = "$argon2id$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // timing equalizer (A2)
)

// Config carries the tunables (from platform config).
type Config struct {
	AccessTTL      time.Duration
	RefreshTTL     time.Duration
	LoginMaxFails  int
	LoginFreeze    time.Duration
	Argon          argon.Params
}

// Service is the auth façade.
type Service struct {
	db      *sql.DB
	devices *device.Service
	users   *user.Service
	convs   *conversation.Service
	audit   *audit.Service
	cache   cache.Cache
	rl      *ratelimit.Limiter
	log     *slog.Logger
	now     clock.Clock
	cfg     Config
}

// New wires the auth service.
func New(db *sql.DB, devices *device.Service, users *user.Service, convs *conversation.Service,
	aud *audit.Service, c cache.Cache, log *slog.Logger, clk clock.Clock, cfg Config) *Service {
	return &Service{
		db: db, devices: devices, users: users, convs: convs, audit: aud,
		cache: c, rl: ratelimit.New(c, refreshRLName, 10, time.Minute),
		log: log, now: clk, cfg: cfg,
	}
}

// TokenPair is the login/register/refresh response payload.
type TokenPair struct {
	UserID            int64     `json:"user_id,string"`
	AccessToken       string    `json:"access_token"`
	AccessExpiresAt   time.Time `json:"access_expires_at"`
	RefreshToken      string    `json:"refresh_token"`
	RefreshExpiresAt  time.Time `json:"refresh_expires_at"`
	DeviceID          string    `json:"device_id"`
	MustChangePwd     bool      `json:"must_change_password"`
}

// DeviceInfo is the client-reported device identity (login/register body).
type DeviceInfo struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	Platform   string `json:"platform"`
}

func (d DeviceInfo) validate() error {
	if d.DeviceID == "" || len(d.DeviceID) != 36 {
		return apperrors.Invalid("device_id must be a UUID")
	}
	if d.DeviceName == "" {
		d.DeviceName = "unknown"
	}
	if !validate.RuneRange(d.DeviceName, 0, 64) {
		return apperrors.Invalid("device_name too long")
	}
	if !validate.OneOf(d.Platform, "ios", "android", "windows", "mac", "web", "unknown", "") {
		return apperrors.Invalid("invalid platform")
	}
	return nil
}

func (d DeviceInfo) name() string {
	if d.DeviceName == "" {
		return "unknown"
	}
	return d.DeviceName
}

func (d DeviceInfo) platform() string {
	if d.Platform == "" {
		return "unknown"
	}
	return d.Platform
}

// Register creates an account and logs it in (SPEC-01 R1–R8).
func (s *Service) Register(ctx context.Context, req RegisterRequest, ip string) (TokenPair, error) {
	phone := validate.NormalizePhone(req.Phone)
	if phone == "" || !validate.Phone(phone) {
		return TokenPair{}, apperrors.Invalid("invalid phone")
	}
	accountName := validate.NormalizeAccountName(req.AccountName)
	if accountName == "" || !validate.AccountName(accountName) {
		return TokenPair{}, apperrors.Invalid("account_name must be 6-20 chars of [a-z0-9_], not reserved")
	}
	if !validate.Password(req.Password) {
		return TokenPair{}, apperrors.Invalid("password must be 8-32 chars with letters and digits")
	}
	nickname := strings.TrimSpace(req.Nickname)
	if !validate.RuneRange(nickname, 2, 32) || !validate.NoControl(nickname) ||
		!validate.NotReserved(strings.ToLower(nickname)) {
		return TokenPair{}, apperrors.Invalid("nickname must be 2-32 valid characters")
	}
	if err := req.Device.validate(); err != nil {
		return TokenPair{}, err
	}

	hash, err := argon.Hash(req.Password, s.cfg.Argon)
	if err != nil {
		return TokenPair{}, apperrors.Wrap(apperrors.InternalError, "hash password", err)
	}

	var pair TokenPair
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		now := s.now.Now()
		userID, err := user.CreateAccountTx(ctx, tx, user.CreateAccountParams{
			Phone: phone, AccountName: accountName, PasswordHash: hash, Nickname: nickname,
		}, now)
		if err != nil {
			if mysqlx.IsDuplicate(err, "uk_users_phone") {
				return apperrors.Conflict("phone already registered")
			}
			if mysqlx.IsDuplicate(err, "uk_users_account_name") {
				return apperrors.Conflict("account_name already taken")
			}
			return apperrors.Wrap(apperrors.InternalError, "create account", err)
		}
		if _, err := s.convs.CreateTransferTx(ctx, tx, userID); err != nil {
			return apperrors.Wrap(apperrors.InternalError, "create transfer conversation", err)
		}
		if err := s.audit.LogTx(ctx, tx, audit.Entry{
			Type: "auth.register", ActorID: userID, IP: ip,
			Detail: map[string]any{"account_name": accountName, "device": req.Device.DeviceID},
		}); err != nil {
			return err
		}
		p, err := s.issueTokensTx(ctx, tx, userID, req.Device, ip)
		if err != nil {
			return err
		}
		pair = p
		return nil
	})
	if err != nil {
		return TokenPair{}, err
	}
	return pair, nil
}

// Login authenticates by phone or account_name (R9–R13, R13a).
func (s *Service) Login(ctx context.Context, req LoginRequest, ip string) (TokenPair, error) {
	if !validate.Password(req.Password) {
		// structurally invalid password can never match; burn the argon time
		// anyway so response timing does not leak validity checks
		_, _ = argon.Verify(req.Password, dummyHash)
		return TokenPair{}, invalidCreds()
	}
	if err := req.Device.validate(); err != nil {
		return TokenPair{}, err
	}

	var phone, accountName string
	if validate.Phone(validate.NormalizePhone(req.LoginID)) {
		phone = validate.NormalizePhone(req.LoginID)
	} else {
		accountName = validate.NormalizeAccountName(req.LoginID)
		if accountName == "" {
			return TokenPair{}, invalidCreds()
		}
	}

	acct, err := s.users.FindAccountByIdentifier(ctx, phone, accountName)
	if err != nil {
		// timing equalizer: unknown account still pays a full argon verify
		_, _ = argon.Verify(req.Password, dummyHash)
		s.audit.Log(ctx, s.db, audit.Entry{Type: "auth.login.failure", IP: ip,
			Detail: map[string]any{"reason": "unknown_account"}})
		return TokenPair{}, invalidCreds()
	}

	// freeze check first (R10)
	lockKey := lockKeyPrefix + fmt.Sprint(acct.ID)
	if _, ok := s.cache.Get(ctx, lockKey); ok {
		s.audit.Log(ctx, s.db, audit.Entry{Type: "auth.login.failure", ActorID: acct.ID, IP: ip,
			Detail: map[string]any{"reason": "frozen"}})
		return TokenPair{}, invalidCreds()
	}

	ok, err := argon.Verify(req.Password, acct.PasswordHash)
	if err != nil || !ok {
		s.recordFailure(ctx, acct.ID)
		s.audit.Log(ctx, s.db, audit.Entry{Type: "auth.login.failure", ActorID: acct.ID, IP: ip,
			Detail: map[string]any{"reason": "bad_password"}})
		return TokenPair{}, invalidCreds()
	}

	// success: clear failures (R11)
	s.cache.Del(ctx, failKeyPrefix+fmt.Sprint(acct.ID))

	// transparent rehash on parameter drift (R13a, design-review D10)
	if argon.NeedsRehash(acct.PasswordHash, s.cfg.Argon) {
		if newHash, herr := argon.Hash(req.Password, s.cfg.Argon); herr == nil {
			if uerr := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
				return user.UpdatePasswordHashTx(ctx, tx, acct.ID, newHash, acct.MustChangePassword, s.now.Now())
			}); uerr != nil {
				s.log.Warn("auth: rehash-on-login failed", "user_id", acct.ID, "err", uerr)
			}
		}
	}

	var pair TokenPair
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		p, err := s.issueTokensTx(ctx, tx, acct.ID, req.Device, ip)
		if err != nil {
			return err
		}
		pair = p
		return nil
	})
	if err != nil {
		return TokenPair{}, err
	}
	s.audit.Log(ctx, s.db, audit.Entry{Type: "auth.login.success", ActorID: acct.ID, IP: ip,
		Detail: map[string]any{"device": req.Device.DeviceID}})
	return pair, nil
}

// Refresh rotates tokens (R15–R17).
func (s *Service) Refresh(ctx context.Context, refreshToken, ip string) (TokenPair, error) {
	hash := ids.SHA256Hex(refreshToken)
	if ok, _ := s.rl.Allow(ctx, hash[:16]); !ok {
		return TokenPair{}, apperrors.New(apperrors.RateLimited, "too many refresh attempts")
	}

	sess, reused, err := s.devices.FindByRefreshHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return TokenPair{}, apperrors.Unauth("invalid refresh token")
	}
	if err != nil {
		return TokenPair{}, apperrors.Wrap(apperrors.InternalError, "refresh lookup", err)
	}
	if reused {
		// rotated-out token replayed → revoke the whole session (R16, D2)
		_ = s.devices.RevokeSession(ctx, sess.ID, "refresh_reuse")
		s.audit.Log(ctx, s.db, audit.Entry{Type: "auth.refresh.reuse_revoked", ActorID: sess.UserID, IP: ip,
			Detail: map[string]any{"session_id": sess.ID}})
		return TokenPair{}, apperrors.Unauth("invalid refresh token")
	}
	if !sess.RefreshExpiresAt.After(s.now.Now()) {
		return TokenPair{}, apperrors.Unauth("refresh token expired")
	}

	access, err := ids.NewToken(32)
	if err != nil {
		return TokenPair{}, apperrors.Wrap(apperrors.InternalError, "token gen", err)
	}
	refresh, err := ids.NewToken(48)
	if err != nil {
		return TokenPair{}, apperrors.Wrap(apperrors.InternalError, "token gen", err)
	}
	now := s.now.Now()
	newAccessHash := ids.SHA256Hex(access)
	newRefreshHash := ids.SHA256Hex(refresh)

	var pair TokenPair
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if err := s.devices.RotateSessionTx(ctx, tx, sess.ID, hash,
			newAccessHash, now.Add(s.cfg.AccessTTL), newRefreshHash, now.Add(s.cfg.RefreshTTL)); err != nil {
			if errors.Is(err, device.ErrConcurrentRotation) {
				// lost race: another request rotated first; presenting the
				// already-rotated token is reuse → revoke (conservative, D2)
				_ = s.devices.RevokeSession(ctx, sess.ID, "refresh_reuse")
				return apperrors.Unauth("invalid refresh token")
			}
			return apperrors.Wrap(apperrors.InternalError, "rotate session", err)
		}
		pair = TokenPair{
			UserID:           sess.UserID,
			AccessToken:      access,
			AccessExpiresAt:  now.Add(s.cfg.AccessTTL),
			RefreshToken:     refresh,
			RefreshExpiresAt: now.Add(s.cfg.RefreshTTL),
			DeviceID:         sess.DeviceID,
			MustChangePwd:    sess.MustChangePassword,
		}
		return nil
	})
	if err != nil {
		return TokenPair{}, err
	}
	// old access token is dead (single-active-token, D1): purge its cache entry
	s.devices.PurgeAccessCache(ctx, hash)
	return pair, nil
}

// Logout revokes the current session (R24).
func (s *Service) Logout(ctx context.Context, principal httpx.Principal, ip string) error {
	if err := s.devices.RevokeSession(ctx, principal.SessionID, "logout"); err != nil {
		return apperrors.Wrap(apperrors.InternalError, "logout", err)
	}
	s.audit.Log(ctx, s.db, audit.Entry{Type: "auth.logout", ActorID: principal.UserID, IP: ip,
		Detail: map[string]any{"session_id": principal.SessionID}})
	return nil
}

// ChangePassword validates the old password, rotates the hash and revokes all
// other sessions in one transaction (R18).
func (s *Service) ChangePassword(ctx context.Context, principal httpx.Principal, oldPassword, newPassword, ip string) error {
	if !validate.Password(newPassword) {
		return apperrors.Invalid("new password must be 8-32 chars with letters and digits")
	}
	acct, err := s.users.FindAccountByID(ctx, principal.UserID)
	if err != nil {
		return err
	}
	ok, err := argon.Verify(oldPassword, acct.PasswordHash)
	if err != nil || !ok {
		return invalidCreds()
	}
	newHash, err := argon.Hash(newPassword, s.cfg.Argon)
	if err != nil {
		return apperrors.Wrap(apperrors.InternalError, "hash password", err)
	}
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if err := user.UpdatePasswordHashTx(ctx, tx, principal.UserID, newHash, false, s.now.Now()); err != nil {
			return apperrors.Wrap(apperrors.InternalError, "update password", err)
		}
		hashes, err := s.devices.RevokeAllButSession(ctx, tx, principal.UserID, principal.SessionID, "password_changed")
		if err != nil {
			return apperrors.Wrap(apperrors.InternalError, "revoke sessions", err)
		}
		for _, h := range hashes {
			s.devices.PurgeAccessCache(ctx, h)
		}
		if err := s.audit.LogTx(ctx, tx, audit.Entry{
			Type: "auth.password.changed", ActorID: principal.UserID, IP: ip,
		}); err != nil {
			return err
		}
		// current session's cached principal may still carry must_change=true
		// if it was set; purge so the next request re-reads from DB
		var curHash string
		if err := tx.QueryRowContext(ctx,
			`SELECT access_token_hash FROM user_sessions WHERE id = ?`, principal.SessionID).Scan(&curHash); err == nil {
			s.devices.PurgeAccessCache(ctx, curHash)
		}
		return nil
	})
}

// ResetPasswordByOperator implements the operator reset flow (R19). Returns
// the one-time temporary password; caller (operator module, phase 6) is
// responsible for delivery and audit context.
func (s *Service) ResetPasswordByOperator(ctx context.Context, operatorID, targetUserID int64, reason string) (string, error) {
	temp, err := ids.NewToken(12) // 16 chars base64url, letters+digits dominated
	if err != nil {
		return "", apperrors.Wrap(apperrors.InternalError, "token gen", err)
	}
	hash, err := argon.Hash(temp, s.cfg.Argon)
	if err != nil {
		return "", apperrors.Wrap(apperrors.InternalError, "hash password", err)
	}
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if err := user.UpdatePasswordHashTx(ctx, tx, targetUserID, hash, true, s.now.Now()); err != nil {
			return apperrors.Wrap(apperrors.InternalError, "update password", err)
		}
		// sessionID 0 matches nothing → revoke EVERY session of the target
		hashes, err := s.devices.RevokeAllButSession(ctx, tx, targetUserID, 0, "password_reset")
		if err != nil {
			return apperrors.Wrap(apperrors.InternalError, "revoke sessions", err)
		}
		for _, h := range hashes {
			s.devices.PurgeAccessCache(ctx, h)
		}
		return s.audit.LogTx(ctx, tx, audit.Entry{
			Type: "auth.password.reset_by_operator", ActorID: operatorID,
			Detail: map[string]any{"target_user": targetUserID, "reason": reason},
		})
	})
	if err != nil {
		return "", err
	}
	return temp, nil
}

// Authenticate implements httpx.Authenticator (ADR-003 chain).
func (s *Service) Authenticate(ctx context.Context, accessToken string) (httpx.Principal, error) {
	hash := ids.SHA256Hex(accessToken)
	sess, err := s.devices.FindByAccessHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return httpx.Principal{}, apperrors.Unauth("invalid access token")
	}
	if err != nil {
		return httpx.Principal{}, apperrors.Wrap(apperrors.InternalError, "authenticate", err)
	}
	return httpx.Principal{
		UserID:             sess.UserID,
		DeviceID:           sess.DeviceID,
		SessionID:          sess.ID,
		MustChangePassword: sess.MustChangePassword,
	}, nil
}

func (s *Service) issueTokensTx(ctx context.Context, tx mysqlx.Tx, userID int64, dev DeviceInfo, ip string) (TokenPair, error) {
	access, err := ids.NewToken(32)
	if err != nil {
		return TokenPair{}, apperrors.Wrap(apperrors.InternalError, "token gen", err)
	}
	refresh, err := ids.NewToken(48)
	if err != nil {
		return TokenPair{}, apperrors.Wrap(apperrors.InternalError, "token gen", err)
	}
	now := s.now.Now()
	accessExp := now.Add(s.cfg.AccessTTL)
	refreshExp := now.Add(s.cfg.RefreshTTL)

	if err := s.devices.UpsertDeviceTx(ctx, tx, dev.DeviceID, userID, dev.name(), dev.platform(), ip, now); err != nil {
		return TokenPair{}, apperrors.Wrap(apperrors.InternalError, "upsert device", err)
	}
	sessionID, err := s.devices.IssueSessionTx(ctx, tx, userID, dev.DeviceID,
		ids.SHA256Hex(access), accessExp, ids.SHA256Hex(refresh), refreshExp, ip)
	if err != nil {
		return TokenPair{}, apperrors.Wrap(apperrors.InternalError, "issue session", err)
	}
	_ = sessionID
	return TokenPair{
		UserID:           userID,
		AccessToken:      access,
		AccessExpiresAt:  accessExp,
		RefreshToken:     refresh,
		RefreshExpiresAt: refreshExp,
		DeviceID:         dev.DeviceID,
	}, nil
}

// recordFailure increments the consecutive-failure counter and freezes the
// account after LoginMaxFails (R11).
func (s *Service) recordFailure(ctx context.Context, userID int64) {
	key := failKeyPrefix + fmt.Sprint(userID)
	n := s.cache.Incr(ctx, key, s.cfg.LoginFreeze)
	if n >= int64(s.cfg.LoginMaxFails) {
		s.cache.Set(ctx, lockKeyPrefix+fmt.Sprint(userID), "1", s.cfg.LoginFreeze)
		s.cache.Del(ctx, key)
		s.audit.Log(ctx, s.db, audit.Entry{Type: "auth.login.frozen", ActorID: userID,
			Detail: map[string]any{"frozen_for": s.cfg.LoginFreeze.String()}})
	}
}

func invalidCreds() error {
	return apperrors.New(apperrors.InvalidCredentials, "invalid credentials")
}
