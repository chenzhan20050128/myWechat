// Package backup owns cloud backups, restore archives and device-migration
// handshakes (spec-08). Online tables are never mutated by restore; restores
// land in read-only restored_* snapshots.
package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// BackupRow is a backups row.
type BackupRow struct {
	ID          int64
	AccountID   int64
	Status      string
	Scope       json.RawMessage
	TotalSize   int64
	PartCount   int64
	ObjectKey   string
	SHA256      string
	CreatedAt   time.Time
	CompletedAt *time.Time
	ExpiresAt   *time.Time
	FailReason  string
}

// RestoreJobRow is a backup_restore_jobs row.
type RestoreJobRow struct {
	ID         int64
	AccountID  int64
	BackupID   int64
	Status     string
	FailReason string
	StartedAt  time.Time
	FinishedAt *time.Time
}

// HandshakeRow is a transfer_handshakes row.
type HandshakeRow struct {
	ID        int64
	AccountID int64
	Code      string
	Status    string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// ProfileRow is a restored_profile_snapshots row.
type ProfileRow struct {
	ID            int64
	RestoreJobID  int64
	AccountID     int64
	Profile       json.RawMessage
	SizeBytes     int64
}

// MessageRow is a restored_message_snapshots row.
type MessageRow struct {
	ID               int64
	RestoreJobID     int64
	ConversationID   int64
	ConversationType string
	Counterparty     json.RawMessage
	Message          json.RawMessage
	OrigMessageID    int64
	OrigCreatedAt    time.Time
	SizeBytes        int64
}

func upsertSlot(ctx context.Context, tx mysqlx.Tx, accountID int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT IGNORE INTO backup_slots (account_id, updated_at) VALUES (?, ?)`, accountID, now)
	return err
}

func lockSlot(ctx context.Context, tx mysqlx.Tx, accountID int64) (activeID *int64, err error) {
	var id sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT active_backup_id FROM backup_slots WHERE account_id = ? FOR UPDATE`, accountID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if id.Valid {
		v := id.Int64
		return &v, nil
	}
	return nil, nil
}

func activeBackupCreating(ctx context.Context, tx mysqlx.Tx, accountID int64) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM backups WHERE account_id = ? AND status = 'creating' LIMIT 1`, accountID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return true, err
}

func insertBackup(ctx context.Context, tx mysqlx.Tx, accountID int64, scope json.RawMessage, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO backups (account_id, status, scope, created_at) VALUES (?, 'creating', ?, ?)`, accountID, scope, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func findBackup(ctx context.Context, db mysqlx.DBTX, id, accountID int64) (*BackupRow, error) {
	var b BackupRow
	var completed, expires sql.NullTime
	var reason sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT id, account_id, status, scope, total_size, part_count, object_key, sha256, created_at, completed_at, expires_at, COALESCE(fail_reason,'')
		FROM backups WHERE id = ? AND account_id = ?`, id, accountID).
		Scan(&b.ID, &b.AccountID, &b.Status, &b.Scope, &b.TotalSize, &b.PartCount, &b.ObjectKey, &b.SHA256, &b.CreatedAt, &completed, &expires, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperrors.Unavail("backup not found")
	}
	if completed.Valid {
		t := completed.Time
		b.CompletedAt = &t
	}
	if expires.Valid {
		t := expires.Time
		b.ExpiresAt = &t
	}
	b.FailReason = reason.String
	return &b, err
}

func findActiveBackup(ctx context.Context, db mysqlx.DBTX, accountID int64) (*BackupRow, error) {
	var activeID sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT active_backup_id FROM backup_slots WHERE account_id = ?`, accountID).Scan(&activeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !activeID.Valid {
		return nil, nil
	}
	return findBackup(ctx, db, activeID.Int64, accountID)
}

// completeBackup marks backup ready and atomically replaces active pointer.
func completeBackup(ctx context.Context, tx mysqlx.Tx, backupID, accountID, totalSize int64, now time.Time) error {
	expires := now.AddDate(0, 0, 30)
	res, err := tx.ExecContext(ctx, `
		UPDATE backups SET status = 'ready', completed_at = ?, expires_at = ?, total_size = ?
		WHERE id = ? AND account_id = ? AND status = 'creating'`, now, expires, totalSize, backupID, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apperrors.New(apperrors.StateConflict, "backup not in creating state")
	}
	_, err = tx.ExecContext(ctx, `UPDATE backup_slots SET active_backup_id = ?, updated_at = ? WHERE account_id = ?`, backupID, now, accountID)
	return err
}

func failBackup(ctx context.Context, tx mysqlx.Tx, backupID int64, reason string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE backups SET status = 'failed', fail_reason = ? WHERE id = ? AND status = 'creating'`, reason, backupID)
	return err
}

func deleteBackup(ctx context.Context, tx mysqlx.Tx, backupID, accountID int64, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE backups SET status = 'deleted', deleted_at = ? WHERE id = ? AND account_id = ? AND status <> 'deleted'`, now, backupID, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apperrors.Unavail("backup not found")
	}
	_, err = tx.ExecContext(ctx, `UPDATE backup_slots SET active_backup_id = NULL, updated_at = ? WHERE account_id = ? AND active_backup_id = ?`, now, accountID, backupID)
	return err
}

