package content

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// Operator decides whether a user may administer content accounts (R1).
type Operator interface {
	IsOperator(ctx context.Context, userID int64) bool
}

// Conversation creates official_service conversations inside a tx (R10).
type Conversation interface {
	CreateOfficialServiceTx(ctx context.Context, tx mysqlx.Tx, userID int64) (int64, error)
}

// Outbox publishes domain events for downstream workers.
type Outbox interface {
	Emit(ctx context.Context, tx mysqlx.Tx, eventType string, aggregateID int64, queue string) error
}

// Service is the content domain service.
type Service struct {
	db           *sql.DB
	operator     Operator
	conversation Conversation
	outbox       Outbox
	now          func() time.Time
}

func New(db *sql.DB, op Operator, conv Conversation, ob Outbox, now func() time.Time) *Service {
	return &Service{db: db, operator: op, conversation: conv, outbox: ob, now: now}
}

// --- accounts (R1, R2) ---

type CreateAccountInput struct {
	Name     string
	Intro    string
	AvatarID *int64
	OperatorID int64
}

func (s *Service) CreateAccount(ctx context.Context, in *CreateAccountInput) (int64, error) {
	if !s.operator.IsOperator(ctx, in.OperatorID) {
		return 0, apperrors.New(apperrors.Forbidden, "operator only")
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 64 {
		return 0, apperrors.Invalid("name required (<=64 chars)")
	}
	if len(in.Intro) > 500 {
		return 0, apperrors.Invalid("intro too long")
	}
	now := s.now()
	r := &AccountRow{Name: in.Name, Intro: in.Intro, AvatarID: in.AvatarID, CreatorID: in.OperatorID}
	var id int64
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		var err error
		id, err = insertAccount(ctx, tx, r, now)
		return err
	})
	if err != nil {
		if mysqlx.IsDuplicate(err, "uk_oaccounts_name") {
			return 0, apperrors.New(apperrors.StateConflict, "name already taken")
		}
		return 0, err
	}
	return id, nil
}

type UpdateAccountInput struct {
	ID         int64
	OperatorID int64
	Name       string
	Intro      string
	Status     string
}

func (s *Service) UpdateAccount(ctx context.Context, in *UpdateAccountInput) error {
	if !s.operator.IsOperator(ctx, in.OperatorID) {
		return apperrors.New(apperrors.Forbidden, "operator only")
	}
	if in.Status != "" && in.Status != "draft" && in.Status != "active" && in.Status != "frozen" && in.Status != "closed" {
		return apperrors.Invalid("invalid status")
	}
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		acc, err := findAccount(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		if acc == nil {
			return apperrors.Unavail("account not found")
		}
		name := in.Name
		if name == "" {
			name = acc.Name
		}
		intro := in.Intro
		if intro == "" {
			intro = acc.Intro
		}
		status := in.Status
		if status == "" {
			status = acc.Status
		}
		return updateAccount(ctx, tx, in.ID, name, intro, status, now)
	})
}

// GetAccount returns an account visible to userID: non-active accounts are
// invisible (R1, R2). Operators always see their own/any account.
func (s *Service) GetAccount(ctx context.Context, userID, accountID int64) (*AccountRow, []MenuRow, *FollowerRow, error) {
	acc, err := findAccount(ctx, s.db, accountID)
	if err != nil {
		return nil, nil, nil, err
	}
	if acc == nil {
		return nil, nil, nil, apperrors.Unavail("account not found")
	}
	if acc.Status != "active" && !(s.operator.IsOperator(ctx, userID) && acc.CreatorID == userID) {
		return nil, nil, nil, apperrors.Unavail("account not found")
	}
	menu, err := listMenu(ctx, s.db, accountID)
	if err != nil {
		return nil, nil, nil, err
	}
	follow, err := findFollower(ctx, s.db, accountID, userID)
	if err != nil {
		return nil, nil, nil, err
	}
	return acc, menu, follow, nil
}

