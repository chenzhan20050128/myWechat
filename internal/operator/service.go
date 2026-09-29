// Package operator owns admin identity, reports, moderation actions and
// read-only operational stats (SPEC-11). It never mutates business state
// directly except through domain ports.
package operator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// Identity decides whether a user is an operator.
type Identity interface {
	IsOperator(ctx context.Context, userID int64) bool
}

// Service is the operator domain service.
type Service struct {
	db       *sql.DB
	identity Identity
	now      func() time.Time
}

func New(db *sql.DB, id Identity, now func() time.Time) *Service {
	return &Service{db: db, identity: id, now: now}
}

// ReportRow is one user-submitted report.
type ReportRow struct {
	ID         int64      `json:"id,string"`
	ReporterID int64      `json:"reporter_id,string"`
	TargetType string     `json:"target_type"`
	TargetID   int64      `json:"target_id,string"`
	Reason     string     `json:"reason"`
	Note       string     `json:"note"`
	Status     string     `json:"status"`
	HandledBy  *int64     `json:"handled_by,omitempty,string"`
	HandledAt  *time.Time `json:"handled_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

var validReasons = map[string]bool{"spam": true, "abuse": true, "porn": true, "fraud": true, "other": true}
var validTargets = map[string]bool{"user": true, "message": true, "moment": true, "group_todo": true, "article": true, "service_message": true}

// SubmitReport is G1. Idempotent on (reporter, target_type, target_id).
func (s *Service) SubmitReport(ctx context.Context, reporterID int64, targetType string, targetID int64, reason, note string) (int64, error) {
	if !validTargets[targetType] {
		return 0, apperrors.Invalid("invalid target_type")
	}
	if !validReasons[reason] {
		return 0, apperrors.Invalid("invalid reason")
	}
	if targetID <= 0 {
		return 0, apperrors.Invalid("invalid target_id")
	}
	now := s.now()
	var id int64
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO reports (reporter_id, target_type, target_id, reason, note, status, created_at)
			 VALUES (?, ?, ?, ?, ?, 'pending', ?)`,
			reporterID, targetType, targetID, reason, note, now)
		if err != nil {
			if mysqlx.IsDuplicate(err, "uk_reports_dedupe") {
				return tx.QueryRowContext(ctx,
					`SELECT id FROM reports WHERE reporter_id = ? AND target_type = ? AND target_id = ?`,
					reporterID, targetType, targetID).Scan(&id)
			}
			return err
		}
		id, _ = res.LastInsertId()
		return nil
	})
	return id, err
}

// ListReports is G3. Operators only.
func (s *Service) ListReports(ctx context.Context, operatorID int64, status string, beforeID int64, limit int) ([]ReportRow, error) {
	if !s.identity.IsOperator(ctx, operatorID) {
		return nil, apperrors.New(apperrors.Forbidden, "operator only")
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := `SELECT id, reporter_id, target_type, target_id, reason, note, status, handled_by, handled_at, created_at
	      FROM reports WHERE 1=1`
	args := []any{}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if beforeID > 0 {
		q += ` AND id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReportRow
	for rows.Next() {
		var r ReportRow
		if err := rows.Scan(&r.ID, &r.ReporterID, &r.TargetType, &r.TargetID, &r.Reason, &r.Note, &r.Status, &r.HandledBy, &r.HandledAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveReport is G4.
func (s *Service) ResolveReport(ctx context.Context, operatorID, reportID int64, decision string) error {
	if !s.identity.IsOperator(ctx, operatorID) {
		return apperrors.New(apperrors.Forbidden, "operator only")
	}
	if decision != "confirmed" && decision != "rejected" {
		return apperrors.Invalid("decision must be confirmed|rejected")
	}
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM reports WHERE id = ?`, reportID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return apperrors.Unavail("report not found")
		}
		if err != nil {
			return err
		}
		if status != "pending" {
			return apperrors.New(apperrors.StateConflict, "report already handled")
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE reports SET status = ?, handled_by = ?, handled_at = ? WHERE id = ?`,
			decision, operatorID, now, reportID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO moderation_actions (operator_id, action, target_type, target_id, created_at)
			 VALUES (?, ?, 'report', ?, ?)`,
			operatorID, "resolve_"+decision, reportID, now)
		return err
	})
}

// QueueStats is R10.
type QueueStats struct {
	OutboxPending int `json:"outbox_pending"`
	RetryPending  int `json:"retry_pending"`
	DeadLetters   int `json:"dead_letters"`
}

func (s *Service) QueueStats(ctx context.Context, operatorID int64) (*QueueStats, error) {
	if !s.identity.IsOperator(ctx, operatorID) {
		return nil, apperrors.New(apperrors.Forbidden, "operator only")
	}
	qs := &QueueStats{}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox_events WHERE status = 'pending'`).Scan(&qs.OutboxPending); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM async_retry_tasks WHERE status = 'pending'`).Scan(&qs.RetryPending); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM async_dead_letters`).Scan(&qs.DeadLetters); err != nil {
		return nil, err
	}
	return qs, nil
}

// DeadLetterRow is one row in async_dead_letters.
type DeadLetterRow struct {
	EventID   string    `json:"event_id"`
	Consumer  string    `json:"consumer_name"`
	LastError string    `json:"last_error"`
	FailedAt  time.Time `json:"failed_at"`
}

func (s *Service) ListDeadLetters(ctx context.Context, operatorID int64, limit int) ([]DeadLetterRow, error) {
	if !s.identity.IsOperator(ctx, operatorID) {
		return nil, apperrors.New(apperrors.Forbidden, "operator only")
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_id, consumer_name, last_error, failed_at FROM async_dead_letters ORDER BY failed_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeadLetterRow
	for rows.Next() {
		var d DeadLetterRow
		if err := rows.Scan(&d.EventID, &d.Consumer, &d.LastError, &d.FailedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ReplayDeadLetter re-arms a retry row so the Scheduler picks it up (R18).
func (s *Service) ReplayDeadLetter(ctx context.Context, operatorID int64, eventID, consumer string) error {
	if !s.identity.IsOperator(ctx, operatorID) {
		return apperrors.New(apperrors.Forbidden, "operator only")
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO async_retry_tasks (consumer_name, event_id, attempt_count, next_attempt_at, last_error, status, created_at, updated_at)
		 VALUES (?, ?, 0, ?, 'replayed', 'pending', ?, ?)
		 ON DUPLICATE KEY UPDATE attempt_count = 0, next_attempt_at = VALUES(next_attempt_at), status = 'pending', updated_at = VALUES(updated_at)`,
		consumer, eventID, now, now, now)
	if err != nil {
		return fmt.Errorf("operator: replay: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apperrors.Unavail("dead letter not found")
	}
	return nil
}
