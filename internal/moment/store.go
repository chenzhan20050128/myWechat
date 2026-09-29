package moment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// MomentRow is a moments row.
type MomentRow struct {
	ID            int64
	AuthorID      int64
	Content       string
	Country       string
	Province      string
	City          string
	PlaceName     string
	AllowComments bool
	AllowLikes    bool
	Status        string
	CreatedAt     time.Time
}

// VisibilityRow is a moment_visibility_users row.
type VisibilityRow struct {
	MomentID int64
	UserID   int64
	Epoch    int64
	Allowed  bool
}

// LikeRow is a moment_likes row.
type LikeRow struct {
	MomentID int64
	UserID   int64
}

// CommentRow is a moment_comments row.
type CommentRow struct {
	ID        int64
	MomentID  int64
	UserID    int64
	ReplyTo   *int64
	Content   string
	Status    string
	CreatedAt time.Time
}

// NotificationRow is a moment_notifications row.
type NotificationRow struct {
	ID          int64
	RecipientID int64
	MomentID    int64
	Kind        string
	ActorID     int64
	CreatedAt   time.Time
	ReadAt      *time.Time
}

// ScheduleRow is a moment_schedules row.
type ScheduleRow struct {
	ID               int64
	AuthorID         int64
	RunAt            time.Time
	Status           string
	CurrentVersion   int
	ExecutionVersion int
	MomentID         *int64
	FailReason       string
	RetryCount       int
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func insertMoment(ctx context.Context, tx mysqlx.Tx, m *MomentRow, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO moments (author_id, content, country, province, city, place_name, allow_comments, allow_likes, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'visible', ?)`,
		m.AuthorID, m.Content, m.Country, m.Province, m.City, m.PlaceName,
		m.AllowComments, m.AllowLikes, now)
	if err != nil {
		return 0, fmt.Errorf("moment: insert: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func insertAsset(ctx context.Context, tx mysqlx.Tx, momentID, mediaObjectID int64, position int, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO moment_assets (moment_id, media_object_id, position, created_at) VALUES (?, ?, ?, ?)`,
		momentID, mediaObjectID, position, now)
	return err
}

func insertVisibility(ctx context.Context, tx mysqlx.Tx, rows []VisibilityRow, now time.Time) error {
	for _, r := range rows {
		allowed := 0
		if r.Allowed {
			allowed = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO moment_visibility_users (moment_id, user_id, friendship_epoch, allowed, snapshot_at)
			VALUES (?, ?, ?, ?, ?)`, r.MomentID, r.UserID, r.Epoch, allowed, now); err != nil {
			return err
		}
	}
	return nil
}

func findMoment(ctx context.Context, db mysqlx.DBTX, id int64) (MomentRow, error) {
	var m MomentRow
	var allowC, allowL int
	err := db.QueryRowContext(ctx, `
		SELECT id, author_id, content, country, province, city, place_name, allow_comments, allow_likes, status, created_at
		FROM moments WHERE id = ?`, id).Scan(
		&m.ID, &m.AuthorID, &m.Content, &m.Country, &m.Province, &m.City, &m.PlaceName,
		&allowC, &allowL, &m.Status, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MomentRow{}, apperrors.Unavail("moment not found")
	}
	if err != nil {
		return MomentRow{}, err
	}
	m.AllowComments = allowC == 1
	m.AllowLikes = allowL == 1
	return m, nil
}

func markMomentDeleted(ctx context.Context, tx mysqlx.Tx, momentID, authorID int64, now time.Time) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE moments SET status = 'deleted', deleted_at = ? WHERE id = ? AND author_id = ? AND status = 'visible'`,
		now, momentID, authorID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apperrors.Unavail("moment not found or not yours")
	}
	return nil
}

// --- Likes ---

func likeExists(ctx context.Context, db mysqlx.DBTX, momentID, userID int64) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM moment_likes WHERE moment_id = ? AND user_id = ?`, momentID, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return true, err
}

func insertLike(ctx context.Context, tx mysqlx.Tx, momentID, userID int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT IGNORE INTO moment_likes (moment_id, user_id, created_at) VALUES (?, ?, ?)`,
		momentID, userID, now)
	return err
}

