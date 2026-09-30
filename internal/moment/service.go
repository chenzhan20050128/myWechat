package moment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mq"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// FriendEpoch is one (user_id, friendship_epoch) pair consumed by the contact port.
type FriendEpoch struct {
	UserID int64
	Epoch  int64
}

// Contact is the moment package's consumer-defined port over the contact
// domain. All relationship lookups for visibility go through this interface.
type Contact interface {
	ActiveFriendsWithEpoch(ctx context.Context, authorID int64) ([]FriendEpoch, error)
	IsFriendCurrentEpoch(ctx context.Context, authorID, viewerID, epoch int64) (bool, error)
	MomentPerm(ctx context.Context, authorID, viewerID int64) (hidden, blocked, noMoments bool, err error)
	ExpandTag(ctx context.Context, ownerID, tagID int64) ([]FriendEpoch, error)
}

// Media asserts uploaded assets are ready before publication.
type Media interface {
	AssertReady(ctx context.Context, ownerID, objectID int64, kind string) error
	BindMomentAssetTx(ctx context.Context, tx mysqlx.Tx, momentID, objectID int64) error
}

// Outbox emits domain events on publication.
type Outbox interface {
	Emit(ctx context.Context, tx mysqlx.Tx, eventType string, aggregateID int64, queue string) error
}

// PublishInput is the input to Publish.
type PublishInput struct {
	AuthorID        int64
	Content         string
	Country         string
	Province        string
	City            string
	PlaceName       string
	AssetIDs        []int64
	Visibility      string // self|all_friends|selected|tag|exclude
	AllowedUserIDs  []int64
	TagID           int64
	ExcludedUserIDs []int64
	AllowComments   *bool
	AllowLikes      *bool
}

// Service is the moment domain service.
type Service struct {
	db      *sql.DB
	contact Contact
	media   Media
	outbox  Outbox
	now     func() time.Time
}

func New(db *sql.DB, c Contact, m Media, o Outbox, now func() time.Time) *Service {
	return &Service{db: db, contact: c, media: m, outbox: o, now: now}
}

