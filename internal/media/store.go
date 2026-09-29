package media

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// SessionRow is one upload_sessions row (R1).
type SessionRow struct {
	ID            string
	OwnerID       int64
	FileName      string
	DeclaredSize  int64
	DeclaredMIME  string
	DeclaredSHA   string
	Purpose       string
	ChunkSize     int64
	TotalChunks   int
	Status        string
	MediaObjectID int64 // 0 = none
	ExpiresAt     time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ObjectRow is one media_objects row (R10).
type ObjectRow struct {
	ID        int64
	OwnerID   int64
	BucketKey string
	Size      int64
	SHA256    string
	MIME      string
	Purpose   string
	Status    string
	CreatedAt time.Time
}

const sessionColumns = `id, owner_id, file_name, declared_size, declared_mime, declared_sha256,
	purpose, chunk_size, total_chunks, status, media_object_id, expires_at, created_at, updated_at`

func scanSession(row interface{ Scan(...any) error }) (SessionRow, error) {
	var s SessionRow
	var objectID sql.NullInt64
	err := row.Scan(&s.ID, &s.OwnerID, &s.FileName, &s.DeclaredSize, &s.DeclaredMIME, &s.DeclaredSHA,
		&s.Purpose, &s.ChunkSize, &s.TotalChunks, &s.Status, &objectID, &s.ExpiresAt, &s.CreatedAt, &s.UpdatedAt)
	if objectID.Valid {
		s.MediaObjectID = objectID.Int64
	}
	return s, err
}

// insertSession creates an open upload session (R1).
func insertSession(ctx context.Context, db mysqlx.DBTX, s SessionRow) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO upload_sessions
			(id, owner_id, file_name, declared_size, declared_mime, declared_sha256,
			 purpose, chunk_size, total_chunks, status, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, ?)`,
		s.ID, s.OwnerID, s.FileName, s.DeclaredSize, s.DeclaredMIME, s.DeclaredSHA,
		s.Purpose, s.ChunkSize, s.TotalChunks, s.ExpiresAt, s.CreatedAt, s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("media: insert session: %w", err)
	}
	return nil
}

// findSession loads a session; missing → RESOURCE_UNAVAILABLE (R4).
func findSession(ctx context.Context, db mysqlx.DBTX, id string) (SessionRow, error) {
	s, err := scanSession(db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM upload_sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return SessionRow{}, apperrors.Unavail("upload session not found")
	}
	if err != nil {
		return SessionRow{}, fmt.Errorf("media: find session: %w", err)
	}
	return s, nil
}

// findObjectByKey resolves the proxy token's key back to an object row (R16).
func findObjectByKey(ctx context.Context, db mysqlx.DBTX, key string) (ObjectRow, error) {
	var o ObjectRow
	err := db.QueryRowContext(ctx, `
		SELECT id, owner_id, bucket_key, size, sha256, mime, purpose, status, created_at
		FROM media_objects WHERE bucket_key = ?`, key).
		Scan(&o.ID, &o.OwnerID, &o.BucketKey, &o.Size, &o.SHA256, &o.MIME, &o.Purpose, &o.Status, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ObjectRow{}, apperrors.Unavail("media object not found")
	}
	if err != nil {
		return ObjectRow{}, fmt.Errorf("media: find object by key: %w", err)
	}
	return o, nil
}

// putChunk records one arrived chunk (R4). The row is upserted so a repeated
// PUT of the same part is a plain overwrite, and the storage blob below the
// row is overwritten by the caller with the same key.
func putChunk(ctx context.Context, tx mysqlx.Tx, uploadID string, n int, size int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO upload_chunks (upload_id, chunk_index, size, received_at)
		VALUES (?, ?, ?, ?) AS v
		ON DUPLICATE KEY UPDATE size = v.size, received_at = v.received_at`,
		uploadID, n, size, now)
	if err != nil {
		return fmt.Errorf("media: put chunk: %w", err)
	}
	return nil
}

