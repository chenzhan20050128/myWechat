// Package conversation owns conversations/conversation_members tables.
// Phase-1 slice: creation of direct/transfer conversations and membership
// checks only (SPEC-01 §2.7, ADR-010). Message flow arrives in phase 2.
package conversation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"time"

	"github.com/example/wechat/internal/platform/clock"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// Service creates conversations and answers membership questions.
type Service struct {
	db  *sql.DB
	now clock.Clock
}

// New builds the conversation service.
func New(db *sql.DB, c clock.Clock) *Service { return &Service{db: db, now: c} }

// CreateTransferTx creates (or returns) the account's unique transfer
// conversation. Idempotent via the transfer_owner_id unique key (R31).
func (s *Service) CreateTransferTx(ctx context.Context, tx mysqlx.Tx, userID int64) (int64, error) {
	now := s.now.Now()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO conversations (type, transfer_owner_id, created_at)
		VALUES ('transfer', ?, ?)
		ON DUPLICATE KEY UPDATE transfer_owner_id = transfer_owner_id`,
		userID, now)
	if err != nil {
		return 0, fmt.Errorf("conversation: create transfer: %w", err)
	}
	var id int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM conversations WHERE transfer_owner_id = ?`, userID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("conversation: fetch transfer: %w", err)
	}
	if err := s.addMemberTx(ctx, tx, id, userID, now); err != nil {
		return 0, err
	}
	return id, nil
}

// CreateDirectTx creates (or returns) the 1:1 conversation between a and b.
// The direct_key unique key deduplicates concurrent/sequential creation (R32).
func (s *Service) CreateDirectTx(ctx context.Context, tx mysqlx.Tx, a, b int64) (int64, error) {
	if a == b {
		return 0, fmt.Errorf("conversation: direct members must differ")
	}
	key := directKey(a, b)
	now := s.now.Now()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO conversations (type, direct_key, created_at)
		VALUES ('direct', ?, ?)
		ON DUPLICATE KEY UPDATE direct_key = direct_key`, key, now)
	if err != nil {
		return 0, fmt.Errorf("conversation: create direct: %w", err)
	}
	var id int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM conversations WHERE direct_key = ?`, key).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("conversation: fetch direct: %w", err)
	}
	if err := s.addMemberTx(ctx, tx, id, a, now); err != nil {
		return 0, err
	}
	if err := s.addMemberTx(ctx, tx, id, b, now); err != nil {
		return 0, err
	}
	return id, nil
}

// CreateGroupTx creates a type='group' conversation and adds the creator as
// its first member (SPEC-05 R1). Direct INSERT — no dedup key needed because
// group conversations are created exactly once per group row.
func (s *Service) CreateGroupTx(ctx context.Context, tx mysqlx.Tx, creatorID int64) (int64, error) {
	now := s.now.Now()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO conversations (type, created_at) VALUES ('group', ?)`, now)
	if err != nil {
		return 0, fmt.Errorf("conversation: create group: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("conversation: group last id: %w", err)
	}
	if err := s.addMemberTx(ctx, tx, id, creatorID, now); err != nil {
		return 0, err
	}
	return id, nil
}

// CreateOfficialServiceTx creates a type='official_service' conversation with
// the user as a member. The staff side is linked via service_session_staff at
// read time, so only the user is recorded as a conversation_members row here
// (SPEC-09 R10).
func (s *Service) CreateOfficialServiceTx(ctx context.Context, tx mysqlx.Tx, userID int64) (int64, error) {
	now := s.now.Now()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO conversations (type, created_at) VALUES ('official_service', ?)`, now)
	if err != nil {
		return 0, fmt.Errorf("conversation: create official_service: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("conversation: official_service last id: %w", err)
	}
	if err := s.addMemberTx(ctx, tx, id, userID, now); err != nil {
		return 0, err
	}
	return id, nil
}

// IsMember reports whether user is a current member (left_at IS NULL) of the
// conversation (R33).
func (s *Service) IsMember(ctx context.Context, conversationID, userID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM conversation_members
		WHERE conversation_id = ? AND user_id = ? AND left_at IS NULL
		LIMIT 1`, conversationID, userID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("conversation: is member: %w", err)
	}
	return true, nil
}

func (s *Service) addMemberTx(ctx context.Context, tx mysqlx.Tx, conversationID, userID int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO conversation_members
			(conversation_id, user_id, membership_epoch, joined_at)
		VALUES (?, ?, 1, ?)`,
		conversationID, userID, now)
	if err != nil {
		return fmt.Errorf("conversation: add member: %w", err)
	}
	return nil
}

// directKey = SHA256("lo:hi") decimal string, 32-byte binary. Same formula as
// the migration comment so SQL-side debugging reproduces the key.
func directKey(a, b int64) []byte {
	lo, hi := a, b
	if lo > hi {
		lo, hi = hi, lo
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", lo, hi)))
	return sum[:]
}