// Publish creates a moment and persists its visibility snapshot (R1-R4).
func (s *Service) Publish(ctx context.Context, in *PublishInput) (int64, error) {
	hasLocation := in.Country != "" || in.Province != "" || in.City != "" || in.PlaceName != ""
	if err := ValidateContent(in.Content, len(in.AssetIDs), hasLocation); err != nil {
		return 0, err
	}
	if in.Visibility == "" {
		in.Visibility = VisAllFriends
	}
	if in.Visibility != VisSelf && in.Visibility != VisAllFriends && in.Visibility != VisSelected && in.Visibility != VisTag && in.Visibility != VisExclude {
		return 0, apperrors.Invalid("invalid visibility")
	}
	now := s.now()
	if in.Visibility == "" {
		in.Visibility = VisAllFriends
	}
	rows, err := s.buildSnapshot(ctx, in)
	if err != nil {
		return 0, err
	}
	for _, id := range in.AssetIDs {
		if err := s.media.AssertReady(ctx, in.AuthorID, id, "image"); err != nil {
			return 0, err
		}
	}
	var momentID int64
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		m := &MomentRow{
			AuthorID:      in.AuthorID,
			Content:       in.Content,
			Country:       in.Country,
			Province:      in.Province,
			City:          in.City,
			PlaceName:     in.PlaceName,
			AllowComments: in.AllowComments == nil || *in.AllowComments,
			AllowLikes:    in.AllowLikes == nil || *in.AllowLikes,
		}
		id, err := insertMoment(ctx, tx, m, now)
		if err != nil {
			return err
		}
		momentID = id
		for i, aid := range in.AssetIDs {
			if err := insertAsset(ctx, tx, momentID, aid, i, now); err != nil {
				return err
			}
			if err := s.media.BindMomentAssetTx(ctx, tx, momentID, aid); err != nil {
				return err
			}
		}
		for i := range rows {
			rows[i].MomentID = momentID
		}
		if err := insertVisibility(ctx, tx, rows, now); err != nil {
			return err
		}
		if s.outbox != nil {
			if err := s.outbox.Emit(ctx, tx, "moment.published", momentID, mq.QueueNotification); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return momentID, nil
}

// buildSnapshot materializes publish-time friendship epochs for the chosen visibility.
func (s *Service) buildSnapshot(ctx context.Context, in *PublishInput) ([]VisibilityRow, error) {
	switch in.Visibility {
	case VisSelf:
		return nil, nil
	case VisAllFriends:
		fr, err := s.contact.ActiveFriendsWithEpoch(ctx, in.AuthorID)
		if err != nil {
			return nil, err
		}
		out := make([]VisibilityRow, 0, len(fr))
		for _, f := range fr {
			out = append(out, VisibilityRow{UserID: f.UserID, Epoch: f.Epoch, Allowed: true})
		}
		return out, nil
	case VisSelected:
		if len(in.AllowedUserIDs) == 0 {
			return nil, apperrors.Invalid("selected visibility requires allowed_user_ids")
		}
		fr, err := s.contact.ActiveFriendsWithEpoch(ctx, in.AuthorID)
		if err != nil {
			return nil, err
		}
		epochByUser := make(map[int64]int64, len(fr))
		for _, f := range fr {
			epochByUser[f.UserID] = f.Epoch
		}
		out := make([]VisibilityRow, 0, len(in.AllowedUserIDs))
		for _, uid := range in.AllowedUserIDs {
			ep, ok := epochByUser[uid]
			if !ok {
				return nil, apperrors.Invalid("allowed user is not a friend")
			}
			out = append(out, VisibilityRow{UserID: uid, Epoch: ep, Allowed: true})
		}
		return out, nil
	case VisTag:
		if in.TagID == 0 {
			return nil, apperrors.Invalid("tag visibility requires tag_id")
		}
		fr, err := s.contact.ExpandTag(ctx, in.AuthorID, in.TagID)
		if err != nil {
			return nil, err
		}
		out := make([]VisibilityRow, 0, len(fr))
		for _, f := range fr {
			out = append(out, VisibilityRow{UserID: f.UserID, Epoch: f.Epoch, Allowed: true})
		}
		return out, nil
	case VisExclude:
		fr, err := s.contact.ActiveFriendsWithEpoch(ctx, in.AuthorID)
		if err != nil {
			return nil, err
		}
		excluded := make(map[int64]struct{}, len(in.ExcludedUserIDs))
		for _, uid := range in.ExcludedUserIDs {
			excluded[uid] = struct{}{}
		}
		out := make([]VisibilityRow, 0, len(fr))
		for _, f := range fr {
			_, skip := excluded[f.UserID]
			out = append(out, VisibilityRow{UserID: f.UserID, Epoch: f.Epoch, Allowed: !skip})
		}
		return out, nil
	}
	return nil, nil
}

// canView is the single visibility gate every read path must pass through.
// It combines publish-time snapshot AND realtime relationship (SPEC-06 §4.2):
// status, author, hidden/blocked/no_moments, snapshot allowed, epoch match.
func (s *Service) canView(ctx context.Context, viewerID int64, m MomentRow) (bool, error) {
	if m.Status != StatusVisible {
		return false, nil
	}
	if viewerID == m.AuthorID {
		return true, nil
	}
	hidden, blocked, noMoments, err := s.contact.MomentPerm(ctx, m.AuthorID, viewerID)
	if err != nil {
		return false, err
	}
	if blocked || hidden || noMoments {
		return false, nil
	}
	var epoch int64
	err = s.db.QueryRowContext(ctx, `
		SELECT friendship_epoch FROM moment_visibility_users WHERE moment_id = ? AND user_id = ? AND allowed = 1`,
		m.ID, viewerID).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ok, err := s.contact.IsFriendCurrentEpoch(ctx, m.AuthorID, viewerID, epoch)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// canViewWithEpoch is the historical name retained for call sites; it delegates to canView.
func (s *Service) canViewWithEpoch(ctx context.Context, viewerID int64, m MomentRow) (bool, error) {
	if m.Status != StatusVisible {
		return false, nil
	}
	if viewerID == m.AuthorID {
		return true, nil
	}
	hidden, blocked, noMoments, err := s.contact.MomentPerm(ctx, m.AuthorID, viewerID)
	if err != nil {
		return false, err
	}
	if blocked || hidden || noMoments {
		return false, nil
	}
	var epoch int64
	err = s.db.QueryRowContext(ctx, `
		SELECT friendship_epoch FROM moment_visibility_users WHERE moment_id = ? AND user_id = ? AND allowed = 1`,
		m.ID, viewerID).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ok, err := s.contact.IsFriendCurrentEpoch(ctx, m.AuthorID, viewerID, epoch)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// Delete soft-deletes the author's own moment (R12).
func (s *Service) Delete(ctx context.Context, momentID, authorID int64) error {
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return markMomentDeleted(ctx, tx, momentID, authorID, now)
	})
}

// Feed returns the viewer's feed page (R5).
func (s *Service) Feed(ctx context.Context, viewerID, beforeID int64, limit int) ([]MomentRow, error) {
	if limit <= 0 || limit > FeedPageSize*2 {
		limit = FeedPageSize
	}
	cands, err := feedCandidates(ctx, s.db, viewerID, beforeID, limit*4)
	if err != nil {
		return nil, err
	}
	out := make([]MomentRow, 0, limit)
	for _, m := range cands {
		ok, err := s.canViewWithEpoch(ctx, viewerID, m)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, m)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// UserMoments lists another user's visible moments (R7).
func (s *Service) UserMoments(ctx context.Context, authorID, viewerID, beforeID int64, limit int, albumOnly bool) ([]MomentRow, error) {
	if limit <= 0 || limit > FeedPageSize*2 {
		limit = FeedPageSize
	}
	cands, err := userMoments(ctx, s.db, authorID, viewerID, beforeID, limit*4, albumOnly)
	if err != nil {
		return nil, err
	}
	out := make([]MomentRow, 0, limit)
	for _, m := range cands {
		ok, err := s.canViewWithEpoch(ctx, viewerID, m)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, m)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Get returns a single moment if visible (R8).
func (s *Service) Get(ctx context.Context, viewerID, momentID int64) (MomentRow, error) {
	m, err := findMoment(ctx, s.db, momentID)
	if err != nil {
		return MomentRow{}, err
	}
	ok, err := s.canViewWithEpoch(ctx, viewerID, m)
	if err != nil {
		return MomentRow{}, err
	}
	if !ok {
		return MomentRow{}, apperrors.Unavail("moment not found")
	}
	return m, nil
}

// Assets returns asset media_object_ids for a moment (after canView gate).
func (s *Service) Assets(ctx context.Context, viewerID, momentID int64) ([]int64, error) {
	if _, err := s.Get(ctx, viewerID, momentID); err != nil {
		return nil, err
	}
	return listAssets(ctx, s.db, momentID)
}

// Like adds a like (R9).
func (s *Service) Like(ctx context.Context, momentID, userID int64) error {
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		m, err := findMoment(ctx, tx, momentID)
		if err != nil {
			return err
		}
		ok, err := s.canViewWithEpoch(ctx, userID, m)
		if err != nil {
			return err
		}
		if !ok {
			return apperrors.Unavail("moment not found")
		}
		if !m.AllowLikes && userID != m.AuthorID {
			return apperrors.Invalid("likes disabled")
		}
		inserted, err := insertLike(ctx, tx, momentID, userID, now)
		if err != nil {
			return err
		}
		if inserted && userID != m.AuthorID {
			return insertNotification(ctx, tx, NotificationRow{RecipientID: m.AuthorID, MomentID: momentID, Kind: "like", ActorID: userID}, now)
		}
		return nil
	})
}

// Unlike removes a like.
func (s *Service) Unlike(ctx context.Context, momentID, userID int64) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return deleteLike(ctx, tx, momentID, userID)
	})
}

