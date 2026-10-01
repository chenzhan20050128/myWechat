package favorite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// Media is the consumer-defined port over media.
type Media interface {
	OnReferencesRemoved(ctx context.Context, objectIDs []int64) error
	EnqueueGCTx(ctx context.Context, tx mysqlx.Tx, objectIDs []int64, purgeAfter time.Time) error
}

// MessageReader is the consumer-defined port over message.
type MessageReader interface {
	ReadableBatch(ctx context.Context, userID int64, messageIDs []int64) ([]MessageSnapshot, error)
}

// MessageSnapshot is the persisted view of a favorite source message.
type MessageSnapshot struct {
	MessageID int64           `json:"message_id"`
	SenderID  int64           `json:"sender_id"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	SentAt    time.Time       `json:"sent_at"`
}

// Service is the favorite domain service.
type Service struct {
	db    *sql.DB
	media Media
	msg   MessageReader
	now   func() time.Time
}

func New(db *sql.DB, m Media, mr MessageReader, now func() time.Time) *Service {
	return &Service{db: db, media: m, msg: mr, now: now}
}

// CreateInput is the input to Create.
type CreateInput struct {
	OwnerID         int64
	Kind            string
	Content         json.RawMessage
	SourceMessageID *int64
	SourceSenderID  *int64
	SourceSentAt    *time.Time
	SourceType      string
	TotalSize       int64
}

// Create creates a favorite. Idempotent on source_message_id when set.
func (s *Service) Create(ctx context.Context, in *CreateInput) (int64, bool, error) {
	if err := ValidateKind(in.Kind); err != nil {
		return 0, false, err
	}
	if len(in.Content) == 0 {
		in.Content = json.RawMessage(`{}`)
	}
	now := s.now()
	if in.SourceMessageID != nil {
		existing, err := findActiveBySource(ctx, s.db, in.OwnerID, *in.SourceMessageID)
		if err != nil {
			return 0, false, err
		}
		if existing != nil {
			return existing.ID, true, nil
		}
	}
	r := &Row{
		OwnerID:         in.OwnerID,
		Kind:            in.Kind,
		Content:         in.Content,
		SourceMessageID: in.SourceMessageID,
		SourceSenderID:  in.SourceSenderID,
		SourceSentAt:    in.SourceSentAt,
		SourceType:      in.SourceType,
		TotalSize:       in.TotalSize,
	}
	var id int64
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		var err error
		id, err = insertFavorite(ctx, tx, r, now)
		return err
	})
	if err != nil {
		if in.SourceMessageID != nil && mysqlx.IsDuplicate(err, "uk_fav_dedupe") {
			existing, e2 := findActiveBySource(ctx, s.db, in.OwnerID, *in.SourceMessageID)
			if e2 == nil && existing != nil {
				return existing.ID, true, nil
			}
		}
		return 0, false, err
	}
	return id, false, nil
}

func (s *Service) Get(ctx context.Context, ownerID, id int64) (*Row, error) {
	r, err := findFavorite(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	if r.OwnerID != ownerID {
		return nil, apperrors.Unavail("favorite not found")
	}
	return r, nil
}

func (s *Service) List(ctx context.Context, ownerID, beforeID int64, kind, tag string, limit int) ([]Row, error) {
	if limit <= 0 || limit > MaxPageSize {
		limit = DefaultPageSize
	}
	return listFavorites(ctx, s.db, ownerID, beforeID, kind, tag, limit)
}

func (s *Service) Delete(ctx context.Context, ownerID, id int64) error {
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return softDelete(ctx, tx, id, ownerID, now)
	})
}

func (s *Service) CreateTag(ctx context.Context, ownerID int64, tag string) error {
	if err := ValidateTag(tag); err != nil {
		return err
	}
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		n, err := countTags(ctx, tx, ownerID)
		if err != nil {
			return err
		}
		if n >= MaxTagsPerUser {
			return apperrors.New(apperrors.QuotaExceeded, "tag quota exceeded")
		}
		return upsertTag(ctx, tx, ownerID, tag, now)
	})
}

func (s *Service) ListTags(ctx context.Context, ownerID int64) ([]TagRow, error) {
	return listTags(ctx, s.db, ownerID)
}

func (s *Service) ApplyTags(ctx context.Context, ownerID, favoriteID int64, tags []string) error {
	r, err := findFavorite(ctx, s.db, favoriteID)
	if err != nil {
		return err
	}
	if r.OwnerID != ownerID {
		return apperrors.Unavail("favorite not found")
	}
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return setTagsOnFavorite(ctx, tx, favoriteID, ownerID, tags)
	})
}

func (s *Service) RemoveTags(ctx context.Context, ownerID, favoriteID int64, tags []string) error {
	r, err := findFavorite(ctx, s.db, favoriteID)
	if err != nil {
		return err
	}
	if r.OwnerID != ownerID {
		return apperrors.Unavail("favorite not found")
	}
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return removeTagsFromFavorite(ctx, tx, favoriteID, tags)
	})
}

// --- cleanup ---

type PreviewInput struct {
	UserID int64
	Scope  string
}

type PreviewResult struct {
	CleanupID  int64        `json:"cleanup_id"`
	Items      []RefSummary `json:"items"`
	TotalBytes int64        `json:"total_bytes"`
	Count      int          `json:"count"`
}

func (s *Service) Preview(ctx context.Context, in *PreviewInput) (*PreviewResult, error) {
	if in.Scope != "user_references" {
		return nil, apperrors.Invalid("scope must be user_references in V1")
	}
	refs, err := listUserReferences(ctx, s.db, in.UserID)
	if err != nil {
		return nil, err
	}
	if len(refs) > MaxCleanupItems {
		refs = refs[:MaxCleanupItems]
	}
	var totalBytes int64
	for _, r := range refs {
		totalBytes += r.Size
	}
	previewPayload, _ := json.Marshal(map[string]any{"count": len(refs), "total_bytes": totalBytes})
	filterPayload, _ := json.Marshal(map[string]any{"scope": in.Scope})
	now := s.now()
	job := &JobRow{UserID: in.UserID, Scope: in.Scope, Filter: filterPayload, Preview: previewPayload, Status: "previewed"}
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		id, err := insertJob(ctx, tx, job, now)
		if err != nil {
			return err
		}
		job.ID = id
		for _, r := range refs {
			if _, err := insertCleanupItem(ctx, tx, &ItemRow{CleanupID: id, UserRefID: r.RefID, MediaObjectID: r.ObjectID}, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &PreviewResult{CleanupID: job.ID, Items: refs, TotalBytes: totalBytes, Count: len(refs)}, nil
}

func (s *Service) Confirm(ctx context.Context, userID, jobID int64) (int, int, error) {
	now := s.now()
	job, err := findJob(ctx, s.db, jobID, userID)
	if err != nil {
		return 0, 0, err
	}
	if job.Status != "previewed" {
		items, _ := listCleanupItems(ctx, s.db, jobID)
		success, failed := 0, 0
		for _, it := range items {
			if it.State == "success" {
				success++
			} else {
				failed++
			}
		}
		return success, failed, nil
	}
	items, err := listCleanupItems(ctx, s.db, jobID)
	if err != nil {
		return 0, 0, err
	}
	if err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return markJobConfirmed(ctx, tx, jobID, userID, now)
	}); err != nil {
		return 0, 0, err
	}
	success, failed := 0, 0
	for _, it := range items {
		err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
			res, err := tx.ExecContext(ctx, `DELETE FROM media_user_references WHERE id = ? AND user_id = ? AND revoked_at IS NULL`, it.UserRefID, userID)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				return updateItemState(ctx, tx, it.ItemID, "success", "", now)
			}
			remaining, err := countActiveRefsForObject(ctx, tx, it.MediaObjectID)
			if err != nil {
				return err
			}
			if remaining == 0 {
				purgeAfter := now.AddDate(0, 0, GCRetentionDays)
				if err := s.media.EnqueueGCTx(ctx, tx, []int64{it.MediaObjectID}, purgeAfter); err != nil {
					return err
				}
			}
			return updateItemState(ctx, tx, it.ItemID, "success", "", now)
		})
		if err != nil {
			_ = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
				return updateItemState(ctx, tx, it.ItemID, "failed", err.Error(), now)
			})
			failed++
		} else {
			success++
		}
	}
	status := "done"
	if failed > 0 {
		status = "partial"
	}
	_ = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return markJobDone(ctx, tx, jobID, status, now)
	})
	return success, failed, nil
}

func (s *Service) Job(ctx context.Context, userID, jobID int64) (*JobRow, []ItemRow, error) {
	job, err := findJob(ctx, s.db, jobID, userID)
	if err != nil {
		return nil, nil, err
	}
	items, err := listCleanupItems(ctx, s.db, jobID)
	if err != nil {
		return nil, nil, err
	}
	return job, items, nil
}
