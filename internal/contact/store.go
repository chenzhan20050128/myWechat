package contact

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
	"github.com/example/wechat/internal/platform/pagination"
)

// RequestRow is a friend_requests row (R2/R3).
type RequestRow struct {
	ID          int64
	ApplicantID int64
	TargetID    int64
	Source      string
	VerifyText  string
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Tag is a contact tag owned by one user (R20).
type Tag struct {
	ID        int64
	Name      string
	CreatedAt time.Time
}

// TagRef is a tag attached to a contact in a list response (R25).
type TagRef struct {
	ID   int64
	Name string
}

const requestColumns = `id, applicant_id, target_id, source, verify_text, status, created_at, updated_at`

func scanRequest(row interface{ Scan(...any) error }) (RequestRow, error) {
	var r RequestRow
	err := row.Scan(&r.ID, &r.ApplicantID, &r.TargetID, &r.Source, &r.VerifyText,
		&r.Status, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// ---------------------------------------------------------------------------
// Friendship epochs (ADR-004/ADR-006: epochs row is the per-pair lock).

// lockEpochRow creates the pointer row if absent and locks it, serializing
// every epoch allocation and friendship mutation for the pair.
func lockEpochRow(ctx context.Context, tx mysqlx.Tx, lo, hi int64) (int64, error) {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO friendship_epochs (user_low, user_high, current_epoch)
		VALUES (?, ?, 0)
		ON DUPLICATE KEY UPDATE user_low = user_low`, lo, hi); err != nil {
		return 0, fmt.Errorf("contact: init epoch row: %w", err)
	}
	var current int64
	if err := tx.QueryRowContext(ctx, `
		SELECT current_epoch FROM friendship_epochs
		WHERE user_low = ? AND user_high = ? FOR UPDATE`, lo, hi).Scan(&current); err != nil {
		return 0, fmt.Errorf("contact: lock epoch row: %w", err)
	}
	return current, nil
}

// nextEpoch returns MAX(historical epoch)+1 for the pair. Epochs are never
// reused, so the friendships rows — not the pointer — are the source of truth
// (R13, ruling 3).
func nextEpoch(ctx context.Context, tx mysqlx.Tx, lo, hi int64) (int64, error) {
	var next int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(friendship_epoch), 0) + 1 FROM friendships
		WHERE user_low = ? AND user_high = ?`, lo, hi).Scan(&next); err != nil {
		return 0, fmt.Errorf("contact: next epoch: %w", err)
	}
	return next, nil
}

func setCurrentEpoch(ctx context.Context, tx mysqlx.Tx, lo, hi, epoch int64) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE friendship_epochs SET current_epoch = ?
		WHERE user_low = ? AND user_high = ?`, epoch, lo, hi); err != nil {
		return fmt.Errorf("contact: set current epoch: %w", err)
	}
	return nil
}

func insertFriendship(ctx context.Context, tx mysqlx.Tx, lo, hi, epoch int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO friendships (user_low, user_high, friendship_epoch, status, started_at)
		VALUES (?, ?, ?, 'active', ?)`, lo, hi, epoch, now); err != nil {
		return fmt.Errorf("contact: insert friendship: %w", err)
	}
	return nil
}

