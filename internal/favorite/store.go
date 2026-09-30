package favorite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// Row is a favorites row.
type Row struct {
	ID               int64
	OwnerID          int64
	Kind             string
	Content          json.RawMessage
	SourceMessageID  *int64
	SourceSenderID   *int64
	SourceSentAt     *time.Time
	SourceType       string
	TotalSize        int64
	Status           string
	CreatedAt        time.Time
}

// TagRow is a favorite_tags row.
type TagRow struct {
	Tag       string
	CreatedAt time.Time
}

// JobRow is a storage_cleanup_jobs row.
type JobRow struct {
	ID          int64
	UserID      int64
	Scope       string
	Filter      json.RawMessage
	Preview     json.RawMessage
	Status      string
	CreatedAt   time.Time
}

// ItemRow is a storage_cleanup_items row.
type ItemRow struct {
	ItemID         int64
	CleanupID      int64
	UserRefID      int64
	MediaObjectID  int64
	State          string
	Error          string
}

func insertFavorite(ctx context.Context, tx mysqlx.Tx, r *Row, now time.Time) (int64, error) {
	var srcMsg, srcSender any
	if r.SourceMessageID != nil {
		srcMsg = *r.SourceMessageID
	}
	if r.SourceSenderID != nil {
		srcSender = *r.SourceSenderID
	}
	var srcSent any
	if r.SourceSentAt != nil {
		srcSent = *r.SourceSentAt
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO favorites (owner_id, kind, content, source_message_id, source_sender_id, source_sent_at, source_type, total_size, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.OwnerID, r.Kind, r.Content, srcMsg, srcSender, srcSent, r.SourceType, r.TotalSize, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func findActiveBySource(ctx context.Context, db mysqlx.DBTX, ownerID, sourceMessageID int64) (*Row, error) {
	r, err := scanFavoriteRow(db.QueryRowContext(ctx, `
		SELECT id, owner_id, kind, content, source_message_id, source_sender_id, source_sent_at, COALESCE(source_type,''), total_size, status, created_at
		FROM favorites WHERE owner_id = ? AND source_message_id = ? AND status = 'active' LIMIT 1`, ownerID, sourceMessageID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

func findFavorite(ctx context.Context, db mysqlx.DBTX, id int64) (*Row, error) {
	r, err := scanFavoriteRow(db.QueryRowContext(ctx, `
		SELECT id, owner_id, kind, content, source_message_id, source_sender_id, source_sent_at, COALESCE(source_type,''), total_size, status, created_at
		FROM favorites WHERE id = ? AND status = 'active'`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperrors.Unavail("favorite not found")
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

func scanFavoriteRow(row *sql.Row) (*Row, error) {
	var r Row
	var srcMsg, srcSender sql.NullInt64
	var srcSent sql.NullTime
	if err := row.Scan(&r.ID, &r.OwnerID, &r.Kind, &r.Content, &srcMsg, &srcSender, &srcSent, &r.SourceType, &r.TotalSize, &r.Status, &r.CreatedAt); err != nil {
		return nil, err
	}
	if srcMsg.Valid {
		v := srcMsg.Int64
		r.SourceMessageID = &v
	}
	if srcSender.Valid {
		v := srcSender.Int64
		r.SourceSenderID = &v
	}
	if srcSent.Valid {
		t := srcSent.Time
		r.SourceSentAt = &t
	}
	return &r, nil
}

func softDelete(ctx context.Context, tx mysqlx.Tx, id, ownerID int64, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE favorites SET status = 'deleted', deleted_at = ? WHERE id = ? AND owner_id = ? AND status = 'active'`, now, id, ownerID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apperrors.Unavail("favorite not found")
	}
	return nil
}

func listFavorites(ctx context.Context, db mysqlx.DBTX, ownerID, beforeID int64, kind, tag string, limit int) ([]Row, error) {
	query := `SELECT id, owner_id, kind, content, source_message_id, source_sender_id, source_sent_at, COALESCE(source_type,''), total_size, status, created_at
		FROM favorites WHERE owner_id = ? AND status = 'active'`
	args := []any{ownerID}
	if beforeID > 0 {
		query += ` AND id < ?`
		args = append(args, beforeID)
	}
	if kind != "" {
		query += ` AND kind = ?`
		args = append(args, kind)
	}
	if tag != "" {
		query += ` AND id IN (SELECT favorite_id FROM favorite_tag_items WHERE tag = ?)`
		args = append(args, tag)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var srcMsg, srcSender sql.NullInt64
		var srcSent sql.NullTime
		if err := rows.Scan(&r.ID, &r.OwnerID, &r.Kind, &r.Content, &srcMsg, &srcSender, &srcSent, &r.SourceType, &r.TotalSize, &r.Status, &r.CreatedAt); err != nil {
			return nil, err
		}
		if srcMsg.Valid {
			v := srcMsg.Int64
			r.SourceMessageID = &v
		}
		if srcSender.Valid {
			v := srcSender.Int64
			r.SourceSenderID = &v
		}
		if srcSent.Valid {
			t := srcSent.Time
			r.SourceSentAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func countTags(ctx context.Context, db mysqlx.DBTX, ownerID int64) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM favorite_tags WHERE owner_id = ?`, ownerID).Scan(&n)
	return n, err
}

func upsertTag(ctx context.Context, tx mysqlx.Tx, ownerID int64, tag string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT IGNORE INTO favorite_tags (owner_id, tag, created_at) VALUES (?, ?, ?)`, ownerID, tag, now)
	return err
}

func listTags(ctx context.Context, db mysqlx.DBTX, ownerID int64) ([]TagRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT tag, created_at FROM favorite_tags WHERE owner_id = ? ORDER BY tag ASC`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TagRow
	for rows.Next() {
		var t TagRow
		if err := rows.Scan(&t.Tag, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func tagExists(ctx context.Context, db mysqlx.DBTX, ownerID int64, tag string) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM favorite_tags WHERE owner_id = ? AND tag = ?`, ownerID, tag).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return true, err
}

func setTagsOnFavorite(ctx context.Context, tx mysqlx.Tx, favoriteID, ownerID int64, tags []string) error {
	for _, t := range tags {
		exists, err := tagExists(ctx, tx, ownerID, t)
		if err != nil {
			return err
		}
		if !exists {
			return apperrors.Invalid("tag does not exist: " + t)
		}
		if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO favorite_tag_items (favorite_id, tag) VALUES (?, ?)`, favoriteID, t); err != nil {
			return err
		}
	}
	return nil
}

func removeTagsFromFavorite(ctx context.Context, tx mysqlx.Tx, favoriteID int64, tags []string) error {
	for _, t := range tags {
		if _, err := tx.ExecContext(ctx, `DELETE FROM favorite_tag_items WHERE favorite_id = ? AND tag = ?`, favoriteID, t); err != nil {
			return err
		}
	}
	return nil
}

// --- cleanup jobs ---

func insertJob(ctx context.Context, tx mysqlx.Tx, j *JobRow, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO storage_cleanup_jobs (user_id, scope, filter, preview, created_at)
		VALUES (?, ?, ?, ?, ?)`, j.UserID, j.Scope, j.Filter, j.Preview, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func findJob(ctx context.Context, db mysqlx.DBTX, id, userID int64) (*JobRow, error) {
	var j JobRow
	err := db.QueryRowContext(ctx, `
		SELECT id, user_id, scope, filter, preview, status, created_at FROM storage_cleanup_jobs WHERE id = ? AND user_id = ?`, id, userID).
		Scan(&j.ID, &j.UserID, &j.Scope, &j.Filter, &j.Preview, &j.Status, &j.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperrors.Unavail("cleanup job not found")
	}
	return &j, err
}

func markJobConfirmed(ctx context.Context, tx mysqlx.Tx, id, userID int64, now time.Time) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE storage_cleanup_jobs SET status = 'confirmed', confirmed_at = ?
		WHERE id = ? AND user_id = ? AND status = 'previewed'`, now, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apperrors.Unavail("job not confirmable")
	}
	return nil
}

func markJobDone(ctx context.Context, tx mysqlx.Tx, id int64, status string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE storage_cleanup_jobs SET status = ?, finished_at = ? WHERE id = ?`, status, now, id)
	return err
}

func insertCleanupItem(ctx context.Context, tx mysqlx.Tx, j *ItemRow, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO storage_cleanup_items (cleanup_id, media_user_reference_id, media_object_id, state, updated_at)
		VALUES (?, ?, ?, 'pending', ?)`, j.CleanupID, j.UserRefID, j.MediaObjectID, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func listCleanupItems(ctx context.Context, db mysqlx.DBTX, cleanupID int64) ([]ItemRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT item_id, cleanup_id, media_user_reference_id, media_object_id, state, COALESCE(error,'')
		FROM storage_cleanup_items WHERE cleanup_id = ? ORDER BY item_id ASC`, cleanupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ItemRow
	for rows.Next() {
		var it ItemRow
		if err := rows.Scan(&it.ItemID, &it.CleanupID, &it.UserRefID, &it.MediaObjectID, &it.State, &it.Error); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func updateItemState(ctx context.Context, tx mysqlx.Tx, itemID int64, state, errMsg string, now time.Time) error {
	var errArg any
	if errMsg != "" {
		errArg = errMsg
	}
	_, err := tx.ExecContext(ctx, `UPDATE storage_cleanup_items SET state = ?, error = ?, updated_at = ? WHERE item_id = ?`, state, errArg, now, itemID)
	return err
}

// listUserReferences returns all active media_user_references for user with
// their object size. Used by the cleanup preview.
type RefSummary struct {
	RefID      int64
	ObjectID   int64
	Size       int64
	OtherUsers int
}

func listUserReferences(ctx context.Context, db mysqlx.DBTX, userID int64) ([]RefSummary, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT r.id, r.object_id, COALESCE(o.size, 0),
		       (SELECT COUNT(DISTINCT r2.user_id) FROM media_user_references r2 WHERE r2.object_id = r.object_id AND r2.revoked_at IS NULL AND r2.user_id <> r.user_id)
		FROM media_user_references r
		JOIN media_objects o ON o.id = r.object_id
		WHERE r.user_id = ? AND r.revoked_at IS NULL`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefSummary
	for rows.Next() {
		var s RefSummary
		if err := rows.Scan(&s.RefID, &s.ObjectID, &s.Size, &s.OtherUsers); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// countActiveRefsForObject returns how many users still have an active reference.
func countActiveRefsForObject(ctx context.Context, db mysqlx.DBTX, objectID int64) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_user_references WHERE object_id = ? AND revoked_at IS NULL`, objectID).Scan(&n)
	return n, err
}
