// Package moment owns moments, their visibility snapshots, likes, comments,
// notifications and scheduled publication (SPEC-06). MySQL rows are the only
// fact source; visibility = publish-time snapshot AND realtime relationship.
package moment

import (
	"strings"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
)

// Visibility enum (R2).
const (
	VisSelf       = "self"
	VisAllFriends = "all_friends"
	VisSelected   = "selected"
	VisTag        = "tag"
	VisExclude    = "exclude"
)

// Moment statuses.
const (
	StatusVisible  = "visible"
	StatusDeleted  = "deleted"
	StatusModerated = "moderated"
)

// Comment statuses.
const (
	CommentVisible = "visible"
	CommentDeleted = "deleted"
)

// Schedule statuses.
const (
	SchedScheduled = "scheduled"
	SchedPublished = "published"
	SchedCancelled = "cancelled"
	SchedFailed    = "failed"
)

// Limits.
const (
	MaxContentRunes   = 2000
	MaxAssets         = 9
	MaxCommentRunes   = 500
	FeedPageSize      = 20
	ScheduleWindow    = 30 * 24 * time.Hour
	ScheduleLeaseTTL  = 2 * time.Minute
)

// ValidateContent enforces R1 (empty moment rejected).
func ValidateContent(content string, assetCount int, hasLocation bool) error {
	c := strings.TrimSpace(content)
	if c == "" && assetCount == 0 && !hasLocation {
		return apperrors.Invalid("moment must have content, image, or location")
	}
	if len([]rune(content)) > MaxContentRunes {
		return apperrors.Invalid("content exceeds 2000 runes")
	}
	if assetCount > MaxAssets {
		return apperrors.Invalid("at most 9 assets")
	}
	return nil
}

// ValidateComment enforces R10.
func ValidateComment(content string) error {
	c := strings.TrimSpace(content)
	if c == "" || len([]rune(content)) > MaxCommentRunes {
		return apperrors.Invalid("comment must be 1..500 runes")
	}
	return nil
}

// ValidateRunAt enforces R13.
func ValidateRunAt(runAt, now time.Time) error {
	if !runAt.After(now) {
		return apperrors.Invalid("run_at must be in the future")
	}
	if runAt.Sub(now) > ScheduleWindow {
		return apperrors.Invalid("run_at must be within 30 days")
	}
	return nil
}