// chunkBitmap returns the received part numbers, ascending (R5).
func chunkBitmap(ctx context.Context, db mysqlx.DBTX, uploadID string) ([]int, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT chunk_index FROM upload_chunks WHERE upload_id = ? ORDER BY chunk_index ASC`, uploadID)
	if err != nil {
		return nil, fmt.Errorf("media: chunk bitmap: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("media: scan bitmap: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("media: bitmap rows: %w", err)
	}
	return out, nil
}

// missingChunks reports which parts have not arrived (R6 completeness).
func missingChunks(ctx context.Context, db mysqlx.DBTX, uploadID string, total int) ([]int, error) {
	got, err := chunkBitmap(ctx, db, uploadID)
	if err != nil {
		return nil, err
	}
	seen := make(map[int]bool, len(got))
	for _, n := range got {
		seen[n] = true
	}
	var missing []int
	for n := 1; n <= total; n++ {
		if !seen[n] {
			missing = append(missing, n)
		}
	}
	return missing, nil
}

// completeSession performs the §5 idempotent transition open→completed.
// It reports whether this call is the one that completed the session.
func completeSession(ctx context.Context, tx mysqlx.Tx, id string, objectID int64, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE upload_sessions
		SET status = 'completed', media_object_id = ?, updated_at = ?
		WHERE id = ? AND status = 'open'`, objectID, now, id)
	if err != nil {
		return false, fmt.Errorf("media: complete session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("media: complete affected: %w", err)
	}
	return n == 1, nil
}

// closeSession marks a session terminal (aborted) and clears its chunks (§4).
func closeSession(ctx context.Context, tx mysqlx.Tx, id, status string, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM upload_chunks WHERE upload_id = ?`, id); err != nil {
		return fmt.Errorf("media: delete chunks: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE upload_sessions SET status = ?, updated_at = ? WHERE id = ?`, status, now, id); err != nil {
		return fmt.Errorf("media: close session: %w", err)
	}
	return nil
}

// expireStaleSessions flips open sessions past their deadline to expired (R7).
func expireStaleSessions(ctx context.Context, db mysqlx.DBTX, now time.Time) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id FROM upload_sessions WHERE status = 'open' AND expires_at < ?`, now)
	if err != nil {
		return nil, fmt.Errorf("media: stale sessions: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("media: scan stale: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("media: stale rows: %w", err)
	}
	return ids, nil
}

// insertObject writes the ready object row (R6/R10). The row only appears
// after every check has passed, so no half-baked object ever exists (§5).
func insertObject(ctx context.Context, tx mysqlx.Tx, o ObjectRow) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO media_objects
			(owner_id, bucket_key, size, sha256, mime, purpose, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'ready', ?, ?)`,
		o.OwnerID, o.BucketKey, o.Size, o.SHA256, o.MIME, o.Purpose, o.CreatedAt, o.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("media: insert object: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("media: object id: %w", err)
	}
	return id, nil
}

// findObject loads an object; missing → RESOURCE_UNAVAILABLE (R14).
func findObject(ctx context.Context, db mysqlx.DBTX, id int64) (ObjectRow, error) {
	var o ObjectRow
	err := db.QueryRowContext(ctx, `
		SELECT id, owner_id, bucket_key, size, sha256, mime, purpose, status, created_at
		FROM media_objects WHERE id = ?`, id).
		Scan(&o.ID, &o.OwnerID, &o.BucketKey, &o.Size, &o.SHA256, &o.MIME, &o.Purpose, &o.Status, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ObjectRow{}, apperrors.Unavail("media object not found")
	}
	if err != nil {
		return ObjectRow{}, fmt.Errorf("media: find object: %w", err)
	}
	return o, nil
}

// isAvatarObject reports whether any active avatar reference points at the
// object (R12 phase-1 authorization: avatars are public profile parts).
func isAvatarObject(ctx context.Context, db mysqlx.DBTX, objectID int64) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx, `
		SELECT 1 FROM media_references
		WHERE object_id = ? AND biz_type = 'avatar' LIMIT 1`, objectID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("media: avatar ref: %w", err)
	}
	return true, nil
}

// insertReference appends a business reference row (R11).
func insertReference(ctx context.Context, tx mysqlx.Tx, objectID int64, bizType, bizID string, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO media_references (object_id, biz_type, biz_id, created_at)
		VALUES (?, ?, ?, ?)`, objectID, bizType, bizID, now)
	if err != nil {
		return 0, fmt.Errorf("media: insert reference: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("media: reference id: %w", err)
	}
	return id, nil
}

// ensureUserReference grants user read access unless an active grant already
// exists (R12/R13: grants are per user and independent).
func ensureUserReference(ctx context.Context, tx mysqlx.Tx, objectID, userID, grantedBy int64, now time.Time) error {
	var one int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM media_user_references
		WHERE object_id = ? AND user_id = ? AND revoked_at IS NULL LIMIT 1`,
		objectID, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO media_user_references (object_id, user_id, granted_by_ref_id, created_at)
			VALUES (?, ?, ?, ?)`, objectID, userID, grantedBy, now)
		if err != nil {
			return fmt.Errorf("media: insert user reference: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("media: user reference: %w", err)
	}
	return nil
}
