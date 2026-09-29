package message

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// MessageRow is a `messages` row.
type MessageRow struct {
	ID             int64
	ConversationID int64
	ConversationSeq uint64
	SenderID       int64
	SenderType     string
	ClientMsgID    string
	Type           string
	Payload        json.RawMessage
	Status         string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	RecalledAt     *time.Time
}

// AssetRow is a message_assets row.
type AssetRow struct {
	ID            int64
	MessageID     int64
	MediaObjectID int64
	Kind          string
}

// SettingsRow is a conversation_settings row.
type SettingsRow struct {
	ConversationID  int64
	UserID          int64
	Pinned          bool
	Muted           bool
	Background      string
	LastReadSeq     uint64
	IsMarkedUnread  bool
	UnreadAnchorSeq uint64
	UpdatedAt       time.Time
}

// ---------------------------------------------------------------------------
// Idempotency + seq allocation (R1/R2).

// findByClientMsg returns the existing row for (sender, client_msg_id), if any.
func findByClientMsg(ctx context.Context, db mysqlx.DBTX, senderID int64, clientMsgID string) (MessageRow, error) {
	var r MessageRow
	err := db.QueryRowContext(ctx, `
		SELECT id, conversation_id, conversation_seq, sender_id, sender_type, client_msg_id,
		       type, payload, status, created_at, expires_at, recalled_at
		FROM messages WHERE sender_id = ? AND client_msg_id = ?`, senderID, clientMsgID).Scan(
		&r.ID, &r.ConversationID, &r.ConversationSeq, &r.SenderID, &r.SenderType, &r.ClientMsgID,
		&r.Type, &r.Payload, &r.Status, &r.CreatedAt, &r.ExpiresAt, &r.RecalledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MessageRow{}, nil
	}
	if err != nil {
		return MessageRow{}, fmt.Errorf("message: find by client id: %w", err)
	}
	return r, nil
}

// lockConversation returns (last_seq, type) for FOR UPDATE allocation.
func lockConversation(ctx context.Context, tx mysqlx.Tx, conversationID int64) (lastSeq uint64, convType string, err error) {
	err = tx.QueryRowContext(ctx, `
		SELECT last_seq, type FROM conversations WHERE id = ? FOR UPDATE`, conversationID).Scan(&lastSeq, &convType)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", apperrors.Unavail("conversation not found")
	}
	if err != nil {
		return 0, "", fmt.Errorf("message: lock conversation: %w", err)
	}
	return lastSeq, convType, nil
}

// bumpConversationSeq writes the new last_seq.
func bumpConversationSeq(ctx context.Context, tx mysqlx.Tx, conversationID int64, seq uint64) error {
	_, err := tx.ExecContext(ctx, `UPDATE conversations SET last_seq = ? WHERE id = ?`, seq, conversationID)
	return err
}