func (s *Service) LookupAccount(ctx context.Context, name string) (*AccountRow, error) {
	acc, err := findAccountByName(ctx, s.db, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	if acc == nil || acc.Status != "active" {
		return nil, apperrors.Unavail("account not found")
	}
	return acc, nil
}

// --- follow (R3) ---

func (s *Service) Follow(ctx context.Context, accountID, userID int64) error {
	acc, err := findAccount(ctx, s.db, accountID)
	if err != nil {
		return err
	}
	if acc == nil || acc.Status != "active" {
		return apperrors.Unavail("account not found")
	}
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		existing, err := findFollower(ctx, tx, accountID, userID)
		if err != nil {
			return err
		}
		if existing != nil {
			return nil
		}
		if err := insertFollower(ctx, tx, accountID, userID, now); err != nil {
			return err
		}
		welcomeID, err := insertNotification(ctx, tx, &NotificationRow{
			AccountID: accountID,
			UserID:    userID,
			Kind:      "welcome",
			Title:     "感谢关注 " + acc.Name,
		}, now)
		if err != nil {
			return err
		}
		if s.outbox != nil {
			if err := s.outbox.Emit(ctx, tx, "official.notification.created", welcomeID, "wechat.push"); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) Unfollow(ctx context.Context, accountID, userID int64) error {
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		_, err := unfollow(ctx, tx, accountID, userID, now)
		return err
	})
}

func (s *Service) SetMute(ctx context.Context, accountID, userID int64, muted bool) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return setMuted(ctx, tx, accountID, userID, muted)
	})
}

// --- articles (R4, R5) ---

type CreateArticleInput struct {
	OperatorID int64
	AccountID  int64
	Title      string
	Summary    string
	Body       string
	CoverID    *int64
}

func (s *Service) CreateArticle(ctx context.Context, in *CreateArticleInput) (int64, error) {
	if !s.operator.IsOperator(ctx, in.OperatorID) {
		return 0, apperrors.New(apperrors.Forbidden, "operator only")
	}
	if in.Title == "" || len(in.Title) > 128 {
		return 0, apperrors.Invalid("title required (<=128)")
	}
	if len(in.Summary) > 512 {
		return 0, apperrors.Invalid("summary too long")
	}
	if len(in.Body) > 20000 {
		return 0, apperrors.Invalid("body too long")
	}
	now := s.now()
	r := &ArticleRow{AccountID: in.AccountID, Title: in.Title, Summary: in.Summary, Body: in.Body, CoverID: in.CoverID}
	var id int64
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		acc, err := findAccount(ctx, tx, in.AccountID)
		if err != nil {
			return err
		}
		if acc == nil {
			return apperrors.Unavail("account not found")
		}
		id, err = insertArticle(ctx, tx, r, now)
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

type UpdateArticleInput struct {
	OperatorID int64
	ID         int64
	Title      string
	Summary    string
	Body       string
	CoverID    *int64
}

func (s *Service) UpdateArticle(ctx context.Context, in *UpdateArticleInput) error {
	if !s.operator.IsOperator(ctx, in.OperatorID) {
		return apperrors.New(apperrors.Forbidden, "operator only")
	}
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		art, err := findArticle(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		if art == nil {
			return apperrors.Unavail("article not found")
		}
		if art.Status != "draft" {
			return apperrors.New(apperrors.StateConflict, "only draft articles can be edited")
		}
		title := in.Title
		if title == "" {
			title = art.Title
		}
		summary := in.Summary
		if summary == "" {
			summary = art.Summary
		}
		body := in.Body
		if body == "" {
			body = art.Body
		}
		cover := in.CoverID
		if cover == nil {
			cover = art.CoverID
		}
		return updateDraftArticle(ctx, tx, in.ID, title, summary, body, cover, now)
	})
}