func deleteLike(ctx context.Context, tx mysqlx.Tx, momentID, userID int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM moment_likes WHERE moment_id = ? AND user_id = ?`, momentID, userID)
	return err
}

func countLikes(ctx context.Context, db mysqlx.DBTX, momentID int64) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM moment_likes WHERE moment_id = ?`, momentID).Scan(&n)
	return n, err
}

// --- Comments ---

func insertComment(ctx context.Context, tx mysqlx.Tx, c *CommentRow, now time.Time) (int64, error) {
	var replyTo any
	if c.ReplyTo != nil {
		replyTo = *c.ReplyTo
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO moment_comments (moment_id, user_id, reply_to, content, status, created_at)
		VALUES (?, ?, ?, ?, 'visible', ?)`, c.MomentID, c.UserID, replyTo, c.Content, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func findComment(ctx context.Context, db mysqlx.DBTX, id int64) (CommentRow, error) {
	var c CommentRow
	var replyTo sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT id, moment_id, user_id, reply_to, content, status, created_at
		FROM moment_comments WHERE id = ?`, id).Scan(&c.ID, &c.MomentID, &c.UserID, &replyTo, &c.Content, &c.Status, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CommentRow{}, apperrors.Unavail("comment not found")
	}
	if replyTo.Valid {
		c.ReplyTo = &replyTo.Int64
	}
	return c, err
}

func listComments(ctx context.Context, db mysqlx.DBTX, momentID int64) ([]CommentRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, moment_id, user_id, reply_to, content, status, created_at
		FROM moment_comments WHERE moment_id = ? ORDER BY id ASC`, momentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommentRow
	for rows.Next() {
		var c CommentRow
		var replyTo sql.NullInt64
		if err := rows.Scan(&c.ID, &c.MomentID, &c.UserID, &replyTo, &c.Content, &c.Status, &c.CreatedAt); err != nil {
			return nil, err
		}
		if replyTo.Valid {
			c.ReplyTo = &replyTo.Int64
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func deleteComment(ctx context.Context, tx mysqlx.Tx, id int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE moment_comments SET status = 'deleted', deleted_at = ? WHERE id = ? AND status = 'visible'`, now, id)
	return err
}

// --- Notifications ---

func insertNotification(ctx context.Context, tx mysqlx.Tx, n NotificationRow, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO moment_notifications (recipient_id, moment_id, kind, actor_id, created_at)
		VALUES (?, ?, ?, ?, ?)`, n.RecipientID, n.MomentID, n.Kind, n.ActorID, now)
	return err
}

func markNotificationsRead(ctx context.Context, tx mysqlx.Tx, recipientID int64, ids []int64, all bool, now time.Time) error {
	if all {
		_, err := tx.ExecContext(ctx, `UPDATE moment_notifications SET read_at = ? WHERE recipient_id = ? AND read_at IS NULL`, now, recipientID)
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids)+2)
	args = append(args, now, recipientID)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE moment_notifications SET read_at = ? WHERE recipient_id = ? AND id IN (`+mysqlx.Placeholders(len(ids))+`) AND read_at IS NULL`,
		args...)
	return err
}

// --- Feed ---

