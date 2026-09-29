package content

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/example/wechat/internal/platform/mysqlx"
)

type AccountRow struct {
	ID        int64     `json:"id,string"`
	Name      string    `json:"name"`
	AvatarID  *int64    `json:"avatar_media_id,omitempty,string"`
	Intro     string    `json:"intro"`
	Status    string    `json:"status"`
	CreatorID int64     `json:"creator_id,string"`
	CreatedAt time.Time `json:"created_at"`
}

type ArticleRow struct {
	ID            int64          `json:"id,string"`
	AccountID     int64          `json:"account_id,string"`
	Title         string         `json:"title"`
	CoverID       *int64         `json:"cover_media_id,omitempty,string"`
	Summary       string         `json:"summary"`
	Body          string         `json:"body"`
	Status        string         `json:"status"`
	PublishedAt   *time.Time     `json:"published_at,omitempty"`
	UnpublishedAt *time.Time     `json:"unpublished_at,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

type MenuRow struct {
	AccountID int64  `json:"account_id,string"`
	Level     int    `json:"level"`
	ParentPos *int   `json:"parent_pos,omitempty"`
	Position  int    `json:"position"`
	Label     string `json:"label"`
	Action    string `json:"action"`
	ArticleID *int64 `json:"article_id,omitempty,string"`
}

type FollowerRow struct {
	AccountID    int64      `json:"account_id,string"`
	UserID       int64      `json:"user_id,string"`
	Muted        bool       `json:"muted"`
	FollowedAt   time.Time  `json:"followed_at"`
	UnfollowedAt *time.Time `json:"unfollowed_at,omitempty"`
}

type SessionRow struct {
	ID           int64      `json:"id,string"`
	AccountID    int64      `json:"official_account_id,string"`
	UserID       int64      `json:"user_id,string"`
	Number       int        `json:"session_number"`
	ConversationID int64    `json:"conversation_id,string"`
	Status       string     `json:"status"`
	ClosedBy     *int64     `json:"closed_by,omitempty,string"`
	ClosedAt     *time.Time `json:"closed_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

type NotificationRow struct {
	ID        int64      `json:"id,string"`
	AccountID int64      `json:"account_id,string"`
	UserID    int64      `json:"user_id,string"`
	Kind      string     `json:"kind"`
	ArticleID *int64     `json:"article_id,omitempty,string"`
	Title     string     `json:"title"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
}

type ReadRow struct {
	AccountID   int64     `json:"account_id,string"`
	ArticleID   int64     `json:"article_id,string"`
	UserID      int64     `json:"user_id,string"`
	FirstReadAt time.Time `json:"first_read_at"`
	LastReadAt  time.Time `json:"last_read_at"`
}

// --- accounts ---

func insertAccount(ctx context.Context, tx mysqlx.Tx, r *AccountRow, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO official_accounts (name, avatar_media_id, intro, status, creator_id, created_at)
		 VALUES (?, ?, ?, 'draft', ?, ?)`,
		r.Name, r.AvatarID, r.Intro, r.CreatorID, now)
	if err != nil {
		return 0, fmt.Errorf("content: insert account: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func findAccountByName(ctx context.Context, db mysqlx.DBTX, name string) (*AccountRow, error) {
	return scanAccount(db.QueryRowContext(ctx,
		`SELECT id, name, avatar_media_id, intro, status, creator_id, created_at
		 FROM official_accounts WHERE name = ?`, name))
}

func findAccount(ctx context.Context, db mysqlx.DBTX, id int64) (*AccountRow, error) {
	return scanAccount(db.QueryRowContext(ctx,
		`SELECT id, name, avatar_media_id, intro, status, creator_id, created_at
		 FROM official_accounts WHERE id = ?`, id))
}

func scanAccount(row *sql.Row) (*AccountRow, error) {
	var r AccountRow
	err := row.Scan(&r.ID, &r.Name, &r.AvatarID, &r.Intro, &r.Status, &r.CreatorID, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("content: scan account: %w", err)
	}
	return &r, nil
}

func updateAccount(ctx context.Context, tx mysqlx.Tx, id int64, name, intro string, status string, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE official_accounts SET name = ?, intro = ?, status = ? WHERE id = ?`,
		name, intro, status, id)
	if err != nil {
		return fmt.Errorf("content: update account: %w", err)
	}
	return nil
}

// --- staff ---

func isStaff(ctx context.Context, db mysqlx.DBTX, accountID, userID int64) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM official_account_staff WHERE account_id = ? AND user_id = ?`,
		accountID, userID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("content: is staff: %w", err)
	}
	return true, nil
}

// --- followers ---

func insertFollower(ctx context.Context, tx mysqlx.Tx, accountID, userID int64, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO official_followers (account_id, user_id, followed_at) VALUES (?, ?, ?)`,
		accountID, userID, now)
	if err != nil {
		return fmt.Errorf("content: insert follower: %w", err)
	}
	return nil
}

func unfollow(ctx context.Context, tx mysqlx.Tx, accountID, userID int64, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE official_followers SET unfollowed_at = ?
		 WHERE account_id = ? AND user_id = ? AND unfollowed_at IS NULL`,
		now, accountID, userID)
	if err != nil {
		return 0, fmt.Errorf("content: unfollow: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func findFollower(ctx context.Context, db mysqlx.DBTX, accountID, userID int64) (*FollowerRow, error) {
	var r FollowerRow
	err := db.QueryRowContext(ctx,
		`SELECT account_id, user_id, muted, followed_at, unfollowed_at
		 FROM official_followers
		 WHERE account_id = ? AND user_id = ? AND unfollowed_at IS NULL
		 ORDER BY followed_at DESC LIMIT 1`, accountID, userID).
		Scan(&r.AccountID, &r.UserID, &r.Muted, &r.FollowedAt, &r.UnfollowedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("content: scan follower: %w", err)
	}
	return &r, nil
}

func listActiveFollowers(ctx context.Context, db mysqlx.DBTX, accountID int64) ([]FollowerRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT account_id, user_id, muted, followed_at, unfollowed_at
		 FROM official_followers
		 WHERE account_id = ? AND unfollowed_at IS NULL`, accountID)
	if err != nil {
		return nil, fmt.Errorf("content: list followers: %w", err)
	}
	defer rows.Close()
	var out []FollowerRow
	for rows.Next() {
		var r FollowerRow
		if err := rows.Scan(&r.AccountID, &r.UserID, &r.Muted, &r.FollowedAt, &r.UnfollowedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func setMuted(ctx context.Context, tx mysqlx.Tx, accountID, userID int64, muted bool) error {
	v := 0
	if muted {
		v = 1
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE official_followers SET muted = ?
		 WHERE account_id = ? AND user_id = ? AND unfollowed_at IS NULL`,
		v, accountID, userID)
	if err != nil {
		return fmt.Errorf("content: set muted: %w", err)
	}
	return nil
}

// --- articles ---

func insertArticle(ctx context.Context, tx mysqlx.Tx, r *ArticleRow, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO official_articles (account_id, title, cover_media_id, summary, body, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 'draft', ?, ?)`,
		r.AccountID, r.Title, r.CoverID, r.Summary, r.Body, now, now)
	if err != nil {
		return 0, fmt.Errorf("content: insert article: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func findArticle(ctx context.Context, db mysqlx.DBTX, id int64) (*ArticleRow, error) {
	var r ArticleRow
	err := db.QueryRowContext(ctx,
		`SELECT id, account_id, title, cover_media_id, summary, body, status, published_at, unpublished_at, created_at, updated_at
		 FROM official_articles WHERE id = ?`, id).
		Scan(&r.ID, &r.AccountID, &r.Title, &r.CoverID, &r.Summary, &r.Body, &r.Status, &r.PublishedAt, &r.UnpublishedAt, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("content: scan article: %w", err)
	}
	return &r, nil
}

func updateDraftArticle(ctx context.Context, tx mysqlx.Tx, id int64, title, summary, body string, cover *int64, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE official_articles SET title = ?, summary = ?, body = ?, cover_media_id = ?, updated_at = ?
		 WHERE id = ? AND status = 'draft'`,
		title, summary, body, cover, now, id)
	if err != nil {
		return fmt.Errorf("content: update draft article: %w", err)
	}
	return nil
}