// insertMessage writes one row and returns its id.
func insertMessage(ctx context.Context, tx mysqlx.Tx, r *MessageRow) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO messages
		  (conversation_id, conversation_seq, sender_id, sender_type, client_msg_id, type, payload, status, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ConversationID, r.ConversationSeq, r.SenderID, r.SenderType, r.ClientMsgID, r.Type,
		r.Payload, r.Status, r.CreatedAt, r.ExpiresAt)
	if err != nil {
		return 0, fmt.Errorf("message: insert: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// insertAsset writes one asset row.
func insertAsset(ctx context.Context, tx mysqlx.Tx, messageID, mediaObjectID int64, kind string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO message_assets (message_id, media_object_id, kind, created_at) VALUES (?, ?, ?, ?)`,
		messageID, mediaObjectID, kind, at)
	return err
}

// insertReference writes the reference-reply snapshot row (R10).
func insertReference(ctx context.Context, tx mysqlx.Tx, msgID, refMsgID, refSenderID int64, refType, digest string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO message_references (message_id, ref_message_id, ref_sender_id, ref_type, ref_digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, msgID, refMsgID, refSenderID, refType, digest, at)
	return err
}

// insertForward records the source→target trace (R11).
func insertForward(ctx context.Context, tx mysqlx.Tx, sourceMsgID, targetMsgID, by int64, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO message_forwards (source_message_id, target_message_id, forwarded_by, created_at)
		VALUES (?, ?, ?, ?)`, sourceMsgID, targetMsgID, by, at)
	return err
}

// ---------------------------------------------------------------------------
// Read path (R14/R15/R16).

func scanMessageRows(rows *sql.Rows) ([]MessageRow, error) {
	defer rows.Close()
	var out []MessageRow
	for rows.Next() {
		var r MessageRow
		if err := rows.Scan(&r.ID, &r.ConversationID, &r.ConversationSeq, &r.SenderID, &r.SenderType,
			&r.ClientMsgID, &r.Type, &r.Payload, &r.Status, &r.CreatedAt, &r.ExpiresAt, &r.RecalledAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// listMessagesAfter returns messages with seq > afterSeq (ascending), bounded.
func listMessagesAfter(ctx context.Context, db mysqlx.DBTX, conversationID int64, afterSeq uint64, limit int) ([]MessageRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, conversation_id, conversation_seq, sender_id, sender_type, client_msg_id,
		       type, payload, status, created_at, expires_at, recalled_at
		FROM messages
		WHERE conversation_id = ? AND conversation_seq > ?
		ORDER BY conversation_seq ASC LIMIT ?`, conversationID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	return scanMessageRows(rows)
}

// listMessagesBefore returns the latest `limit` messages with seq < beforeSeq (descending, reversed).
// beforeSeq=0 means "no upper bound" — fetch the newest page.
func listMessagesBefore(ctx context.Context, db mysqlx.DBTX, conversationID int64, beforeSeq uint64, limit int) ([]MessageRow, error) {
	q := `
		SELECT id, conversation_id, conversation_seq, sender_id, sender_type, client_msg_id,
		       type, payload, status, created_at, expires_at, recalled_at
		FROM messages WHERE conversation_id = ?`
	args := []any{conversationID}
	if beforeSeq != 0 {
		q += ` AND conversation_seq < ?`
		args = append(args, beforeSeq)
	}
	q += ` ORDER BY conversation_seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	rs, err := scanMessageRows(rows)
	if err != nil {
		return nil, err
	}
	// Reverse to ascending.
	for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
		rs[i], rs[j] = rs[j], rs[i]
	}
	return rs, nil
}

// getMessage fetches one row by id.
func getMessage(ctx context.Context, db mysqlx.DBTX, id int64) (MessageRow, error) {
	var r MessageRow
	err := db.QueryRowContext(ctx, `
		SELECT id, conversation_id, conversation_seq, sender_id, sender_type, client_msg_id,
		       type, payload, status, created_at, expires_at, recalled_at
		FROM messages WHERE id = ?`, id).Scan(
		&r.ID, &r.ConversationID, &r.ConversationSeq, &r.SenderID, &r.SenderType, &r.ClientMsgID,
		&r.Type, &r.Payload, &r.Status, &r.CreatedAt, &r.ExpiresAt, &r.RecalledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MessageRow{}, apperrors.Unavail("message not found")
	}
	return r, err
}

// minUsableSeq returns the smallest seq in the conversation whose expiry is in the future
// (R16). Returns (0, nil) when there are no live messages.
func minUsableSeq(ctx context.Context, db mysqlx.DBTX, conversationID int64, now time.Time) (uint64, error) {
	var seq uint64
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(MIN(conversation_seq), 0) FROM messages
		WHERE conversation_id = ? AND expires_at > ?`, conversationID, now).Scan(&seq)
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// ---------------------------------------------------------------------------
// Recall / delivered (R9/R22).

// recallMessage conditionally marks a message recalled. rows=0 means it cannot be recalled.
func recallMessage(ctx context.Context, tx mysqlx.Tx, msgID, senderID int64, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE messages
		SET status = 'recalled', recalled_at = ?
		WHERE id = ? AND sender_id = ? AND status IN ('stored','delivered')
		  AND created_at > ?`, now, msgID, senderID, now.Add(-RecallWindow))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// markDelivered conditionally flips stored→delivered (R22). Idempotent.
func markDelivered(ctx context.Context, db mysqlx.DBTX, msgID int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE messages SET status = 'delivered' WHERE id = ? AND status = 'stored'`, msgID)
	return err
}

// ---------------------------------------------------------------------------
// Conversation settings (R13). Lazy UPSERT, never read-first.

func upsertSettings(ctx context.Context, tx mysqlx.Tx, s *SettingsRow) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO conversation_settings
		  (conversation_id, user_id, pinned, muted, background, last_read_seq, is_marked_unread, unread_anchor_seq, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
		  pinned = VALUES(pinned), muted = VALUES(muted), background = VALUES(background),
		  updated_at = VALUES(updated_at)`,
		s.ConversationID, s.UserID, s.Pinned, s.Muted, s.Background, s.LastReadSeq, s.IsMarkedUnread, s.UnreadAnchorSeq, s.UpdatedAt)
	return err
}

// loadSettings returns the row (or a zero-value SettingsRow if not yet written).
func loadSettings(ctx context.Context, db mysqlx.DBTX, conversationID, userID int64) (SettingsRow, error) {
	var s SettingsRow
	err := db.QueryRowContext(ctx, `
		SELECT conversation_id, user_id, pinned, muted, background, last_read_seq, is_marked_unread, unread_anchor_seq, updated_at
		FROM conversation_settings WHERE conversation_id = ? AND user_id = ?`, conversationID, userID).Scan(
		&s.ConversationID, &s.UserID, &s.Pinned, &s.Muted, &s.Background, &s.LastReadSeq, &s.IsMarkedUnread, &s.UnreadAnchorSeq, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SettingsRow{ConversationID: conversationID, UserID: userID}, nil
	}
	return s, err
}

// advanceReadSeq clamps last_read_seq to max(current, next) and clears is_marked_unread (R13).
func advanceReadSeq(ctx context.Context, tx mysqlx.Tx, conversationID, userID int64, next uint64, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO conversation_settings (conversation_id, user_id, last_read_seq, is_marked_unread, updated_at)
		VALUES (?, ?, ?, 0, ?)
		ON DUPLICATE KEY UPDATE
		  last_read_seq = GREATEST(last_read_seq, VALUES(last_read_seq)),
		  is_marked_unread = 0,
		  updated_at = VALUES(updated_at)`, conversationID, userID, next, at)
	return err
}

// markUnread records the current last_seq as the unread anchor (R13); never touches last_read_seq.
func markUnread(ctx context.Context, tx mysqlx.Tx, conversationID, userID int64, anchorSeq uint64, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO conversation_settings (conversation_id, user_id, is_marked_unread, unread_anchor_seq, updated_at)
		VALUES (?, ?, 1, ?, ?)
		ON DUPLICATE KEY UPDATE
		  is_marked_unread = 1, unread_anchor_seq = VALUES(unread_anchor_seq), updated_at = VALUES(updated_at)`,
		conversationID, userID, anchorSeq, at)
	return err
}

// countUnread returns R18 unread count for one conversation.
func countUnread(ctx context.Context, db mysqlx.DBTX, conversationID, userID int64, lastReadSeq uint64) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM messages
		WHERE conversation_id = ?
		  AND conversation_seq > ?
		  AND status IN ('stored','delivered')
		  AND sender_id <> ?`, conversationID, lastReadSeq, userID).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------
// Pins (R12).

// pinMessage INSERT IGNORE; returns false if already pinned.
func pinMessage(ctx context.Context, tx mysqlx.Tx, conversationID, messageID, by int64, at time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO message_pins (conversation_id, message_id, pinned_by, created_at) VALUES (?, ?, ?, ?)`,
		conversationID, messageID, by, at)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// countPins returns current pin count for the conversation.