// PublishArticle flips draft→published and emits a fanout event (R4, R15, I3).
func (s *Service) PublishArticle(ctx context.Context, operatorID, articleID int64) error {
	if !s.operator.IsOperator(ctx, operatorID) {
		return apperrors.New(apperrors.Forbidden, "operator only")
	}
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		art, err := findArticle(ctx, tx, articleID)
		if err != nil {
			return err
		}
		if art == nil {
			return apperrors.Unavail("article not found")
		}
		n, err := publishArticle(ctx, tx, articleID, now)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		if s.outbox != nil {
			if err := s.outbox.Emit(ctx, tx, "official.article.published", articleID, "wechat.article.notify"); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) UnpublishArticle(ctx context.Context, operatorID, articleID int64) error {
	if !s.operator.IsOperator(ctx, operatorID) {
		return apperrors.New(apperrors.Forbidden, "operator only")
	}
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		n, err := unpublishArticle(ctx, tx, articleID, now)
		if err != nil {
			return err
		}
		if n == 0 {
			return apperrors.New(apperrors.StateConflict, "article not published")
		}
		return nil
	})
}

// ListPublishedArticles returns published articles for an active account.
func (s *Service) ListPublishedArticles(ctx context.Context, accountID, beforeID int64, limit int) ([]ArticleRow, error) {
	acc, err := findAccount(ctx, s.db, accountID)
	if err != nil {
		return nil, err
	}
	if acc == nil || acc.Status != "active" {
		return nil, apperrors.Unavail("account not found")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	return listPublishedArticles(ctx, s.db, accountID, beforeID, limit)
}

// GetArticle returns the article body only if visible (R5). On success it
// upserts the read record (R6).
func (s *Service) GetArticle(ctx context.Context, userID, articleID int64) (*ArticleRow, error) {
	var art *ArticleRow
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		var err error
		art, err = findArticle(ctx, tx, articleID)
		if err != nil {
			return err
		}
		if art == nil {
			return apperrors.New(apperrors.ContentUnavailable, "article unavailable")
		}
		acc, err := findAccount(ctx, tx, art.AccountID)
		if err != nil {
			return err
		}
		if acc == nil || acc.Status != "active" || art.Status != "published" {
			return apperrors.New(apperrors.ContentUnavailable, "article unavailable")
		}
		return upsertArticleRead(ctx, tx, art.AccountID, art.ID, userID, s.now())
	})
	return art, err
}

// --- menus (R7-R9) ---

type MenuInput struct {
	Level     int    `json:"level"`
	ParentPos *int   `json:"parent_pos"`
	Position  int    `json:"position"`
	Label     string `json:"label"`
	Action    string `json:"action"`
	ArticleID *int64 `json:"article_id"`
}

func (s *Service) SaveMenu(ctx context.Context, operatorID, accountID int64, items []MenuInput) error {
	if !s.operator.IsOperator(ctx, operatorID) {
		return apperrors.New(apperrors.Forbidden, "operator only")
	}
	level1, level2 := 0, 0
	byLevel1 := map[int][]MenuInput{}
	for _, it := range items {
		if it.Level == 1 {
			level1++
			byLevel1[it.Position] = append(byLevel1[it.Position], it)
		} else if it.Level == 2 {
			level2++
		} else {
			return apperrors.Invalid("menu level must be 1 or 2")
		}
		if it.Label == "" || len(it.Label) > 32 {
			return apperrors.Invalid("menu label required (<=32)")
		}
		if it.Action != "open_article" && it.Action != "start_service" {
			return apperrors.Invalid("invalid menu action")
		}
		if it.Action == "open_article" && it.ArticleID == nil {
			return apperrors.Invalid("open_article requires article_id")
		}
	}
	if level1 > 3 {
		return apperrors.Invalid("at most 3 top-level menus")
	}
	if level2 > 15 {
		return apperrors.Invalid("at most 15 sub-menus")
	}
	for pos, children := range byLevel1 {
		if len(children) > 5 {
			return apperrors.Invalid("at most 5 sub-menus per top-level")
		}
		_ = pos
	}
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		acc, err := findAccount(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if acc == nil {
			return apperrors.Unavail("account not found")
		}
		rows := make([]MenuRow, 0, len(items))
		for _, it := range items {
			if it.Action == "open_article" {
				art, err := findArticle(ctx, tx, *it.ArticleID)
				if err != nil {
					return err
				}
				if art == nil || art.AccountID != accountID {
					return apperrors.Invalid("menu article not found")
				}
				if art.Status != "published" {
					return apperrors.Invalid("menu must point to a published article")
				}
			}
			row := MenuRow{
				AccountID: accountID,
				Level:     it.Level,
				ParentPos: it.ParentPos,
				Position:  it.Position,
				Label:     it.Label,
				Action:    it.Action,
				ArticleID: it.ArticleID,
			}
			rows = append(rows, row)
		}
		return replaceMenu(ctx, tx, accountID, rows, now)
	})
}