func publishArticle(ctx context.Context, tx mysqlx.Tx, id int64, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE official_articles SET status = 'published', published_at = ?, updated_at = ?
		 WHERE id = ? AND status = 'draft'`, now, now, id)
	if err != nil {
		return 0, fmt.Errorf("content: publish article: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func unpublishArticle(ctx context.Context, tx mysqlx.Tx, id int64, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE official_articles SET status = 'unpublished', unpublished_at = ?, updated_at = ?
		 WHERE id = ? AND status = 'published'`, now, now, id)
	if err != nil {
		return 0, fmt.Errorf("content: unpublish article: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func listPublishedArticles(ctx context.Context, db mysqlx.DBTX, accountID, beforeID int64, limit int) ([]ArticleRow, error) {
	q := `SELECT id, account_id, title, cover_media_id, summary, body, status, published_at, unpublished_at, created_at, updated_at
	      FROM official_articles
	      WHERE account_id = ? AND status = 'published'`
	args := []any{accountID}
	if beforeID > 0 {
		q += ` AND id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("content: list articles: %w", err)
	}
	defer rows.Close()
	var out []ArticleRow
	for rows.Next() {
		var r ArticleRow
		if err := rows.Scan(&r.ID, &r.AccountID, &r.Title, &r.CoverID, &r.Summary, &r.Body, &r.Status, &r.PublishedAt, &r.UnpublishedAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- article reads ---

func upsertArticleRead(ctx context.Context, tx mysqlx.Tx, accountID, articleID, userID int64, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO official_article_reads (account_id, article_id, user_id, first_read_at, last_read_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE last_read_at = VALUES(last_read_at)`,
		accountID, articleID, userID, now, now)
	if err != nil {
		return fmt.Errorf("content: upsert read: %w", err)
	}
	return nil
}

// --- menus ---

func replaceMenu(ctx context.Context, tx mysqlx.Tx, accountID int64, items []MenuRow, now time.Time) error {
	if len(items) == 0 {
		_, err := tx.ExecContext(ctx, `DELETE FROM official_menus WHERE account_id = ?`, accountID)
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM official_menus WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	for _, it := range items {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO official_menus (account_id, level, parent_pos, position, label, action, article_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			accountID, it.Level, it.ParentPos, it.Position, it.Label, it.Action, it.ArticleID)
		if err != nil {
			return fmt.Errorf("content: insert menu: %w", err)
		}
	}
	return nil
}

func listMenu(ctx context.Context, db mysqlx.DBTX, accountID int64) ([]MenuRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT account_id, level, parent_pos, position, label, action, article_id
		 FROM official_menus WHERE account_id = ? ORDER BY level, position`, accountID)
	if err != nil {
		return nil, fmt.Errorf("content: list menu: %w", err)
	}
	defer rows.Close()
	var out []MenuRow
	for rows.Next() {
		var r MenuRow
		if err := rows.Scan(&r.AccountID, &r.Level, &r.ParentPos, &r.Position, &r.Label, &r.Action, &r.ArticleID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- notifications ---

func insertNotification(ctx context.Context, tx mysqlx.Tx, r *NotificationRow, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO official_notifications (account_id, user_id, kind, article_id, title, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.AccountID, r.UserID, r.Kind, r.ArticleID, r.Title, now)
	if err != nil {
		return 0, fmt.Errorf("content: insert notification: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func countArticleNotificationsToday(ctx context.Context, db mysqlx.DBTX, accountID, userID int64, dayStart time.Time) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM official_notifications
		 WHERE account_id = ? AND user_id = ? AND kind = 'article' AND created_at >= ?`,
		accountID, userID, dayStart).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("content: count notifications today: %w", err)
	}
	return n, nil
}

