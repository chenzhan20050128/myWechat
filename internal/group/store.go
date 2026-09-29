package group

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

// GroupRow is a `groups` row (R1).
type GroupRow struct {
	ID         int64
	Name       string
	OwnerID    int64
	Announcement string
	MemberCount int
	Status     string
	ConversationID int64
	CreatedAt  time.Time
}

// MemberRow is a group_members row joined with its role.
type MemberRow struct {
	ID       int64
	GroupID  int64
	UserID   int64
	Role     string
	JoinedAt time.Time
	MutedUntil *time.Time
}

// InviteCodeRow is a group_invite_codes row.
type InviteCodeRow struct {
	ID        int64
	GroupID   int64
	Code      string
	UseCount  int
	MaxUses   int
	ExpiresAt time.Time
	CreatedAt time.Time
}

// TodoRow is a group_todos row.
type TodoRow struct {
	ID          int64
	GroupID     int64
	Title       string
	Description string
	DueAt       *time.Time
	Status      string
	CreatedBy   int64
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// ---------------------------------------------------------------------------
// Group lock + read (R6 lock order: groups row FOR UPDATE).

func findGroup(ctx context.Context, db mysqlx.DBTX, id int64) (GroupRow, error) {
	var g GroupRow
	err := db.QueryRowContext(ctx, `
		SELECT id, name, owner_id, COALESCE(announcement,''), member_count, status, conversation_id, created_at
		FROM `+"`groups`"+` WHERE id = ?`, id).Scan(
		&g.ID, &g.Name, &g.OwnerID, &g.Announcement, &g.MemberCount, &g.Status, &g.ConversationID, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return GroupRow{}, apperrors.Unavail("group not found")
	}
	if err != nil {
		return GroupRow{}, fmt.Errorf("group: find: %w", err)
	}
	return g, nil
}

func lockGroup(ctx context.Context, tx mysqlx.Tx, id int64) (GroupRow, error) {
	var g GroupRow
	err := tx.QueryRowContext(ctx, `
		SELECT id, name, owner_id, COALESCE(announcement,''), member_count, status, conversation_id, created_at
		FROM `+"`groups`"+` WHERE id = ? FOR UPDATE`, id).Scan(
		&g.ID, &g.Name, &g.OwnerID, &g.Announcement, &g.MemberCount, &g.Status, &g.ConversationID, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return GroupRow{}, apperrors.Unavail("group not found")
	}
	if err != nil {
		return GroupRow{}, fmt.Errorf("group: lock: %w", err)
	}
	return g, nil
}

func insertGroup(ctx context.Context, tx mysqlx.Tx, g GroupRow) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO `+"`groups`"+` (name, owner_id, conversation_id, member_count, status, created_at)
		VALUES (?, ?, ?, 1, 'active', ?)`, g.Name, g.OwnerID, g.ConversationID, g.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("group: insert: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func updateGroupName(ctx context.Context, tx mysqlx.Tx, id int64, name string) error {
	_, err := tx.ExecContext(ctx, `UPDATE `+"`groups`"+` SET name = ? WHERE id = ?`, name, id)
	return err
}

func updateGroupAvatar(ctx context.Context, tx mysqlx.Tx, id, avatarMediaID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE `+"`groups`"+` SET avatar_media_id = ? WHERE id = ?`, avatarMediaID, id)
	return err
}

func updateAnnouncement(ctx context.Context, tx mysqlx.Tx, id int64, content string, by int64, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE `+"`groups`"+` SET announcement = ?, announcement_updated_by = ?, announcement_updated_at = ?
		WHERE id = ?`, content, by, at, id)
	return err
}

func dissolveGroup(ctx context.Context, tx mysqlx.Tx, id int64, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE `+"`groups`"+` SET status = 'dissolved', dissolved_at = ? WHERE id = ?`, at, id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE group_members SET left_at = ?, left_reason = 'dissolved'
		WHERE group_id = ? AND left_at IS NULL`, at, id)
	return err
}

func transferOwnership(ctx context.Context, tx mysqlx.Tx, groupID, newOwnerID int64) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE group_members SET role = 'member' WHERE group_id = ? AND role = 'owner'`, groupID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE group_members SET role = 'owner' WHERE group_id = ? AND user_id = ?`, groupID, newOwnerID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE `+"`groups`"+` SET owner_id = ? WHERE id = ?`, newOwnerID, groupID)
	return err
}

