// Package user owns users / user_profiles / user_moment_settings tables.
// Account creation runs inside the caller's transaction (auth.Register);
// profile read/update is the user-facing slice of SPEC-01 §2.6.
package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
	"github.com/example/wechat/internal/platform/validate"
)

// Account is the credential row (used by auth).
type Account struct {
	ID                 int64
	Phone              string
	AccountName        string
	PasswordHash       string
	MustChangePassword bool
	Status             string
}

// Profile is the public-facing profile row.
type Profile struct {
	UserID         int64
	Nickname       string
	Gender         string
	RegionCountry  string
	RegionProvince string
	RegionCity     string
	Signature      string
	StatusText     string
	AvatarMediaID  *int64
}

// CreateAccountParams is the atomic account-creation payload (SPEC-01 R7).
type CreateAccountParams struct {
	Phone        string
	AccountName  string // already normalized
	PasswordHash string
	Nickname     string
}

// WhichIdentifierTaken reports which unique identifier collided (for error
// messages after the unique index rejects the insert — lookup only happens
// AFTER the constraint fired, so no check-then-insert race).
func WhichIdentifierTaken(ctx context.Context, db mysqlx.DBTX, phone, accountName string) (string, error) {
	var one int
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM users WHERE phone = ? LIMIT 1`, phone).Scan(&one); err == nil {
		return "phone", nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM users WHERE account_name = ? LIMIT 1`, accountName).Scan(&one); err == nil {
		return "account_name", nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return "", fmt.Errorf("user: duplicate but neither identifier found")
}

// CreateAccountTx inserts users + user_profiles + user_moment_settings in the
// caller's transaction and returns the new user id (R7).
func CreateAccountTx(ctx context.Context, tx mysqlx.Tx, p CreateAccountParams, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO users (phone, account_name, password_hash, must_change_password, status, created_at, updated_at)
		VALUES (?, ?, ?, 0, 'active', ?, ?)`,
		p.Phone, p.AccountName, p.PasswordHash, now, now)
	if err != nil {
		return 0, err // caller maps 1062 → STATE_CONFLICT
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("user: last insert id: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_profiles (user_id, nickname, created_at, updated_at)
		VALUES (?, ?, ?, ?)`, id, p.Nickname, now, now); err != nil {
		return 0, fmt.Errorf("user: insert profile: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_moment_settings (user_id, created_at, updated_at)
		VALUES (?, ?, ?)`, id, now, now); err != nil {
		return 0, fmt.Errorf("user: insert moment settings: %w", err)
	}
	return id, nil
}

// UpdatePasswordHashTx sets a new hash (caller owns tx semantics).
func UpdatePasswordHashTx(ctx context.Context, tx mysqlx.Tx, userID int64, hash string, mustChange bool, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE users SET password_hash = ?, must_change_password = ?, updated_at = ?
		WHERE id = ?`, hash, b2i(mustChange), now, userID)
	if err != nil {
		return fmt.Errorf("user: update password: %w", err)
	}
	return nil
}

// MediaBinding is the port user needs from the media module (ADR-001: media
// tables are validated/written only by media). Implemented by media.Service.
type MediaBinding interface {
	// BindAvatarTx enforces the SPEC-01 R27 object constraints and records the
	// avatar references on the caller's transaction.
	BindAvatarTx(ctx context.Context, tx mysqlx.Tx, objectID, userID int64) error
}

// Service exposes profile read/write.
type Service struct {
	db    *sql.DB
	now   func() time.Time
	media MediaBinding
}

// New builds the profile service. media may be nil only in callers that never
// touch avatars.
func New(db *sql.DB, now func() time.Time, media MediaBinding) *Service {
	return &Service{db: db, now: now, media: media}
}

// FindAccountByID loads the credential row.
func (s *Service) FindAccountByID(ctx context.Context, id int64) (Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx,
		`SELECT id, phone, account_name, password_hash, must_change_password, status
		 FROM users WHERE id = ?`, id))
}

// FindAccountByIdentifier loads by exact phone or account_name (normalized).
func (s *Service) FindAccountByIdentifier(ctx context.Context, phone, accountName string) (Account, error) {
	if phone != "" {
		return scanAccount(s.db.QueryRowContext(ctx,
			`SELECT id, phone, account_name, password_hash, must_change_password, status
			 FROM users WHERE phone = ?`, phone))
	}
	return scanAccount(s.db.QueryRowContext(ctx,
		`SELECT id, phone, account_name, password_hash, must_change_password, status
		 FROM users WHERE account_name = ?`, accountName))
}