func countExistingArticleNotifications(ctx context.Context, db mysqlx.DBTX, accountID, articleID int64) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM official_notifications
		 WHERE account_id = ? AND article_id = ? AND kind = 'article'`,
		accountID, articleID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("content: count existing article notifications: %w", err)
	}
	return n, nil
}

func listNotifications(ctx context.Context, db mysqlx.DBTX, userID, beforeID int64, limit int) ([]NotificationRow, error) {
	q := `SELECT id, account_id, user_id, kind, article_id, title, created_at, read_at
	      FROM official_notifications WHERE user_id = ?`
	args := []any{userID}
	if beforeID > 0 {
		q += ` AND id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("content: list notifications: %w", err)
	}
	defer rows.Close()
	var out []NotificationRow
	for rows.Next() {
		var r NotificationRow
		if err := rows.Scan(&r.ID, &r.AccountID, &r.UserID, &r.Kind, &r.ArticleID, &r.Title, &r.CreatedAt, &r.ReadAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func markNotificationsRead(ctx context.Context, tx mysqlx.Tx, userID int64, ids []int64, all bool, now time.Time) error {
	if all {
		_, err := tx.ExecContext(ctx,
			`UPDATE official_notifications SET read_at = ? WHERE user_id = ? AND read_at IS NULL`,
			now, userID)
		return err
	}
	for _, id := range ids {
		_, err := tx.ExecContext(ctx,
			`UPDATE official_notifications SET read_at = ? WHERE id = ? AND user_id = ? AND read_at IS NULL`,
			now, id, userID)
		if err != nil {
			return err
		}
	}
	return nil
}

// --- service sessions ---

func findActiveSession(ctx context.Context, db mysqlx.DBTX, accountID, userID int64) (*SessionRow, error) {
	var r SessionRow
	err := db.QueryRowContext(ctx,
		`SELECT id, official_account_id, user_id, session_number, conversation_id, status, closed_by, closed_at, created_at
		 FROM service_sessions
		 WHERE official_account_id = ? AND user_id = ? AND status = 'active'
		 ORDER BY id DESC LIMIT 1`, accountID, userID).
		Scan(&r.ID, &r.AccountID, &r.UserID, &r.Number, &r.ConversationID, &r.Status, &r.ClosedBy, &r.ClosedAt, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("content: scan active session: %w", err)
	}
	return &r, nil
}

func findSession(ctx context.Context, db mysqlx.DBTX, id int64) (*SessionRow, error) {
	var r SessionRow
	err := db.QueryRowContext(ctx,
		`SELECT id, official_account_id, user_id, session_number, conversation_id, status, closed_by, closed_at, created_at
		 FROM service_sessions WHERE id = ?`, id).
		Scan(&r.ID, &r.AccountID, &r.UserID, &r.Number, &r.ConversationID, &r.Status, &r.ClosedBy, &r.ClosedAt, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("content: scan session: %w", err)
	}
	return &r, nil
}

func findSessionByConversation(ctx context.Context, db mysqlx.DBTX, conversationID int64) (*SessionRow, error) {
	var r SessionRow
	err := db.QueryRowContext(ctx,
		`SELECT id, official_account_id, user_id, session_number, conversation_id, status, closed_by, closed_at, created_at
		 FROM service_sessions WHERE conversation_id = ? ORDER BY id DESC LIMIT 1`, conversationID).
		Scan(&r.ID, &r.AccountID, &r.UserID, &r.Number, &r.ConversationID, &r.Status, &r.ClosedBy, &r.ClosedAt, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("content: scan session by conv: %w", err)
	}
	return &r, nil
}

func insertSession(ctx context.Context, tx mysqlx.Tx, accountID, userID int64, number int, conversationID int64, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO service_sessions (official_account_id, user_id, session_number, conversation_id, status, created_at)
		 VALUES (?, ?, ?, ?, 'active', ?)`,
		accountID, userID, number, conversationID, now)
	if err != nil {
		return 0, fmt.Errorf("content: insert session: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func maxSessionNumber(ctx context.Context, tx mysqlx.Tx, accountID, userID int64) (int, error) {
	var n sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT MAX(session_number) FROM service_sessions WHERE official_account_id = ? AND user_id = ? FOR UPDATE`,
		accountID, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("content: max session number: %w", err)
	}
	if !n.Valid {
		return 0, nil
	}
	return int(n.Int64), nil
}

func closeSession(ctx context.Context, tx mysqlx.Tx, id, closerID int64, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE service_sessions SET status = 'closed', closed_by = ?, closed_at = ?
		 WHERE id = ? AND status = 'active'`,
		closerID, now, id)
	if err != nil {
		return fmt.Errorf("content: close session: %w", err)
	}
	return nil
}

func listStaffSessions(ctx context.Context, db mysqlx.DBTX, accountID int64, status string, beforeID int64, limit int) ([]SessionRow, error) {
	q := `SELECT id, official_account_id, user_id, session_number, conversation_id, status, closed_by, closed_at, created_at
	      FROM service_sessions WHERE official_account_id = ?`
	args := []any{accountID}
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
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("content: list staff sessions: %w", err)
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(&r.ID, &r.AccountID, &r.UserID, &r.Number, &r.ConversationID, &r.Status, &r.ClosedBy, &r.ClosedAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- staff assignment ---

func assignedStaff(ctx context.Context, db mysqlx.DBTX, sessionID int64) (int64, error) {
	var staffID int64
	err := db.QueryRowContext(ctx,
		`SELECT staff_id FROM service_session_staff WHERE session_id = ?`, sessionID).Scan(&staffID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("content: assigned staff: %w", err)
	}
	return staffID, nil
}

func assignStaff(ctx context.Context, tx mysqlx.Tx, sessionID, staffID int64, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT IGNORE INTO service_session_staff (session_id, staff_id, assigned_at) VALUES (?, ?, ?)`,
		sessionID, staffID, now)
	if err != nil {
		return fmt.Errorf("content: assign staff: %w", err)
	}
	return nil
}

// --- events ---

func insertSessionEvent(ctx context.Context, tx mysqlx.Tx, sessionID, actorID int64, event string, detail json.RawMessage, now time.Time) error {
	if detail == nil {
		detail = json.RawMessage(`{}`)
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO service_session_events (session_id, event, actor_id, detail, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		sessionID, event, actorID, detail, now)
	if err != nil {
		return fmt.Errorf("content: insert session event: %w", err)
	}
	return nil
}
