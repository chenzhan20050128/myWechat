// Package device owns user_devices / user_sessions tables: device registry,
// session lifecycle (issue, rotate, revoke) and the cache invalidation that
// makes revocation immediate (ADR-003). SPEC-01 §2.5.
package device

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/example/wechat/internal/platform/cache"
	"github.com/example/wechat/internal/platform/clock"
	"github.com/example/wechat/internal/platform/mysqlx"
)

const (
	// accessCacheKey prefixes the per-token session cache entry.
	accessCacheKey = "sess:at:"
)

// Session is a login instance on a device.
type Session struct {
	ID                 int64
	UserID             int64
	DeviceID           string
	AccessTokenHash    string
	AccessExpiresAt    time.Time
	RefreshTokenHash   string
	PrevRefreshHash    string // rotated-out hash, for reuse detection (D2)
	RefreshExpiresAt   time.Time
	MustChangePassword bool
	Revoked            bool
}

// Device aggregates the registry row (for the device list view).
type Device struct {
	ID           string
	Name         string
	Platform     string
	FirstLoginAt time.Time
	LastActiveAt time.Time
	LastIP       string
	SessionID    int64
	Current      bool
}

// Service issues and revokes sessions.
type Service struct {
	db    *sql.DB
	cache cache.Cache
	now   clock.Clock
}

// New builds the device service.
func New(db *sql.DB, c cache.Cache, clk clock.Clock) *Service {
	return &Service{db: db, cache: c, now: clk}
}

// UpsertDeviceTx registers/refreshes a device row for a user. deviceID is
// client-generated and stable per physical device (SPEC-01 R20).
func (s *Service) UpsertDeviceTx(ctx context.Context, tx mysqlx.Tx, deviceID string, userID int64, name, platform, ip string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO user_devices (id, user_id, device_name, platform, first_login_at, last_active_at, last_ip)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			user_id = VALUES(user_id), device_name = VALUES(device_name),
			platform = VALUES(platform), last_active_at = VALUES(last_active_at),
			last_ip = VALUES(last_ip)`,
		deviceID, userID, name, platform, now, now, ip)
	if err != nil {
		return fmt.Errorf("device: upsert: %w", err)
	}
	return nil
}

// IssueSessionTx creates a session row with fresh tokens (R14). Returns the
// session; token plaintexts are produced by the caller (auth) and passed in.
func (s *Service) IssueSessionTx(ctx context.Context, tx mysqlx.Tx, userID int64, deviceID, accessHash string, accessExp time.Time, refreshHash string, refreshExp time.Time, ip string) (int64, error) {
	now := s.now.Now()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO user_sessions
			(user_id, device_id, access_token_hash, access_expires_at,
			 refresh_token_hash, refresh_expires_at, created_at, last_seen_at, last_ip)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, deviceID, accessHash, accessExp, refreshHash, refreshExp, now, now, ip)
	if err != nil {
		return 0, fmt.Errorf("device: issue session: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("device: session id: %w", err)
	}
	return id, nil
}

// FindByAccessHash resolves an access token hash to a live session
// (revoked_at IS NULL, not expired). Cache-first, DB fallback (ADR-003).
func (s *Service) FindByAccessHash(ctx context.Context, hash string) (Session, error) {
	if v, ok := s.cache.Get(ctx, accessCacheKey+hash); ok {
		if sess, ok := decodeSession(v); ok {
			return sess, nil
		}
	}
	var sess Session
	var prev sql.NullString
	var mustChange int
	err := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.user_id, s.device_id, s.access_expires_at,
		       s.refresh_token_hash, s.prev_refresh_token_hash, s.refresh_expires_at, u.must_change_password
		FROM user_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.access_token_hash = ? AND s.revoked_at IS NULL`,
		hash).Scan(&sess.ID, &sess.UserID, &sess.DeviceID, &sess.AccessExpiresAt,
		&sess.RefreshTokenHash, &prev, &sess.RefreshExpiresAt, &mustChange)
	if errors.Is(err, sql.ErrNoRows) {
		return sess, sql.ErrNoRows
	}
	if err != nil {
		return sess, fmt.Errorf("device: find session: %w", err)
	}
	if !sess.AccessExpiresAt.After(s.now.Now()) {
		return Session{}, sql.ErrNoRows
	}
	sess.AccessTokenHash = hash
	sess.PrevRefreshHash = prev.String
	sess.MustChangePassword = mustChange == 1
	s.cacheSession(ctx, hash, sess)
	return sess, nil
}