func expireReadyBackups(ctx context.Context, tx mysqlx.Tx, now time.Time) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM backups WHERE status = 'ready' AND expires_at IS NOT NULL AND expires_at <= ? FOR UPDATE SKIP LOCKED`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE backups SET status = 'expired' WHERE id = ?`, id); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE backup_slots SET active_backup_id = NULL WHERE active_backup_id = ?`, id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// --- restore ---

func insertRestoreJob(ctx context.Context, tx mysqlx.Tx, accountID, backupID int64, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO backup_restore_jobs (account_id, backup_id, status, started_at) VALUES (?, ?, 'running', ?)`, accountID, backupID, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func setBackupRestoring(ctx context.Context, tx mysqlx.Tx, backupID int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE backups SET status = 'restoring' WHERE id = ? AND status = 'ready'`, backupID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apperrors.New(apperrors.StateConflict, "backup not ready")
	}
	return nil
}

func setBackupReady(ctx context.Context, tx mysqlx.Tx, backupID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE backups SET status = 'ready' WHERE id = ? AND status = 'restoring'`, backupID)
	return err
}

func upsertStaging(ctx context.Context, tx mysqlx.Tx, jobID int64, section string, payload json.RawMessage, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO backup_restore_staging (job_id, section, payload, created_at) VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE payload = VALUES(payload)`, jobID, section, payload, now)
	return err
}

func listStaging(ctx context.Context, tx mysqlx.Tx, jobID int64) (map[string]json.RawMessage, error) {
	rows, err := tx.QueryContext(ctx, `SELECT section, payload FROM backup_restore_staging WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var sec string
		var p json.RawMessage
		if err := rows.Scan(&sec, &p); err != nil {
			return nil, err
		}
		out[sec] = p
	}
	return out, rows.Err()
}

func deleteStaging(ctx context.Context, tx mysqlx.Tx, jobID int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM backup_restore_staging WHERE job_id = ?`, jobID)
	return err
}

func completeRestoreJob(ctx context.Context, tx mysqlx.Tx, jobID int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE backup_restore_jobs SET status = 'succeeded', finished_at = ? WHERE id = ?`, now, jobID)
	return err
}

func failRestoreJob(ctx context.Context, tx mysqlx.Tx, jobID int64, reason string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE backup_restore_jobs SET status = 'failed', fail_reason = ?, finished_at = ? WHERE id = ?`, reason, now, jobID)
	return err
}

func insertProfileSnapshot(ctx context.Context, tx mysqlx.Tx, accountID, jobID int64, profile json.RawMessage, size int64, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO restored_profile_snapshots (account_id, restore_job_id, profile, size_bytes, created_at)
		VALUES (?, ?, ?, ?, ?)`, accountID, jobID, profile, size, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func findProfileSnapshot(ctx context.Context, db mysqlx.DBTX, accountID, jobID int64) (*ProfileRow, error) {
	var p ProfileRow
	err := db.QueryRowContext(ctx, `
		SELECT id, restore_job_id, account_id, profile, size_bytes
		FROM restored_profile_snapshots WHERE account_id = ? AND restore_job_id = ?`, accountID, jobID).
		Scan(&p.ID, &p.RestoreJobID, &p.AccountID, &p.Profile, &p.SizeBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperrors.Unavail("archive not found")
	}
	return &p, err
}

func listRestoreJobs(ctx context.Context, db mysqlx.DBTX, accountID int64, limit int) ([]RestoreJobRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, account_id, backup_id, status, COALESCE(fail_reason,''), started_at, finished_at
		FROM backup_restore_jobs WHERE account_id = ? ORDER BY id DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RestoreJobRow
	for rows.Next() {
		var r RestoreJobRow
		var fin sql.NullTime
		if err := rows.Scan(&r.ID, &r.AccountID, &r.BackupID, &r.Status, &r.FailReason, &r.StartedAt, &fin); err != nil {
			return nil, err
		}
		if fin.Valid {
			t := fin.Time
			r.FinishedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func deleteArchive(ctx context.Context, tx mysqlx.Tx, accountID, jobID int64) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM restored_profile_snapshots WHERE account_id = ? AND restore_job_id = ?`, accountID, jobID)
	if err != nil {
		return err
	}
	_ = res
	if _, err := tx.ExecContext(ctx, `DELETE FROM restored_message_snapshots WHERE account_id = ? AND restore_job_id = ?`, accountID, jobID); err != nil {
		return err
	}
	return nil
}

// --- handshakes ---

func insertHandshake(ctx context.Context, tx mysqlx.Tx, accountID int64, code string, now time.Time) (int64, error) {
	expires := now.Add(time.Hour)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO transfer_handshakes (account_id, source_device, target_device, code, status, expires_at, created_at)
		VALUES (?, '', '', ?, 'pending', ?, ?)`, accountID, code, expires, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func findHandshake(ctx context.Context, db mysqlx.DBTX, id, accountID int64) (*HandshakeRow, error) {
	var h HandshakeRow
	err := db.QueryRowContext(ctx, `
		SELECT id, account_id, code, status, expires_at, created_at
		FROM transfer_handshakes WHERE id = ? AND account_id = ?`, id, accountID).
		Scan(&h.ID, &h.AccountID, &h.Code, &h.Status, &h.ExpiresAt, &h.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperrors.Unavail("handshake not found")
	}
	return &h, err
}

func markHandshakeAccepted(ctx context.Context, tx mysqlx.Tx, id int64, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE transfer_handshakes SET status = 'transferring' WHERE id = ? AND status = 'pending'`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apperrors.New(apperrors.StateConflict, "handshake not pending")
	}
	return nil
}

func markHandshakeDone(ctx context.Context, tx mysqlx.Tx, id int64, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE transfer_handshakes SET status = 'done' WHERE id = ? AND status = 'transferring'`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apperrors.New(apperrors.StateConflict, "handshake not transferring")
	}
	return nil
}
