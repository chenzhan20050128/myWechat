// Package group owns groups, members, roles, invite codes, mutes and todos
// (SPEC-05). Bytes are not stored here; MySQL rows are the only fact source.
package group

import (
	"strings"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/validate"
)

// Roles (R5).
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// Group statuses (R3).
const (
	StatusActive    = "active"
	StatusDissolved = "dissolved"
)

// Left reasons (R12).
const (
	LeftQuit     = "quit"
	LeftRemoved  = "removed"
	LeftDissolved = "dissolved"
)

// Todo statuses (R19-R24).
const (
	TodoActive     = "active"
	TodoCompleted  = "completed"
	TodoOverdue    = "overdue"
	TodoCancelled  = "cancelled"
	AsgAssigned    = "assigned"
	AsgCompleted   = "completed"
	AsgWaived      = "waived"
	AsgCancelled   = "cancelled"
)

// Limits (R2, R6, R9, R11, R14, R19).
const (
	MaxGroupSize      = 500
	MaxAdmins         = 10
	MaxInviteBatch     = 50
	MaxInviteUses      = 50
	InviteTTL         = 24 * time.Hour
	InviteCodeLen      = 10
	BanRejoinWindow   = 24 * time.Hour
	MaxTitleRunes      = 200
	MaxDescRunes       = 2000
	MaxAssignees      = 20
	MaxGroupnameRunes = 64
	MaxAnnouncement   = 2000
)

// Mute durations (R14).
var muteDurations = map[string]time.Duration{
	"10m":   10 * time.Minute,
	"1h":    time.Hour,
	"1d":    24 * time.Hour,
	"forever": 0,
}

// validateGroupName enforces R1's 1..64-char bound.
func validateGroupName(name string) error {
	if !validate.RuneRange(name, 1, MaxGroupnameRunes) || !validate.NoControl(name) {
		return apperrors.Invalid("group name must be 1..64 characters without control characters")
	}
	return nil
}

// validateTitle enforces R19's title bound.
func validateTitle(title string) error {
	if !validate.RuneRange(title, 1, MaxTitleRunes) || !validate.NoControl(title) {
		return apperrors.Invalid("title must be 1..200 characters without control characters")
	}
	return nil
}

// validateDescription enforces R19's description bound.
func validateDescription(desc string) error {
	if len([]rune(desc)) > MaxDescRunes {
		return apperrors.Invalid("description too long")
	}
	return nil
}

// muteUntil parses a duration key; forever returns a nil *time.Time.
func muteUntil(d string, now time.Time) (*time.Time, error) {
	dur, ok := muteDurations[d]
	if !ok {
		return nil, apperrors.Invalid("duration must be 10m|1h|1d|forever")
	}
	if d == "forever" {
		return nil, nil
	}
	t := now.Add(dur)
	return &t, nil
}

// canSend reports whether user may post in the group (R15/R16). owner/admin
// are immune; others are blocked while a non-expired mute exists.
//
// mutedUntil is NULL when no mute row exists, or the row's until_at (NULL =
// permanent). The DB field is the source of truth; we never rely on a worker
// to expire rows.
func canSend(role string, mutedUntil *time.Time, now time.Time) error {
	if role == RoleOwner || role == RoleAdmin {
		return nil
	}
	if mutedUntil == nil {
		return nil
	}
	if mutedUntil.IsZero() || mutedUntil.After(now) {
		return apperrors.New(apperrors.Forbidden, "muted")
	}
	return nil
}

// canManageTodos mirrors canSend for the create/update endpoint (R15).
func canManageTodos(role string, mutedUntil *time.Time, now time.Time) error {
	return canSend(role, mutedUntil, now)
}

// resolveAssignees dedupes and bounds the assignee list (R19).
func resolveAssignees(ids []int64) ([]int64, error) {
	if len(ids) < 1 || len(ids) > MaxAssignees {
		return nil, apperrors.Invalid("assignee_ids must be 1..20")
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, apperrors.Invalid("assignee_id must be positive")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// reasonOf normalizes a left_reason string.
func reasonOf(r string) string {
	switch strings.ToLower(r) {
	case LeftQuit, LeftRemoved, LeftDissolved:
		return strings.ToLower(r)
	default:
		return ""
	}
}