// FindByRefreshHash resolves by current refresh hash; if absent, checks the
// rotated-out hash for reuse detection (R16). Returns (session, reused).
func (s *Service) FindByRefreshHash(ctx context.Context, hash string) (Session, bool, error) {
	var sess Session
	var prev sql.NullString
	var mustChange int
	var revokedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.user_id, s.device_id, s.access_token_hash, s.access_expires_at,
		       s.refresh_token_hash, s.prev_refresh_token_hash, s.refresh_expires_at,
		       u.must_change_password, s.revoked_at
		FROM user_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.refresh_token_hash = ? OR (s.prev_refresh_token_hash = ? AND s.revoked_at IS NULL)`,
		hash, hash).
		Scan(&sess.ID, &sess.UserID, &sess.DeviceID, &sess.AccessTokenHash, &sess.AccessExpiresAt,
			&sess.RefreshTokenHash, &prev, &sess.RefreshExpiresAt, &mustChange, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, sql.ErrNoRows
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("device: find by refresh: %w", err)
	}
	sess.PrevRefreshHash = prev.String
	sess.MustChangePassword = mustChange == 1
	sess.Revoked = revokedAt.Valid
	reused := sess.RefreshTokenHash != hash // matched only via prev hash
	return sess, reused, nil
}

// RotateSessionTx atomically replaces both token hashes (R15). Optimistic
// guard on the current refresh hash: zero rows affected = concurrent rotation
// — the caller must treat that as reuse (R16).
func (s *Service) RotateSessionTx(ctx context.Context, tx mysqlx.Tx, sessionID int64, oldRefreshHash, newAccessHash string, accessExp time.Time, newRefreshHash string, refreshExp time.Time) error {
	now := s.now.Now()
	res, err := tx.ExecContext(ctx, `
		UPDATE user_sessions SET
			prev_refresh_token_hash = refresh_token_hash,
			refresh_token_hash = ?, refresh_expires_at = ?,
			access_token_hash = ?, access_expires_at = ?,
			last_seen_at = ?
		WHERE id = ? AND refresh_token_hash = ? AND revoked_at IS NULL`,
		newRefreshHash, refreshExp, newAccessHash, accessExp, now, sessionID, oldRefreshHash)
	if err != nil {
		return fmt.Errorf("device: rotate session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConcurrentRotation
	}
	return nil
}

// ErrConcurrentRotation marks a lost race on rotation (caller revokes).
var ErrConcurrentRotation = errors.New("device: concurrent rotation")

// TouchAccessTx updates the cached principal after rotation.
func (s *Service) cacheSession(ctx context.Context, accessHash string, sess Session) {
	ttl := time.Until(sess.AccessExpiresAt)
	if ttl <= 0 {
		return
	}
	s.cache.Set(ctx, accessCacheKey+accessHash, encodeSession(sess), ttl)
}

// RevokeSession marks one session revoked and purges its cache entry.
func (s *Service) RevokeSession(ctx context.Context, sessionID int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE user_sessions SET revoked_at = ?, revoke_reason = ?
		WHERE id = ? AND revoked_at IS NULL`, s.now.Now(), reason, sessionID)
	if err != nil {
		return fmt.Errorf("device: revoke session: %w", err)
	}
	return s.purgeSessionCache(ctx, sessionID)
}

// RevokeOthers revokes every live session of user except the given device.
// Returns the number of revoked sessions.
func (s *Service) RevokeOthers(ctx context.Context, userID int64, exceptDeviceID, reason string) (int64, error) {
	hashes, err := s.liveAccessHashes(ctx, userID, &exceptDeviceID)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_sessions SET revoked_at = ?, revoke_reason = ?
		WHERE user_id = ? AND revoked_at IS NULL AND device_id <> ?`,
		s.now.Now(), reason, userID, exceptDeviceID)
	if err != nil {
		return 0, fmt.Errorf("device: revoke others: %w", err)
	}
	n, _ := res.RowsAffected()
	s.purgeCache(ctx, hashes)
	return n, nil
}

// RevokeDevice revokes every live session on the given device of user.
func (s *Service) RevokeDevice(ctx context.Context, userID int64, deviceID, reason string) (int64, error) {
	hashes, err := s.liveAccessHashes(ctx, userID, &deviceID)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_sessions SET revoked_at = ?, revoke_reason = ?
		WHERE user_id = ? AND device_id = ? AND revoked_at IS NULL`,
		s.now.Now(), reason, userID, deviceID)
	if err != nil {
		return 0, fmt.Errorf("device: revoke device: %w", err)
	}
	n, _ := res.RowsAffected()
	s.purgeCache(ctx, hashes)
	return n, nil
}