// endFriendship closes the current active epoch (R12). The pointer row is left
// pointing at the deleted epoch so "current relationship" reads as inactive.
func endFriendship(ctx context.Context, tx mysqlx.Tx, lo, hi, epoch int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE friendships SET status = 'deleted', ended_at = ?
		WHERE user_low = ? AND user_high = ? AND friendship_epoch = ? AND status = 'active'`,
		now, lo, hi, epoch); err != nil {
		return fmt.Errorf("contact: end friendship: %w", err)
	}
	return nil
}

// activeFriendship returns the current active epoch for the pair, if any (R11).
func activeFriendship(ctx context.Context, db mysqlx.DBTX, lo, hi int64) (int64, bool, error) {
	var epoch int64
	err := db.QueryRowContext(ctx, `
		SELECT f.friendship_epoch
		FROM friendships f
		JOIN friendship_epochs e
		  ON e.user_low = f.user_low AND e.user_high = f.user_high
		 AND e.current_epoch = f.friendship_epoch
		WHERE f.user_low = ? AND f.user_high = ? AND f.status = 'active'`, lo, hi).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("contact: active friendship: %w", err)
	}
	return epoch, true, nil
}

// activeFriendIDsIn returns the subset of candidates that are active friends
// of owner (R23, batch form of R11).
func activeFriendIDsIn(ctx context.Context, db mysqlx.DBTX, owner int64, candidates []int64) (map[int64]struct{}, error) {
	out := make(map[int64]struct{}, len(candidates))
	if len(candidates) == 0 {
		return out, nil
	}
	// Placeholder order: the IF projection, then the low branch, then the high
	// branch — each of the three needs its own owner argument.
	args := make([]any, 0, 2*len(candidates)+3)
	args = append(args, owner)
	args = append(args, owner)
	for _, id := range candidates {
		args = append(args, id)
	}
	args = append(args, owner)
	for _, id := range candidates {
		args = append(args, id)
	}
	ph := mysqlx.Placeholders(len(candidates))
	rows, err := db.QueryContext(ctx, `
		SELECT IF(f.user_low = ?, f.user_high, f.user_low) AS friend_id
		FROM friendships f
		JOIN friendship_epochs e
		  ON e.user_low = f.user_low AND e.user_high = f.user_high
		 AND e.current_epoch = f.friendship_epoch
		WHERE f.status = 'active'
		  AND ((f.user_low = ? AND f.user_high IN (`+ph+`))
		    OR (f.user_high = ? AND f.user_low IN (`+ph+`)))`, args...)
	if err != nil {
		return nil, fmt.Errorf("contact: active friends in: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("contact: scan active friend: %w", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("contact: active friends rows: %w", err)
	}
	return out, nil
}

// listActiveFriendIDs pages active friends by ascending friend id (R25). Both
// index branches are keyset-friendly: PK(user_low,user_high) and
// idx_friendships_high(user_high,user_low).
func listActiveFriendIDs(ctx context.Context, db mysqlx.DBTX, owner, afterID int64, limit int) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT friend_id FROM (
		  SELECT f.user_high AS friend_id
		  FROM friendships f
		  JOIN friendship_epochs e
		    ON e.user_low = f.user_low AND e.user_high = f.user_high
		   AND e.current_epoch = f.friendship_epoch
		  WHERE f.user_low = ? AND f.status = 'active' AND f.user_high > ?
		  UNION ALL
		  SELECT f.user_low AS friend_id
		  FROM friendships f
		  JOIN friendship_epochs e
		    ON e.user_low = f.user_low AND e.user_high = f.user_high
		   AND e.current_epoch = f.friendship_epoch
		  WHERE f.user_high = ? AND f.status = 'active' AND f.user_low > ?
		) t ORDER BY friend_id ASC LIMIT ?`,
		owner, afterID, owner, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("contact: list friend ids: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("contact: scan friend id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("contact: friend ids rows: %w", err)
	}
	return ids, nil
}

// ---------------------------------------------------------------------------
// Friend settings (R15-R19).

const settingsColumns = `remark, message_perm, moment_perm, moment_notify`