// ---------------------------------------------------------------------------
// Members.

func activeMember(ctx context.Context, db mysqlx.DBTX, groupID, userID int64) (MemberRow, string, error) {
	var m MemberRow
	var mutedUntil sql.NullTime
	err := db.QueryRowContext(ctx, `
		SELECT m.id, m.group_id, m.user_id, m.role, m.joined_at, mu.until_at
		FROM group_members m
		LEFT JOIN group_mutes mu ON mu.group_id = m.group_id AND mu.user_id = m.user_id
		WHERE m.group_id = ? AND m.user_id = ? AND m.left_at IS NULL`, groupID, userID).Scan(
		&m.ID, &m.GroupID, &m.UserID, &m.Role, &m.JoinedAt, &mutedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return MemberRow{}, "", nil
	}
	if err != nil {
		return MemberRow{}, "", fmt.Errorf("group: active member: %w", err)
	}
	if mutedUntil.Valid {
		t := mutedUntil.Time
		m.MutedUntil = &t
	}
	return m, m.Role, nil
}

// insertMember adds an active member row (INSERT IGNORE relies on the
// active_flag unique key for idempotency; returns whether it was inserted).
func insertMember(ctx context.Context, tx mysqlx.Tx, groupID, userID int64, role string, at time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO group_members (group_id, user_id, role, joined_at)
		VALUES (?, ?, ?, ?)`, groupID, userID, role, at)
	if err != nil {
		return false, fmt.Errorf("group: insert member: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func countActiveMembers(ctx context.Context, tx mysqlx.Tx, groupID int64) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_members WHERE group_id = ? AND left_at IS NULL`, groupID).Scan(&n)
	return n, err
}

func countAdmins(ctx context.Context, tx mysqlx.Tx, groupID int64) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_members WHERE group_id = ? AND left_at IS NULL AND role = 'admin'`, groupID).Scan(&n)
	return n, err
}

func quitMember(ctx context.Context, tx mysqlx.Tx, groupID, userID int64, at time.Time, reason string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE group_members SET left_at = ?, left_reason = ?
		WHERE group_id = ? AND user_id = ? AND left_at IS NULL`, at, reason, groupID, userID)
	return err
}

func bumpMemberCount(ctx context.Context, tx mysqlx.Tx, groupID int64, delta int) error {
	_, err := tx.ExecContext(ctx, `UPDATE `+"`groups`"+` SET member_count = member_count + ? WHERE id = ?`, delta, groupID)
	return err
}

// ---------------------------------------------------------------------------
// Invite codes.

func insertInviteCode(ctx context.Context, tx mysqlx.Tx, groupID int64, code string, by int64, at, expires time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO group_invite_codes (group_id, code, created_by, max_uses, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, groupID, code, by, MaxInviteUses, expires, at)
	return err
}

func lockInviteCode(ctx context.Context, tx mysqlx.Tx, code string) (InviteCodeRow, error) {
	var r InviteCodeRow
	err := tx.QueryRowContext(ctx, `
		SELECT id, group_id, code, use_count, max_uses, expires_at, created_at
		FROM group_invite_codes WHERE code = ? FOR UPDATE`, code).Scan(
		&r.ID, &r.GroupID, &r.Code, &r.UseCount, &r.MaxUses, &r.ExpiresAt, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return InviteCodeRow{}, apperrors.Unavail("invalid invite code")
	}
	return r, err
}

func consumeInviteCode(ctx context.Context, tx mysqlx.Tx, id int64) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE group_invite_codes SET use_count = use_count + 1
		WHERE id = ? AND use_count < max_uses`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func recordInviteUse(ctx context.Context, tx mysqlx.Tx, codeID, userID int64, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO group_invite_uses (code_id, user_id, joined_at) VALUES (?, ?, ?)`, codeID, userID, at)
	return err
}

// recentlyRemoved checks the 24h ban window (R10 rule 3).
func recentlyRemoved(ctx context.Context, tx mysqlx.Tx, groupID, userID int64, within time.Duration, now time.Time) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM group_members
		WHERE group_id = ? AND user_id = ? AND left_reason = 'removed' AND left_at > ?
		LIMIT 1`, groupID, userID, now.Add(-within)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ---------------------------------------------------------------------------
// Mutes.

func upsertMute(ctx context.Context, tx mysqlx.Tx, groupID, userID, by int64, until *time.Time, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO group_mutes (group_id, user_id, until_at, created_by, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE until_at = VALUES(until_at), created_by = VALUES(created_by), created_at = VALUES(created_at)`,
		groupID, userID, until, by, at)
	return err
}