// --- service sessions (R10-R14) ---

type StartSessionResult struct {
	ID             int64 `json:"id,string"`
	Number         int   `json:"session_number"`
	ConversationID int64 `json:"conversation_id,string"`
	Created        bool  `json:"created"`
}

func (s *Service) StartSession(ctx context.Context, accountID, userID int64) (*StartSessionResult, error) {
	acc, err := findAccount(ctx, s.db, accountID)
	if err != nil {
		return nil, err
	}
	if acc == nil || acc.Status != "active" {
		return nil, apperrors.Unavail("account not found")
	}
	follow, err := findFollower(ctx, s.db, accountID, userID)
	if err != nil {
		return nil, err
	}
	if follow == nil {
		return nil, apperrors.New(apperrors.Forbidden, "must follow account first")
	}
	now := s.now()
	var result *StartSessionResult
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		existing, err := findActiveSession(ctx, tx, accountID, userID)
		if err != nil {
			return err
		}
		if existing != nil {
			result = &StartSessionResult{ID: existing.ID, Number: existing.Number, ConversationID: existing.ConversationID, Created: false}
			return nil
		}
		maxN, err := maxSessionNumber(ctx, tx, accountID, userID)
		if err != nil {
			return err
		}
		conversationID, err := s.conversation.CreateOfficialServiceTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		sessionID, err := insertSession(ctx, tx, accountID, userID, maxN+1, conversationID, now)
		if err != nil {
			return err
		}
		detail, _ := json.Marshal(map[string]any{"account_id": accountID, "user_id": userID})
		if err := insertSessionEvent(ctx, tx, sessionID, userID, "created", detail, now); err != nil {
			return err
		}
		result = &StartSessionResult{ID: sessionID, Number: maxN + 1, ConversationID: conversationID, Created: true}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Service) CloseSession(ctx context.Context, sessionID, closerID int64) error {
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		sess, err := findSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if sess == nil {
			return apperrors.Unavail("session not found")
		}
		isUser := sess.UserID == closerID
		staff, err := isStaff(ctx, tx, sess.AccountID, closerID)
		if err != nil {
			return err
		}
		if !isUser && !staff {
			return apperrors.New(apperrors.Forbidden, "not a participant")
		}
		if sess.Status == "closed" {
			return nil
		}
		if err := closeSession(ctx, tx, sessionID, closerID, now); err != nil {
			return err
		}
		detail, _ := json.Marshal(map[string]any{"by": closerID})
		return insertSessionEvent(ctx, tx, sessionID, closerID, "closed", detail, now)
	})
}

// CanSendServiceMessage implements the message module's R6 callback for
// official_service conversations. senderType is "user" or "staff".
func (s *Service) CanSendServiceMessage(ctx context.Context, conversationID, senderID int64, senderType string) error {
	sess, err := findSessionByConversation(ctx, s.db, conversationID)
	if err != nil {
		return err
	}
	if sess == nil {
		return apperrors.Unavail("session not found")
	}
	if sess.Status != "active" {
		return apperrors.New(apperrors.StateConflict, "session closed")
	}
	switch senderType {
	case "user":
		if sess.UserID != senderID {
			return apperrors.New(apperrors.Forbidden, "not session user")
		}
	case "staff":
		staff, err := isStaff(ctx, s.db, sess.AccountID, senderID)
		if err != nil {
			return err
		}
		if !staff {
			return apperrors.New(apperrors.Forbidden, "not a staff member")
		}
	}
	return nil
}