func loadSettings(ctx context.Context, db mysqlx.DBTX, owner int64, friendIDs []int64) (map[int64]Settings, error) {
	out := make(map[int64]Settings, len(friendIDs))
	if len(friendIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(friendIDs)+1)
	args = append(args, owner)
	for _, id := range friendIDs {
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT friend_id, `+settingsColumns+` FROM friend_settings
		WHERE owner_id = ? AND friend_id IN (`+mysqlx.Placeholders(len(friendIDs))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("contact: load settings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var s Settings
		var notify int
		if err := rows.Scan(&id, &s.Remark, &s.MessagePerm, &s.MomentPerm, &notify); err != nil {
			return nil, fmt.Errorf("contact: scan settings: %w", err)
		}
		s.MomentNotify = notify == 1
		out[id] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("contact: settings rows: %w", err)
	}
	return out, nil
}

// resetSettings restores the defaults for one direction of a new epoch (R8④).
func resetSettings(ctx context.Context, tx mysqlx.Tx, owner, friend int64, now time.Time) error {
	d := DefaultSettings()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO friend_settings
			(owner_id, friend_id, remark, message_perm, moment_perm, moment_notify, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) AS v
		ON DUPLICATE KEY UPDATE
			remark = v.remark, message_perm = v.message_perm,
			moment_perm = v.moment_perm, moment_notify = v.moment_notify,
			updated_at = v.updated_at`,
		owner, friend, d.Remark, d.MessagePerm, d.MomentPerm, b2i(d.MomentNotify), now, now); err != nil {
		return fmt.Errorf("contact: reset settings: %w", err)
	}
	return nil
}

func upsertSettings(ctx context.Context, tx mysqlx.Tx, owner, friend int64, s Settings, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO friend_settings
			(owner_id, friend_id, remark, message_perm, moment_perm, moment_notify, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) AS v
		ON DUPLICATE KEY UPDATE
			remark = v.remark, message_perm = v.message_perm,
			moment_perm = v.moment_perm, moment_notify = v.moment_notify,
			updated_at = v.updated_at`,
		owner, friend, s.Remark, s.MessagePerm, s.MomentPerm, b2i(s.MomentNotify), now, now); err != nil {
		return fmt.Errorf("contact: upsert settings: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Friend requests (R1-R10).

// upsertPendingRequest merges a repeat application into the single pending row
// (R3); the pending_guard generated column carries the partial unique key.
func upsertPendingRequest(ctx context.Context, tx mysqlx.Tx, in CreateRequestInput, targetID int64, now time.Time) (int64, error) {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO friend_requests
			(applicant_id, target_id, source, verify_text, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'pending', ?, ?) AS v
		ON DUPLICATE KEY UPDATE
			source = v.source, verify_text = v.verify_text, updated_at = v.updated_at`,
		in.ApplicantID, targetID, in.Source, in.VerifyText, now, now); err != nil {
		return 0, fmt.Errorf("contact: upsert request: %w", err)
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM friend_requests
		WHERE applicant_id = ? AND target_id = ? AND status = 'pending'`,
		in.ApplicantID, targetID).Scan(&id); err != nil {
		return 0, fmt.Errorf("contact: fetch pending request: %w", err)
	}
	return id, nil
}

// lastRejectedAt returns the most recent rejection of this direction (R6).
func lastRejectedAt(ctx context.Context, db mysqlx.DBTX, applicant, target int64) (time.Time, bool, error) {
	var at sql.NullTime
	err := db.QueryRowContext(ctx, `
		SELECT rejected_at FROM friend_requests
		WHERE applicant_id = ? AND target_id = ? AND status = 'rejected' AND rejected_at IS NOT NULL
		ORDER BY rejected_at DESC LIMIT 1`, applicant, target).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("contact: last rejection: %w", err)
	}
	return at.Time, at.Valid, nil
}

// findRequest reads a request row without locking (used to learn the pair
// before taking the epoch lock, keeping ADR-006 lock order intact).
func findRequest(ctx context.Context, db mysqlx.DBTX, id int64) (RequestRow, error) {
	r, err := scanRequest(db.QueryRowContext(ctx,
		`SELECT `+requestColumns+` FROM friend_requests WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, apperrors.Unavail("friend request not found")
	}
	if err != nil {
		return r, fmt.Errorf("contact: find request: %w", err)
	}
	return r, nil
}