func countPins(ctx context.Context, tx mysqlx.Tx, conversationID int64) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_pins WHERE conversation_id = ?`, conversationID).Scan(&n)
	return n, err
}

// unpinMessage deletes one pin row.
func unpinMessage(ctx context.Context, tx mysqlx.Tx, conversationID, messageID int64) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM message_pins WHERE conversation_id = ? AND message_id = ?`, conversationID, messageID)
	return err
}

// listPins returns pinned message ids newest-first.
func listPins(ctx context.Context, db mysqlx.DBTX, conversationID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT message_id FROM message_pins WHERE conversation_id = ? ORDER BY created_at DESC`, conversationID)
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

// ---------------------------------------------------------------------------
// Retention worker (R26).

// sweepExpired pulls one batch of stale messages for the worker.
func sweepExpired(ctx context.Context, tx mysqlx.Tx, now time.Time, batch int) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM messages
		WHERE status IN ('stored','delivered') AND expires_at <= ?
		LIMIT ? FOR UPDATE SKIP LOCKED`, now, batch)
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
	return ids, rows.Err()
}

// expireMessage flips status to 'expired'.
func expireMessage(ctx context.Context, tx mysqlx.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE messages SET status = 'expired' WHERE id = ?`, id)
	return err
}

// assetsForMessage returns all media object ids referenced by one message.
func assetsForMessage(ctx context.Context, tx mysqlx.Tx, messageID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT media_object_id FROM message_assets WHERE message_id = ?`, messageID)
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

