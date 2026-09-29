package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// Identity is the cross-module user view: exactly the fields the contract
// allows a non-friend to see (SPEC-01 R28, contract §4.5). Phone, region and
// signature never leave this module through it.
type Identity struct {
	UserID        int64
	AccountName   string
	Nickname      string
	AvatarMediaID *int64
}

// FindIdentityByIdentifier resolves a user by exact phone or account_name.
// Both empty or no match → RESOURCE_UNAVAILABLE.
func (s *Service) FindIdentityByIdentifier(ctx context.Context, phone, accountName string) (Identity, error) {
	var id int64
	var err error
	switch {
	case phone != "":
		err = s.db.QueryRowContext(ctx, `SELECT id FROM users WHERE phone = ?`, phone).Scan(&id)
	case accountName != "":
		err = s.db.QueryRowContext(ctx, `SELECT id FROM users WHERE account_name = ?`, accountName).Scan(&id)
	default:
		return Identity{}, apperrors.Invalid("phone or account_name required")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, apperrors.Unavail("user not found")
	}
	if err != nil {
		return Identity{}, fmt.Errorf("user: find identity: %w", err)
	}
	return s.identityByID(ctx, id)
}

// Identities loads identities for many ids at once; unknown ids are omitted.
// Order of the result is not significant.
func (s *Service) Identities(ctx context.Context, ids []int64) (map[int64]Identity, error) {
	out := make(map[int64]Identity, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.account_name, p.nickname, p.avatar_media_id
		FROM users u JOIN user_profiles p ON p.user_id = u.id
		WHERE u.id IN (`+mysqlx.Placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("user: identities: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var it Identity
		var avatar sql.NullInt64
		if err := rows.Scan(&it.UserID, &it.AccountName, &it.Nickname, &avatar); err != nil {
			return nil, fmt.Errorf("user: scan identity: %w", err)
		}
		if avatar.Valid {
			v := avatar.Int64
			it.AvatarMediaID = &v
		}
		out[it.UserID] = it
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("user: identities rows: %w", err)
	}
	return out, nil
}

func (s *Service) identityByID(ctx context.Context, id int64) (Identity, error) {
	var it Identity
	var avatar sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.account_name, p.nickname, p.avatar_media_id
		FROM users u JOIN user_profiles p ON p.user_id = u.id
		WHERE u.id = ?`, id).Scan(&it.UserID, &it.AccountName, &it.Nickname, &avatar)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, apperrors.Unavail("user not found")
	}
	if err != nil {
		return Identity{}, fmt.Errorf("user: identity by id: %w", err)
	}
	if avatar.Valid {
		v := avatar.Int64
		it.AvatarMediaID = &v
	}
	return it, nil
}