// lockRequestTx locks the request row for the state transition (R8/R9).
func lockRequestTx(ctx context.Context, tx mysqlx.Tx, id int64) (RequestRow, error) {
	r, err := scanRequest(tx.QueryRowContext(ctx,
		`SELECT `+requestColumns+` FROM friend_requests WHERE id = ? FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, apperrors.Unavail("friend request not found")
	}
	if err != nil {
		return r, fmt.Errorf("contact: lock request: %w", err)
	}
	return r, nil
}

func setRequestStatus(ctx context.Context, tx mysqlx.Tx, id int64, status string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE friend_requests SET status = ?, rejected_at = ?, updated_at = ?
		WHERE id = ?`, status, rejectedAt(status, now), now, id); err != nil {
		return fmt.Errorf("contact: set request status: %w", err)
	}
	return nil
}

func rejectedAt(status string, now time.Time) any {
	if status == RequestRejected {
		return now
	}
	return nil
}

// listRequests pages one mailbox by (created_at, id) descending (R10).
func listRequests(ctx context.Context, db mysqlx.DBTX, box string, userID int64, cur pagination.Page, limit int) ([]RequestRow, error) {
	var where string
	switch box {
	case "received":
		where = "target_id = ?"
	case "sent":
		where = "applicant_id = ?"
	default:
		return nil, apperrors.Invalid("box must be received|sent")
	}
	args := []any{userID}
	keyset := ""
	if cur.ID > 0 {
		at := time.UnixMicro(cur.Sort).UTC()
		keyset = " AND (created_at < ? OR (created_at = ? AND id < ?))"
		args = append(args, at, at, cur.ID)
	}
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, `
		SELECT `+requestColumns+` FROM friend_requests
		WHERE `+where+keyset+`
		ORDER BY created_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("contact: list requests: %w", err)
	}
	defer rows.Close()
	var out []RequestRow
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("contact: scan request: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("contact: request rows: %w", err)
	}
	return out, nil
}

// expireStaleRequests implements the R2 expiry transition (driven by the
// worker in phase 2; exposed now so the rule lives with its table).
func expireStaleRequests(ctx context.Context, db mysqlx.DBTX, before, now time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE friend_requests SET status = 'expired', updated_at = ?
		WHERE status = 'pending' AND created_at < ?`, now, before)
	if err != nil {
		return 0, fmt.Errorf("contact: expire requests: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("contact: expire requests affected: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Tags (R20-R23).

func insertTag(ctx context.Context, db mysqlx.DBTX, owner int64, name string, now time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO contact_tags (owner_id, name, created_at) VALUES (?, ?, ?)`,
		owner, name, now)
	if err != nil {
		return 0, err // caller maps 1062 → STATE_CONFLICT
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("contact: tag id: %w", err)
	}
	return id, nil
}

// tagOwner returns the owner of a tag, or RESOURCE_UNAVAILABLE (R19: tags are
// invisible to anyone but their owner).
func tagOwner(ctx context.Context, db mysqlx.DBTX, tagID int64) (int64, error) {
	var owner int64
	err := db.QueryRowContext(ctx, `SELECT owner_id FROM contact_tags WHERE id = ?`, tagID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, apperrors.Unavail("tag not found")
	}
	if err != nil {
		return 0, fmt.Errorf("contact: tag owner: %w", err)
	}
	return owner, nil
}

func renameTag(ctx context.Context, db mysqlx.DBTX, tagID int64, name string) error {
	if _, err := db.ExecContext(ctx,
		`UPDATE contact_tags SET name = ? WHERE id = ?`, name, tagID); err != nil {
		return err // caller maps 1062 → STATE_CONFLICT
	}
	return nil
}

func deleteTagTx(ctx context.Context, tx mysqlx.Tx, tagID int64) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM contact_tag_members WHERE tag_id = ?`, tagID); err != nil {
		return fmt.Errorf("contact: delete tag members: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM contact_tags WHERE id = ?`, tagID); err != nil {
		return fmt.Errorf("contact: delete tag: %w", err)
	}
	return nil
}

func listTags(ctx context.Context, db mysqlx.DBTX, owner int64) ([]Tag, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, created_at FROM contact_tags WHERE owner_id = ? ORDER BY name ASC`, owner)
	if err != nil {
		return nil, fmt.Errorf("contact: list tags: %w", err)
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("contact: scan tag: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("contact: tag rows: %w", err)
	}
	return out, nil
}

func addTagMembers(ctx context.Context, tx mysqlx.Tx, tagID int64, friendIDs []int64) error {
	for _, friendID := range friendIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT IGNORE INTO contact_tag_members (tag_id, friend_id) VALUES (?, ?)`,
			tagID, friendID); err != nil {
			return fmt.Errorf("contact: add tag member: %w", err)
		}
	}
	return nil
}

func removeTagMembers(ctx context.Context, tx mysqlx.Tx, tagID int64, friendIDs []int64) error {
	if len(friendIDs) == 0 {
		return nil
	}
	args := make([]any, 0, len(friendIDs)+1)
	args = append(args, tagID)
	for _, id := range friendIDs {
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM contact_tag_members WHERE tag_id = ? AND friend_id IN (`+
			mysqlx.Placeholders(len(friendIDs))+`)`, args...); err != nil {
		return fmt.Errorf("contact: remove tag members: %w", err)
	}
	return nil
}

// loadTags groups the owner's tags by friend (R25).
func loadTags(ctx context.Context, db mysqlx.DBTX, owner int64, friendIDs []int64) (map[int64][]TagRef, error) {
	out := make(map[int64][]TagRef, len(friendIDs))
	if len(friendIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(friendIDs)+1)
	args = append(args, owner)
	for _, id := range friendIDs {
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT m.friend_id, t.id, t.name
		FROM contact_tag_members m
		JOIN contact_tags t ON t.id = m.tag_id
		WHERE t.owner_id = ? AND m.friend_id IN (`+mysqlx.Placeholders(len(friendIDs))+`)
		ORDER BY t.name ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("contact: load tags: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var friendID int64
		var ref TagRef
		if err := rows.Scan(&friendID, &ref.ID, &ref.Name); err != nil {
			return nil, fmt.Errorf("contact: scan tag ref: %w", err)
		}
		out[friendID] = append(out[friendID], ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("contact: tag ref rows: %w", err)
	}
	return out, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