// deleteAssets drops the message_assets rows for one message (R26).
func deleteAssets(ctx context.Context, tx mysqlx.Tx, messageID int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM message_assets WHERE message_id = ?`, messageID)
	return err
}

// ---------------------------------------------------------------------------
// Read-side helpers for views (A2/A3/A12).

// ReferenceRow is one message_references row.
type ReferenceRow struct {
	MessageID   int64
	RefMsgID    int64
	RefSenderID int64
	RefType     string
	RefDigest   string
}

// loadReference returns the reference snapshot (if any) for a message.
func loadReference(ctx context.Context, db mysqlx.DBTX, messageID int64) (ReferenceRow, error) {
	var r ReferenceRow
	err := db.QueryRowContext(ctx, `
		SELECT message_id, ref_message_id, ref_sender_id, ref_type, ref_digest
		FROM message_references WHERE message_id = ?`, messageID).Scan(
		&r.MessageID, &r.RefMsgID, &r.RefSenderID, &r.RefType, &r.RefDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return ReferenceRow{}, nil
	}
	return r, err
}

// assetRowsForMessage returns all asset rows for one message.
func assetRowsForMessage(ctx context.Context, db mysqlx.DBTX, messageID int64) ([]AssetRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, message_id, media_object_id, kind FROM message_assets WHERE message_id = ?`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssetRow
	for rows.Next() {
		var a AssetRow
		if err := rows.Scan(&a.ID, &a.MessageID, &a.MediaObjectID, &a.Kind); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// pinnedMessageView is one element of A12.
type pinnedMessageView struct {
	MsgID       int64
	Payload     json.RawMessage
	MsgType     string
	Status      string
	SenderID    int64
	Seq         uint64
	CreatedAt   time.Time
}

// listPinnedMessages loads pinned rows joined to their message bodies.
func listPinnedMessages(ctx context.Context, db mysqlx.DBTX, conversationID int64) ([]pinnedMessageView, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.payload, m.type, m.status, m.sender_id, m.conversation_seq, m.created_at
		FROM message_pins p JOIN messages m ON m.id = p.message_id
		WHERE p.conversation_id = ? ORDER BY p.created_at DESC`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pinnedMessageView
	for rows.Next() {
		var v pinnedMessageView
		if err := rows.Scan(&v.MsgID, &v.Payload, &v.MsgType, &v.Status, &v.SenderID, &v.Seq, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// conversationMemberID returns the other member of a direct conversation row.
// The conversations table itself stores only the binary key; the roster lives
// in conversation_members. We resolve by listing members and picking the one
// that is not `me`.
func otherDirectMember(ctx context.Context, db mysqlx.DBTX, conversationID, me int64) (int64, error) {
	var other int64
	err := db.QueryRowContext(ctx, `
		SELECT user_id FROM conversation_members
		WHERE conversation_id = ? AND user_id <> ? AND left_at IS NULL LIMIT 1`, conversationID, me).Scan(&other)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, apperrors.Unavail("direct partner not found")
	}
	return other, err
}

// transferOwner returns the owner for a transfer conversation (R5).
func transferOwner(ctx context.Context, db mysqlx.DBTX, conversationID int64) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `
		SELECT transfer_owner_id FROM conversations WHERE id = ? AND type = 'transfer'`, conversationID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, apperrors.Unavail("transfer conversation not found")
	}
	return id, err
}

// conversationType returns the type column of a conversation.
func conversationType(ctx context.Context, db mysqlx.DBTX, conversationID int64) (string, error) {
	var t string
	err := db.QueryRowContext(ctx, `SELECT type FROM conversations WHERE id = ?`, conversationID).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return "", apperrors.Unavail("conversation not found")
	}
	return t, err
}

// lastLiveMessage returns the newest non-expired message in a conversation (R19 digest).
func lastLiveMessage(ctx context.Context, db mysqlx.DBTX, conversationID int64) (MessageRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, conversation_id, conversation_seq, sender_id, sender_type, client_msg_id,
		       type, payload, status, created_at, expires_at, recalled_at
		FROM messages
		WHERE conversation_id = ? AND status <> 'expired'
		ORDER BY conversation_seq DESC LIMIT 1`, conversationID)
	if err != nil {
		return MessageRow{}, err
	}
	rs, err := scanMessageRows(rows)
	if err != nil {
		return MessageRow{}, err
	}
	if len(rs) == 0 {
		return MessageRow{}, nil
	}
	return rs[0], nil
}

// listDirectConversationsForUser returns conversation ids where the user is
// a current member of a direct/transfer conversation (R19).
func listDirectConversationsForUser(ctx context.Context, db mysqlx.DBTX, userID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT cm.conversation_id
		FROM conversation_members cm
		JOIN conversations c ON c.id = cm.conversation_id
		WHERE cm.user_id = ? AND cm.left_at IS NULL AND c.type IN ('direct','transfer')`, userID)
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

// loadSettingsBatch loads my settings for a batch of conversations (A3).
func loadSettingsBatch(ctx context.Context, db mysqlx.DBTX, userID int64, conversationIDs []int64) (map[int64]SettingsRow, error) {
	out := make(map[int64]SettingsRow, len(conversationIDs))
	if len(conversationIDs) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT conversation_id, user_id, pinned, muted, background, last_read_seq, is_marked_unread, unread_anchor_seq, updated_at
		FROM conversation_settings
		WHERE user_id = ? AND conversation_id IN (`+mysqlx.Placeholders(len(conversationIDs))+`)`,
		append([]any{userID}, intsToAny(conversationIDs)...))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s SettingsRow
		if err := rows.Scan(&s.ConversationID, &s.UserID, &s.Pinned, &s.Muted, &s.Background,
			&s.LastReadSeq, &s.IsMarkedUnread, &s.UnreadAnchorSeq, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out[s.ConversationID] = s
	}
	return out, rows.Err()
}

func intsToAny(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