// feedCandidates returns candidate moments for viewer (UNION of author=me OR visibility hit).
func feedCandidates(ctx context.Context, db mysqlx.DBTX, viewerID int64, beforeID int64, limit int) ([]MomentRow, error) {
	query := `
		SELECT m.id, m.author_id, m.content, m.country, m.province, m.city, m.place_name, m.allow_comments, m.allow_likes, m.status, m.created_at
		FROM moments m
		WHERE m.status = 'visible'`
	args := []any{}
	if beforeID > 0 {
		query += ` AND m.id < ?`
		args = append(args, beforeID)
	}
	query += ` AND (m.author_id = ? OR EXISTS (SELECT 1 FROM moment_visibility_users v WHERE v.moment_id = m.id AND v.user_id = ? AND v.allowed = 1))
		ORDER BY m.id DESC LIMIT ?`
	args = append(args, viewerID, viewerID, limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMoments(rows)
}

func scanMoments(rows *sql.Rows) ([]MomentRow, error) {
	var out []MomentRow
	for rows.Next() {
		var m MomentRow
		var ac, al int
		if err := rows.Scan(&m.ID, &m.AuthorID, &m.Content, &m.Country, &m.Province, &m.City, &m.PlaceName, &ac, &al, &m.Status, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.AllowComments = ac == 1
		m.AllowLikes = al == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// userMoments lists moments of author visible to the viewer by author+visibility.
func userMoments(ctx context.Context, db mysqlx.DBTX, authorID, viewerID, beforeID int64, limit int, albumOnly bool) ([]MomentRow, error) {
	q := `
		SELECT m.id, m.author_id, m.content, m.country, m.province, m.city, m.place_name, m.allow_comments, m.allow_likes, m.status, m.created_at
		FROM moments m
		WHERE m.status = 'visible' AND m.author_id = ?`
	args := []any{authorID}
	if beforeID > 0 {
		q += ` AND m.id < ?`
		args = append(args, beforeID)
	}
	if authorID != viewerID {
		q += ` AND EXISTS (SELECT 1 FROM moment_visibility_users v WHERE v.moment_id = m.id AND v.user_id = ? AND v.allowed = 1)`
		args = append(args, viewerID)
	}
	if albumOnly {
		q += ` AND EXISTS (SELECT 1 FROM moment_assets a WHERE a.moment_id = m.id)`
	}
	q += ` ORDER BY m.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMoments(rows)
}

func listAssets(ctx context.Context, db mysqlx.DBTX, momentID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT media_object_id FROM moment_assets WHERE moment_id = ? ORDER BY position ASC`, momentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// --- Schedules ---

func insertSchedule(ctx context.Context, tx mysqlx.Tx, s *ScheduleRow, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO moment_schedules (author_id, run_at, status, current_version, created_at, updated_at)
		VALUES (?, ?, 'scheduled', 1, ?, ?)`, s.AuthorID, s.RunAt, now, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func insertScheduleContent(ctx context.Context, tx mysqlx.Tx, scheduleID int64, version int, m *MomentRow) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO moment_schedule_contents (schedule_id, version, content, country, province, city, place_name)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, scheduleID, version, m.Content, m.Country, m.Province, m.City, m.PlaceName)
	return err
}

func insertScheduleAssets(ctx context.Context, tx mysqlx.Tx, scheduleID int64, version int, ids []int64, now time.Time) error {
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO moment_schedule_assets (schedule_id, version, media_object_id, position) VALUES (?, ?, ?, ?)`,
			scheduleID, version, id, i); err != nil {
			return err
		}
	}
	return nil
}

func insertScheduleVisibility(ctx context.Context, tx mysqlx.Tx, scheduleID int64, version int, rows []VisibilityRow) error {
	for _, r := range rows {
		allowed := 0
		if r.Allowed {
			allowed = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO moment_schedule_visibility_users (schedule_id, version, user_id, friendship_epoch, allowed)
			VALUES (?, ?, ?, ?, ?)`, scheduleID, version, r.UserID, r.Epoch, allowed); err != nil {
			return err
		}
	}
	return nil
}

func findSchedule(ctx context.Context, db mysqlx.DBTX, id int64) (ScheduleRow, error) {
	var s ScheduleRow
	var momentID sql.NullInt64
	var reason sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT id, author_id, run_at, status, current_version, execution_version, moment_id, COALESCE(fail_reason,''), retry_count, created_at, updated_at
		FROM moment_schedules WHERE id = ?`, id).Scan(
		&s.ID, &s.AuthorID, &s.RunAt, &s.Status, &s.CurrentVersion, &s.ExecutionVersion,
		&momentID, &reason, &s.RetryCount, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduleRow{}, apperrors.Unavail("schedule not found")
	}
	if momentID.Valid {
		s.MomentID = &momentID.Int64
	}
	s.FailReason = reason.String
	return s, err
}

// claimDueSchedules atomically leases one due scheduled row (FOR UPDATE SKIP LOCKED).
func claimDueSchedules(ctx context.Context, tx mysqlx.Tx, worker string, now time.Time, leaseTTL time.Duration) (*ScheduleRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, author_id, run_at, status, current_version, execution_version, moment_id, COALESCE(fail_reason,''), retry_count, created_at, updated_at
		FROM moment_schedules
		WHERE status = 'scheduled' AND run_at <= ?
		ORDER BY run_at ASC LIMIT 1 FOR UPDATE SKIP LOCKED`, now)
	if err != nil {
		return nil, err
	}
	var s ScheduleRow
	var momentID sql.NullInt64
	var reason sql.NullString
	if rows.Next() {
		if err := rows.Scan(&s.ID, &s.AuthorID, &s.RunAt, &s.Status, &s.CurrentVersion, &s.ExecutionVersion,
			&momentID, &reason, &s.RetryCount, &s.CreatedAt, &s.UpdatedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if momentID.Valid {
			s.MomentID = &momentID.Int64
		}
		s.FailReason = reason.String
	}
	_ = rows.Close()
	if s.ID == 0 {
		return nil, nil
	}
	leaseUntil := now.Add(leaseTTL)
	_, err = tx.ExecContext(ctx, `
		UPDATE moment_schedules SET execution_version = execution_version + 1,
		  lease_owner = ?, lease_until = ?, updated_at = ?
		WHERE id = ? AND status = 'scheduled'`, worker, leaseUntil, now, s.ID)
	if err != nil {
		return nil, err
	}
	s.ExecutionVersion++
	return &s, nil
}

func loadScheduleContent(ctx context.Context, db mysqlx.DBTX, scheduleID int64, version int) (MomentRow, []int64, []VisibilityRow, error) {
	var m MomentRow
	err := db.QueryRowContext(ctx, `
		SELECT content, country, province, city, place_name FROM moment_schedule_contents WHERE schedule_id = ? AND version = ?`,
		scheduleID, version).Scan(&m.Content, &m.Country, &m.Province, &m.City, &m.PlaceName)
	if err != nil {
		return MomentRow{}, nil, nil, err
	}
	assetRows, err := db.QueryContext(ctx, `SELECT media_object_id FROM moment_schedule_assets WHERE schedule_id = ? AND version = ? ORDER BY position ASC`, scheduleID, version)
	if err != nil {
		return MomentRow{}, nil, nil, err
	}
	defer assetRows.Close()
	var assets []int64
	for assetRows.Next() {
		var id int64
		if err := assetRows.Scan(&id); err != nil {
			return MomentRow{}, nil, nil, err
		}
		assets = append(assets, id)
	}
	visRows, err := db.QueryContext(ctx, `SELECT user_id, friendship_epoch, allowed FROM moment_schedule_visibility_users WHERE schedule_id = ? AND version = ?`, scheduleID, version)
	if err != nil {
		return MomentRow{}, nil, nil, err
	}
	defer visRows.Close()
	var vis []VisibilityRow
	for visRows.Next() {
		var v VisibilityRow
		var allowed int
		if err := visRows.Scan(&v.UserID, &v.Epoch, &allowed); err != nil {
			return MomentRow{}, nil, nil, err
		}
		v.Allowed = allowed == 1
		vis = append(vis, v)
	}
	return m, assets, vis, nil
}

func markSchedulePublished(ctx context.Context, tx mysqlx.Tx, scheduleID, momentID int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE moment_schedules SET status = 'published', moment_id = ?, published_at = ?, lease_owner = NULL, lease_until = NULL, updated_at = ?
		WHERE id = ? AND status = 'scheduled'`, momentID, now, now, scheduleID)
	return err
}

func markScheduleFailed(ctx context.Context, tx mysqlx.Tx, scheduleID int64, reason string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE moment_schedules SET status = 'failed', fail_reason = ?, retry_count = retry_count + 1, lease_owner = NULL, lease_until = NULL, updated_at = ?
		WHERE id = ?`, reason, now, scheduleID)
	return err
}

func markScheduleCancelled(ctx context.Context, tx mysqlx.Tx, scheduleID, authorID int64, now time.Time) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE moment_schedules SET status = 'cancelled', cancelled_at = ?, updated_at = ?
		WHERE id = ? AND author_id = ? AND status = 'scheduled'`, now, now, scheduleID, authorID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apperrors.Unavail("schedule not found or not cancellable")
	}
	return nil
}

func resetScheduleForRetry(ctx context.Context, tx mysqlx.Tx, scheduleID, authorID int64, now time.Time) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE moment_schedules SET status = 'scheduled', fail_reason = NULL, run_at = ?, lease_owner = NULL, lease_until = NULL, updated_at = ?
		WHERE id = ? AND author_id = ? AND status = 'failed'`, now, now, scheduleID, authorID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apperrors.Unavail("schedule not found or not in failed state")
	}
	return nil
}

func recoverStaleLeases(ctx context.Context, tx mysqlx.Tx, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE moment_schedules SET lease_owner = NULL, lease_until = NULL
		WHERE status = 'scheduled' AND lease_until IS NOT NULL AND lease_until < now()`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