func deleteMute(ctx context.Context, tx mysqlx.Tx, groupID, userID int64) (bool, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM group_mutes WHERE group_id = ? AND user_id = ?`, groupID, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---------------------------------------------------------------------------
// Group events (audit trail).

func insertGroupEvent(ctx context.Context, tx mysqlx.Tx, groupID int64, event string, actor, target int64, detail map[string]any, at time.Time) error {
	payload, _ := json.Marshal(detail)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO group_events (group_id, event, actor_id, target_id, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, groupID, event, actor, nullableID(target), string(payload), at)
	return err
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// ---------------------------------------------------------------------------
// Todos.

func insertTodo(ctx context.Context, tx mysqlx.Tx, t TodoRow, at time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO group_todos (group_id, title, description, due_at, status, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'active', ?, ?, ?)`, t.GroupID, t.Title, t.Description, t.DueAt, t.CreatedBy, at, at)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func lockTodo(ctx context.Context, tx mysqlx.Tx, todoID int64) (TodoRow, error) {
	var t TodoRow
	var dueAt, completedAt sql.NullTime
	err := tx.QueryRowContext(ctx, `
		SELECT id, group_id, title, description, due_at, status, created_by, created_at, completed_at
		FROM group_todos WHERE id = ? AND deleted_at IS NULL FOR UPDATE`, todoID).Scan(
		&t.ID, &t.GroupID, &t.Title, &t.Description, &dueAt, &t.Status, &t.CreatedBy, &t.CreatedAt, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TodoRow{}, apperrors.Unavail("todo not found")
	}
	if dueAt.Valid {
		tt := dueAt.Time
		t.DueAt = &tt
	}
	if completedAt.Valid {
		tt := completedAt.Time
		t.CompletedAt = &tt
	}
	return t, nil
}

func insertAssignee(ctx context.Context, tx mysqlx.Tx, todoID, userID int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO group_todo_assignees (todo_id, user_id, status) VALUES (?, ?, 'assigned')`, todoID, userID)
	return err
}

func snapshotMembers(ctx context.Context, tx mysqlx.Tx, todoID, groupID int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO group_todo_member_snapshots (todo_id, user_id)
		SELECT ?, user_id FROM group_members WHERE group_id = ? AND left_at IS NULL`, todoID, groupID)
	return err
}

func snapshotHasUser(ctx context.Context, tx mysqlx.Tx, todoID, userID int64) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM group_todo_member_snapshots WHERE todo_id = ? AND user_id = ?`, todoID, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func addToSnapshot(ctx context.Context, tx mysqlx.Tx, todoID, userID int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO group_todo_member_snapshots (todo_id, user_id) VALUES (?, ?)`, todoID, userID)
	return err
}

// waiveAssigneesOnLeave sets assigned assignees to 'waived' when the user
// leaves the group (R23). Returns the affected todo ids.
func waiveAssigneesOnLeave(ctx context.Context, tx mysqlx.Tx, groupID, userID int64) ([]int64, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE group_todo_assignees a JOIN group_todos t ON t.id = a.todo_id
		SET a.status = 'waived'
		WHERE t.group_id = ? AND a.user_id = ? AND a.status = 'assigned'`, groupID, userID)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT t.id FROM group_todos t JOIN group_todo_assignees a ON a.todo_id = t.id
		WHERE t.group_id = ? AND a.user_id = ? AND t.deleted_at IS NULL AND t.status = 'active'`, groupID, userID)
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
	return ids, nil
}