// RevokeAllButSession revokes every live session except sessionID (password
// change: current device survives, contract §3.2 R18).
func (s *Service) RevokeAllButSession(ctx context.Context, tx mysqlx.Tx, userID, sessionID int64, reason string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT access_token_hash FROM user_sessions
		WHERE user_id = ? AND revoked_at IS NULL AND id <> ?`, userID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("device: collect hashes: %w", err)
	}
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return nil, err
		}
		hashes = append(hashes, h)
	}
	rows.Close()
	if _, err := tx.ExecContext(ctx, `
		UPDATE user_sessions SET revoked_at = ?, revoke_reason = ?
		WHERE user_id = ? AND revoked_at IS NULL AND id <> ?`,
		s.now.Now(), reason, userID, sessionID); err != nil {
		return nil, fmt.Errorf("device: revoke all but session: %w", err)
	}
	return hashes, nil
}

// ListDevices returns one row per live session with device info (R21).
func (s *Service) ListDevices(ctx context.Context, userID int64, currentDeviceID string) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, d.device_name, d.platform, d.first_login_at, d.last_active_at, d.last_ip, s.id
		FROM user_sessions s JOIN user_devices d ON d.id = s.device_id
		WHERE s.user_id = ? AND s.revoked_at IS NULL
		ORDER BY s.last_seen_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("device: list: %w", err)
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Name, &d.Platform, &d.FirstLoginAt, &d.LastActiveAt, &d.LastIP, &d.SessionID); err != nil {
			return nil, err
		}
		d.Current = d.ID == currentDeviceID
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Service) liveAccessHashes(ctx context.Context, userID int64, onlyDevice *string) ([]string, error) {
	q := `SELECT access_token_hash FROM user_sessions WHERE user_id = ? AND revoked_at IS NULL`
	args := []any{userID}
	if onlyDevice != nil {
		q += ` AND device_id = ?`
		args = append(args, *onlyDevice)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("device: live hashes: %w", err)
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		hashes = append(hashes, h)
	}
	return hashes, rows.Err()
}

func (s *Service) purgeCache(ctx context.Context, hashes []string) {
	keys := make([]string, 0, len(hashes))
	for _, h := range hashes {
		keys = append(keys, accessCacheKey+h)
	}
	if len(keys) > 0 {
		s.cache.Del(ctx, keys...)
	}
}

func (s *Service) purgeSessionCache(ctx context.Context, sessionID int64) error {
	// the access hash may have rotated; read it before the row was already
	// marked revoked (best-effort: cache entries also carry TTL).
	var h string
	err := s.db.QueryRowContext(ctx,
		`SELECT access_token_hash FROM user_sessions WHERE id = ?`, sessionID).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	s.cache.Del(ctx, accessCacheKey+h)
	return nil
}

// PurgeAccessCache evicts one access-token cache entry (used by auth after
// rotation/revocation — keeps the key scheme private to this package).
func (s *Service) PurgeAccessCache(ctx context.Context, accessHash string) {
	s.cache.Del(ctx, accessCacheKey+accessHash)
}

// encodeSession/decodeSession: compact "id|user|device|mustChange|expMs".
// Device IDs are UUIDs — '|' cannot appear inside fields.
func encodeSession(sess Session) string {
	m := 0
	if sess.MustChangePassword {
		m = 1
	}
	return fmt.Sprintf("%d|%d|%s|%d|%d",
		sess.ID, sess.UserID, sess.DeviceID, m, sess.AccessExpiresAt.UnixMilli())
}

func decodeSession(v string) (Session, bool) {
	parts := strings.Split(v, "|")
	if len(parts) != 5 {
		return Session{}, false
	}
	id, err1 := strconv.ParseInt(parts[0], 10, 64)
	uid, err2 := strconv.ParseInt(parts[1], 10, 64)
	m, err3 := strconv.Atoi(parts[3])
	exp, err4 := strconv.ParseInt(parts[4], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return Session{}, false
	}
	return Session{
		ID: id, UserID: uid, DeviceID: parts[2],
		MustChangePassword: m == 1,
		AccessExpiresAt:    time.UnixMilli(exp).UTC(),
	}, true
}