// StaffReply records a staff text reply and assigns them to the session on
// first message (R12). The message itself is persisted by the message module.
func (s *Service) StaffReply(ctx context.Context, sessionID, staffID int64) error {
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		sess, err := findSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if sess == nil {
			return apperrors.Unavail("session not found")
		}
		staff, err := isStaff(ctx, tx, sess.AccountID, staffID)
		if err != nil {
			return err
		}
		if !staff {
			return apperrors.New(apperrors.Forbidden, "not a staff member")
		}
		if sess.Status != "active" {
			return apperrors.New(apperrors.StateConflict, "session closed")
		}
		current, err := assignedStaff(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if current == 0 {
			if err := assignStaff(ctx, tx, sessionID, staffID, now); err != nil {
				return err
			}
			detail, _ := json.Marshal(map[string]any{"staff_id": staffID})
			if err := insertSessionEvent(ctx, tx, sessionID, staffID, "assigned", detail, now); err != nil {
				return err
			}
			return nil
		}
		if current != staffID {
			return apperrors.New(apperrors.Forbidden, "another staff is assigned")
		}
		return nil
	})
}

func (s *Service) ListStaffSessions(ctx context.Context, staffID, accountID int64, status string, beforeID int64, limit int) ([]SessionRow, error) {
	staff, err := isStaff(ctx, s.db, accountID, staffID)
	if err != nil {
		return nil, err
	}
	if !staff {
		return nil, apperrors.New(apperrors.Forbidden, "not a staff member")
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	return listStaffSessions(ctx, s.db, accountID, status, beforeID, limit)
}

func (s *Service) StaffCanRead(ctx context.Context, conversationID, staffID int64) (bool, error) {
	sess, err := findSessionByConversation(ctx, s.db, conversationID)
	if err != nil {
		return false, err
	}
	if sess == nil {
		return false, nil
	}
	return isStaff(ctx, s.db, sess.AccountID, staffID)
}

// --- notifications (R15-R19) ---

func (s *Service) ListNotifications(ctx context.Context, userID, beforeID int64, limit int) ([]NotificationRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	return listNotifications(ctx, s.db, userID, beforeID, limit)
}

func (s *Service) MarkNotificationsRead(ctx context.Context, userID int64, ids []int64, all bool) error {
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return markNotificationsRead(ctx, tx, userID, ids, all, now)
	})
}

// FanoutArticleNotifications is invoked by the worker on official.article.published.
// It inserts at most 3 article notifications per active, non-muted follower per
// UTC day, skipping any article that already has notifications (I4).
func (s *Service) FanoutArticleNotifications(ctx context.Context, articleID int64) (int, error) {
	art, err := findArticle(ctx, s.db, articleID)
	if err != nil {
		return 0, err
	}
	if art == nil || art.Status != "published" {
		return 0, nil
	}
	existing, err := countExistingArticleNotifications(ctx, s.db, art.AccountID, articleID)
	if err != nil {
		return 0, err
	}
	if existing > 0 {
		return 0, nil
	}
	followers, err := listActiveFollowers(ctx, s.db, art.AccountID)
	if err != nil {
		return 0, err
	}
	now := s.now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	sent := 0
	for _, f := range followers {
		if f.Muted {
			continue
		}
		n, err := countArticleNotificationsToday(ctx, s.db, f.AccountID, f.UserID, dayStart)
		if err != nil {
			return sent, err
		}
		if n >= 3 {
			continue
		}
		articleIDCopy := art.ID
		_, err = insertNotification(ctx, s.db, &NotificationRow{
			AccountID: f.AccountID,
			UserID:    f.UserID,
			Kind:      "article",
			ArticleID: &articleIDCopy,
			Title:     art.Title,
		}, now)
		if err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}