// recomputeTodoStatus sets the aggregate status from assignee rows (R22).
func recomputeTodoStatus(ctx context.Context, tx mysqlx.Tx, todoID int64, at time.Time) error {
	var assigned, completed, waived, cancelled int
	err := tx.QueryRowContext(ctx, `
		SELECT
			SUM(status = 'assigned'), SUM(status = 'completed'),
			SUM(status = 'waived'), SUM(status = 'cancelled')
		FROM group_todo_assignees WHERE todo_id = ?`, todoID).Scan(&assigned, &completed, &waived, &cancelled)
	if err != nil {
		return err
	}
	switch {
	case assigned > 0:
		return nil // still active
	case completed > 0 && waived+completed+assigned+cancelled == completed+waived+cancelled:
		_, err = tx.ExecContext(ctx, `UPDATE group_todos SET status = 'completed', completed_at = ?, updated_at = ? WHERE id = ? AND status = 'active'`, at, at, todoID)
	case waived > 0 && completed == 0:
		_, err = tx.ExecContext(ctx, `UPDATE group_todos SET status = 'cancelled', cancelled_at = ?, updated_at = ? WHERE id = ? AND status = 'active'`, at, at, todoID)
	}
	return err
}

func completeAssignee(ctx context.Context, tx mysqlx.Tx, todoID, userID int64, at time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE group_todo_assignees SET status = 'completed', completed_at = ?
		WHERE todo_id = ? AND user_id = ? AND status = 'assigned'`, at, todoID, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func cancelTodo(ctx context.Context, tx mysqlx.Tx, todoID int64, at time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE group_todos SET status = 'cancelled', cancelled_at = ?, updated_at = ?
		WHERE id = ? AND status = 'active'`, at, at, todoID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func hideTodo(ctx context.Context, tx mysqlx.Tx, todoID int64, at time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE group_todos SET deleted_at = ? WHERE id = ?`, at, todoID)
	return err
}

func insertTodoEvent(ctx context.Context, tx mysqlx.Tx, todoID int64, event string, actor int64, detail map[string]any, at time.Time) error {
	payload, _ := json.Marshal(detail)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO group_todo_events (todo_id, event, actor_id, detail, created_at)
		VALUES (?, ?, ?, ?, ?)`, todoID, event, actor, string(payload), at)
	return err
}

// ---------------------------------------------------------------------------
// Read lists.

// MemberView is one row of GET /groups/{id}/members.
type MemberView struct {
	UserID     int64   `json:"user_id"`
	Role       string  `json:"role"`
	JoinedAt   time.Time `json:"joined_at"`
	MutedUntil *time.Time `json:"muted_until,omitempty"`
}