// Likes lists users who liked a moment.
func (s *Service) Likes(ctx context.Context, viewerID, momentID int64) ([]int64, error) {
	if _, err := s.Get(ctx, viewerID, momentID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT user_id FROM moment_likes WHERE moment_id = ? ORDER BY created_at ASC`, momentID)
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

// AddComment adds a comment (R10).
func (s *Service) AddComment(ctx context.Context, momentID, userID int64, replyTo *int64, content string) (*CommentRow, error) {
	if err := ValidateComment(content); err != nil {
		return nil, err
	}
	now := s.now()
	var out *CommentRow
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		m, err := findMoment(ctx, tx, momentID)
		if err != nil {
			return err
		}
		ok, err := s.canViewWithEpoch(ctx, userID, m)
		if err != nil {
			return err
		}
		if !ok {
			return apperrors.Unavail("moment not found")
		}
		if !m.AllowComments && userID != m.AuthorID {
			return apperrors.Invalid("comments disabled")
		}
		if replyTo != nil {
			rt, err := findComment(ctx, tx, *replyTo)
			if err != nil {
				return err
			}
			if rt.MomentID != momentID || rt.Status != CommentVisible {
				return apperrors.Invalid("reply target not visible")
			}
		}
		id, err := insertComment(ctx, tx, &CommentRow{MomentID: momentID, UserID: userID, ReplyTo: replyTo, Content: content}, now)
		if err != nil {
			return err
		}
		out = &CommentRow{ID: id, MomentID: momentID, UserID: userID, ReplyTo: replyTo, Content: content, Status: CommentVisible, CreatedAt: now}
		if userID != m.AuthorID {
			if err := insertNotification(ctx, tx, NotificationRow{RecipientID: m.AuthorID, MomentID: momentID, Kind: "comment", ActorID: userID}, now); err != nil {
				return err
			}
		}
		if replyTo != nil {
			rt, err := findComment(ctx, tx, *replyTo)
			if err != nil {
				return err
			}
			if rt.UserID != userID {
				if err := insertNotification(ctx, tx, NotificationRow{RecipientID: rt.UserID, MomentID: momentID, Kind: "reply", ActorID: userID}, now); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return out, err
}

// DeleteComment removes a comment (author or moment owner).
func (s *Service) DeleteComment(ctx context.Context, commentID, userID int64) error {
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		c, err := findComment(ctx, tx, commentID)
		if err != nil {
			return err
		}
		m, err := findMoment(ctx, tx, c.MomentID)
		if err != nil {
			return err
		}
		if c.UserID != userID && m.AuthorID != userID {
			return apperrors.Invalid("not allowed")
		}
		return deleteComment(ctx, tx, commentID, now)
	})
}

// Comments lists comments on a moment.
func (s *Service) Comments(ctx context.Context, viewerID, momentID int64) ([]CommentRow, error) {
	if _, err := s.Get(ctx, viewerID, momentID); err != nil {
		return nil, err
	}
	rows, err := listComments(ctx, s.db, momentID)
	if err != nil {
		return nil, err
	}
	out := make([]CommentRow, 0, len(rows))
	for _, c := range rows {
		if c.Status == CommentVisible {
			out = append(out, c)
		}
	}
	return out, nil
}

// Notifications lists the recipient's moment notifications (R11).
func (s *Service) Notifications(ctx context.Context, recipientID int64, limit int) ([]NotificationRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, recipient_id, moment_id, kind, actor_id, created_at, read_at
		FROM moment_notifications WHERE recipient_id = ? ORDER BY id DESC LIMIT ?`, recipientID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationRow
	for rows.Next() {
		var n NotificationRow
		if err := rows.Scan(&n.ID, &n.RecipientID, &n.MomentID, &n.Kind, &n.ActorID, &n.CreatedAt, &n.ReadAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkNotificationsRead marks notifications read (R11).
func (s *Service) MarkNotificationsRead(ctx context.Context, recipientID int64, ids []int64, all bool) error {
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return markNotificationsRead(ctx, tx, recipientID, ids, all, now)
	})
}

// CreateSchedule stages a scheduled moment (R13).
func (s *Service) CreateSchedule(ctx context.Context, in *PublishInput, runAt time.Time) (int64, error) {
	now := s.now()
	if err := ValidateRunAt(runAt, now); err != nil {
		return 0, err
	}
	rows, err := s.buildSnapshot(ctx, in)
	if err != nil {
		return 0, err
	}
	var scheduleID int64
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		sid, err := insertSchedule(ctx, tx, &ScheduleRow{AuthorID: in.AuthorID, RunAt: runAt}, now)
		if err != nil {
			return err
		}
		scheduleID = sid
		m := MomentRow{
			Content:       in.Content,
			Country:       in.Country,
			Province:      in.Province,
			City:          in.City,
			PlaceName:     in.PlaceName,
			AllowComments: in.AllowComments == nil || *in.AllowComments,
			AllowLikes:    in.AllowLikes == nil || *in.AllowLikes,
		}
		if err := insertScheduleContent(ctx, tx, sid, 1, &m); err != nil {
			return err
		}
		if err := insertScheduleAssets(ctx, tx, sid, 1, in.AssetIDs, now); err != nil {
			return err
		}
		for i := range rows {
			rows[i].MomentID = sid
		}
		return insertScheduleVisibility(ctx, tx, sid, 1, rows)
	})
	if err != nil {
		return 0, err
	}
	return scheduleID, nil
}