func scanAccount(row *sql.Row) (Account, error) {
	var a Account
	var mustChange int
	err := row.Scan(&a.ID, &a.Phone, &a.AccountName, &a.PasswordHash, &mustChange, &a.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return a, apperrors.Unavail("account not found")
	}
	if err != nil {
		return a, fmt.Errorf("user: scan account: %w", err)
	}
	a.MustChangePassword = mustChange == 1
	return a, nil
}

// GetProfile loads the profile row.
func (s *Service) GetProfile(ctx context.Context, userID int64) (Profile, error) {
	var p Profile
	var avatar sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id, nickname, gender, region_country, region_province, region_city,
		       signature, status_text, avatar_media_id
		FROM user_profiles WHERE user_id = ?`, userID).
		Scan(&p.UserID, &p.Nickname, &p.Gender, &p.RegionCountry, &p.RegionProvince,
			&p.RegionCity, &p.Signature, &p.StatusText, &avatar)
	if errors.Is(err, sql.ErrNoRows) {
		return p, apperrors.Unavail("profile not found")
	}
	if err != nil {
		return p, fmt.Errorf("user: get profile: %w", err)
	}
	if avatar.Valid {
		v := avatar.Int64
		p.AvatarMediaID = &v
	}
	return p, nil
}

// ProfileUpdate is a partial profile patch (nil = unchanged). SPEC-01 R26.
type ProfileUpdate struct {
	Nickname       *string
	Gender         *string
	RegionCountry  *string
	RegionProvince *string
	RegionCity     *string
	Signature      *string
	StatusText     *string
}

// Validate checks every provided field against the contract limits.
func (u *ProfileUpdate) Validate() error {
	check := func(v *string, maxRunes int, name string) error {
		if v == nil {
			return nil
		}
		if !validate.RuneRange(*v, 0, maxRunes) {
			return apperrors.Invalid(fmt.Sprintf("%s must be 0..%d characters", name, maxRunes))
		}
		if !validate.NoControl(*v) {
			return apperrors.Invalid(fmt.Sprintf("%s contains control or edge whitespace", name))
		}
		return nil
	}
	if err := check(u.Nickname, 32, "nickname"); err != nil {
		return err
	}
	if u.Nickname != nil && !validate.RuneRange(*u.Nickname, 2, 32) {
		return apperrors.Invalid("nickname must be 2..32 characters")
	}
	if u.Gender != nil && !validate.OneOf(*u.Gender, "unspecified", "male", "female") {
		return apperrors.Invalid("gender must be unspecified|male|female")
	}
	if err := check(u.RegionCountry, 64, "region.country"); err != nil {
		return err
	}
	if err := check(u.RegionProvince, 64, "region.province"); err != nil {
		return err
	}
	if err := check(u.RegionCity, 64, "region.city"); err != nil {
		return err
	}
	if err := check(u.Signature, 100, "signature"); err != nil {
		return err
	}
	if err := check(u.StatusText, 32, "status"); err != nil {
		return err
	}
	return nil
}

// UpdateProfile applies a validated patch (R26, R29: no historical rewrite).
func (s *Service) UpdateProfile(ctx context.Context, userID int64, u ProfileUpdate) error {
	if err := u.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE user_profiles SET
			nickname        = COALESCE(?, nickname),
			gender          = COALESCE(?, gender),
			region_country  = COALESCE(?, region_country),
			region_province = COALESCE(?, region_province),
			region_city     = COALESCE(?, region_city),
			signature       = COALESCE(?, signature),
			status_text     = COALESCE(?, status_text),
			updated_at      = ?
		WHERE user_id = ?`,
		nullable(u.Nickname), nullable(u.Gender), nullable(u.RegionCountry),
		nullable(u.RegionProvince), nullable(u.RegionCity), nullable(u.Signature),
		nullable(u.StatusText), s.now(), userID)
	if err != nil {
		return fmt.Errorf("user: update profile: %w", err)
	}
	return nil
}

// SetAvatar binds an avatar media object atomically (R27): the media-side
// constraints, the avatar references and the profile row commit in one
// transaction, so a failure can never leave references pointing at a profile
// that was never updated (ADR-001: each module writes only its own tables).
func (s *Service) SetAvatar(ctx context.Context, userID, mediaID int64) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if err := s.media.BindAvatarTx(ctx, tx, mediaID, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE user_profiles SET avatar_media_id = ?, updated_at = ? WHERE user_id = ?`,
			mediaID, s.now(), userID); err != nil {
			return fmt.Errorf("user: set avatar: %w", err)
		}
		return nil
	})
}

func nullable[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