func listActiveMembers(ctx context.Context, db mysqlx.DBTX, groupID int64) ([]MemberView, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.user_id, m.role, m.joined_at, mu.until_at
		FROM group_members m
		LEFT JOIN group_mutes mu ON mu.group_id = m.group_id AND mu.user_id = m.user_id
		WHERE m.group_id = ? AND m.left_at IS NULL
		ORDER BY m.role = 'owner' DESC, m.joined_at ASC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberView
	for rows.Next() {
		var v MemberView
		var until sql.NullTime
		if err := rows.Scan(&v.UserID, &v.Role, &v.JoinedAt, &until); err != nil {
			return nil, err
		}
		if until.Valid {
			t := until.Time
			v.MutedUntil = &t
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// MyGroupView is one row of GET /users/me/groups.
type MyGroupView struct {
	GroupID   int64     `json:"group_id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	MemberCount int     `json:"member_count"`
}

func listMyGroups(ctx context.Context, db mysqlx.DBTX, userID int64) ([]MyGroupView, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT g.id, g.name, m.role, g.member_count
		FROM group_members m
		JOIN `+"`groups`"+` g ON g.id = m.group_id
		WHERE m.user_id = ? AND m.left_at IS NULL AND g.status = 'active'
		ORDER BY m.joined_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MyGroupView
	for rows.Next() {
		var v MyGroupView
		if err := rows.Scan(&v.GroupID, &v.Name, &v.Role, &v.MemberCount); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// findGroupByConversation resolves a group row from its conversation_id (message-side port).
func findGroupByConversation(ctx context.Context, db mysqlx.DBTX, conversationID int64) (GroupRow, error) {
	var g GroupRow
	err := db.QueryRowContext(ctx, `
		SELECT id, name, owner_id, COALESCE(announcement,''), member_count, status, conversation_id, created_at
		FROM `+"`groups`"+` WHERE conversation_id = ?`, conversationID).Scan(
		&g.ID, &g.Name, &g.OwnerID, &g.Announcement, &g.MemberCount, &g.Status, &g.ConversationID, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return GroupRow{}, apperrors.Unavail("group not found for conversation")
	}
	if err != nil {
		return GroupRow{}, fmt.Errorf("group: by conversation: %w", err)
	}
	return g, nil
}

// wasMemberAt reports whether user occupied an active membership interval covering `at` (R15).
func wasMemberAt(ctx context.Context, db mysqlx.DBTX, groupID, userID int64, at time.Time) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx, `
		SELECT 1 FROM group_members
		WHERE group_id = ? AND user_id = ? AND joined_at <= ? AND (left_at IS NULL OR left_at > ?)
		LIMIT 1`, groupID, userID, at, at).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return true, err
}

// listGroupConversationsForUser returns (conversation_id, group_id, name) for every group
// the user is currently an active member of (R19).
func listGroupConversationsForUser(ctx context.Context, db mysqlx.DBTX, userID int64) ([]GroupConvView, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT g.conversation_id, g.id, g.name
		FROM group_members m
		JOIN `+"`groups`"+` g ON g.id = m.group_id
		WHERE m.user_id = ? AND m.left_at IS NULL AND g.status = 'active'`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupConvView
	for rows.Next() {
		var v GroupConvView
		if err := rows.Scan(&v.ConversationID, &v.GroupID, &v.Name); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GroupConvView is one row of the user's active group conversations.
type GroupConvView struct {
	ConversationID int64
	GroupID        int64
	Name           string
}

// TodoView is one row of the todo list/detail responses.
type TodoView struct {
	ID          int64       `json:"id"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	DueAt       *time.Time  `json:"due_at,omitempty"`
	Status      string      `json:"status"`
	CreatedBy   int64       `json:"created_by"`
	CreatedAt   time.Time   `json:"created_at"`
	Assignees   []AssigneeView `json:"assignees"`
}

// AssigneeView is one row of group_todo_assignees.
type AssigneeView struct {
	UserID      int64      `json:"user_id"`
	Status      string     `json:"status"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

func listAssignees(ctx context.Context, db mysqlx.DBTX, todoID int64) ([]AssigneeView, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT user_id, status, completed_at FROM group_todo_assignees WHERE todo_id = ?`, todoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssigneeView
	for rows.Next() {
		var v AssigneeView
		var done sql.NullTime
		if err := rows.Scan(&v.UserID, &v.Status, &done); err != nil {
			return nil, err
		}
		if done.Valid {
			t := done.Time
			v.CompletedAt = &t
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// listVisibleTodos returns todos the caller may see (R20): moderators see
// every active todo; ordinary members see todos in whose snapshot they sit.
func listVisibleTodos(ctx context.Context, db mysqlx.DBTX, groupID, userID int64, isMod bool) ([]TodoRow, error) {
	q := `
		SELECT t.id, t.group_id, t.title, t.description, t.due_at, t.status, t.created_by, t.created_at, t.completed_at
		FROM group_todos t
		WHERE t.group_id = ? AND t.deleted_at IS NULL AND t.status IN ('active','completed')`
	args := []any{groupID}
	if !isMod {
		q += ` AND EXISTS (SELECT 1 FROM group_todo_member_snapshots s WHERE s.todo_id = t.id AND s.user_id = ?)`
		args = append(args, userID)
	}
	q += ` ORDER BY t.created_at DESC LIMIT 100`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TodoRow
	for rows.Next() {
		var t TodoRow
		var dueAt, completedAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.GroupID, &t.Title, &t.Description, &dueAt, &t.Status, &t.CreatedBy, &t.CreatedAt, &completedAt); err != nil {
			return nil, err
		}
		if dueAt.Valid {
			tt := dueAt.Time
			t.DueAt = &tt
		}
		if completedAt.Valid {
			tt := completedAt.Time
			t.CompletedAt = &tt
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// markOverdue flips active todos past due_at to 'overdue' (worker hook).
func markOverdue(ctx context.Context, db mysqlx.DBTX, at time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE group_todos SET status = 'overdue', updated_at = ?
		WHERE status = 'active' AND due_at IS NOT NULL AND due_at < ? AND deleted_at IS NULL`, at, at)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// pruneExpiredMutes removes temporary mutes whose until_at has passed. The
// DB field stays the source of truth for canSend; this is a storage-only GC.
func pruneExpiredMutes(ctx context.Context, db mysqlx.DBTX, at time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM group_mutes WHERE until_at IS NOT NULL AND until_at < ?`, at)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