// ListSchedules lists scheduled moments for the author.
func (s *Service) ListSchedules(ctx context.Context, authorID int64) ([]ScheduleRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, author_id, run_at, status, current_version, execution_version, moment_id, COALESCE(fail_reason,''), retry_count, created_at, updated_at
		FROM moment_schedules WHERE author_id = ? AND status IN ('scheduled','failed') ORDER BY run_at ASC`, authorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduleRow
	for rows.Next() {
		var sch ScheduleRow
		var mid sql.NullInt64
		var reason sql.NullString
		if err := rows.Scan(&sch.ID, &sch.AuthorID, &sch.RunAt, &sch.Status, &sch.CurrentVersion, &sch.ExecutionVersion, &mid, &reason, &sch.RetryCount, &sch.CreatedAt, &sch.UpdatedAt); err != nil {
			return nil, err
		}
		if mid.Valid {
			sch.MomentID = &mid.Int64
		}
		sch.FailReason = reason.String
		out = append(out, sch)
	}
	return out, rows.Err()
}

// CancelSchedule cancels a scheduled moment (R15).
func (s *Service) CancelSchedule(ctx context.Context, scheduleID, authorID int64) error {
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return markScheduleCancelled(ctx, tx, scheduleID, authorID, now)
	})
}

// RetrySchedule reschedules a failed moment for immediate publication (R16).
func (s *Service) RetrySchedule(ctx context.Context, scheduleID, authorID int64) error {
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return resetScheduleForRetry(ctx, tx, scheduleID, authorID, now)
	})
}

// PublishDueSchedules is the worker entry point: lease and publish due schedules (R17).
func (s *Service) PublishDueSchedules(ctx context.Context, worker string) (int, error) {
	now := s.now()
	published := 0
	for {
		var leased *ScheduleRow
		err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
			var err error
			leased, err = claimDueSchedules(ctx, tx, worker, now, ScheduleLeaseTTL)
			return err
		})
		if err != nil {
			return published, err
		}
		if leased == nil {
			return published, nil
		}
		if err := s.publishLeased(ctx, leased, worker, now); err != nil {
			failErr := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
				return markScheduleFailed(ctx, tx, leased.ID, leased.ExecutionVersion, worker, err.Error(), now)
			})
			if failErr != nil {
				return published, fmt.Errorf("publish schedule %d: %w (fail: %v)", leased.ID, err, failErr)
			}
			return published, err
		}
		published++
	}
}

func (s *Service) publishLeased(ctx context.Context, sch *ScheduleRow, worker string, now time.Time) error {
	content, assetIDs, vis, err := loadScheduleContent(ctx, s.db, sch.ID, sch.CurrentVersion)
	if err != nil {
		return err
	}
	content.AuthorID = sch.AuthorID
	var momentID int64
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		content.AllowComments = true
		content.AllowLikes = true
		mid, err := insertMoment(ctx, tx, &content, now)
		if err != nil {
			return err
		}
		momentID = mid
		for i, aid := range assetIDs {
			if err := insertAsset(ctx, tx, momentID, aid, i, now); err != nil {
				return err
			}
			if err := s.media.BindMomentAssetTx(ctx, tx, momentID, aid); err != nil {
				return err
			}
		}
		for i := range vis {
			vis[i].MomentID = momentID
		}
		if err := insertVisibility(ctx, tx, vis, now); err != nil {
			return err
		}
		if err := markSchedulePublished(ctx, tx, sch.ID, momentID, sch.ExecutionVersion, worker, now); err != nil {
			return err
		}
		return nil
	})
	return err
}

// RecoverStaleLeases releases leases held longer than TTL (worker safety).
func (s *Service) RecoverStaleLeases(ctx context.Context) error {
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		_, err := recoverStaleLeases(ctx, tx, now)
		return err
	})
}
