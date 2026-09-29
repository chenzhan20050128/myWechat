// Package contact owns friend requests, friendship epochs, directional friend
// settings, contact tags and the address-book read paths (SPEC-02, ADR-004).
// Friend-facing tables are written only here; other modules call the exported
// Service methods (GetActiveFriendship, CanSendMessage, ListFriendIDs).
package contact

import (
	"fmt"
	"strings"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/validate"
)

// Request sources (R1, contract §4.1).
const (
	SourcePhone       = "phone"
	SourceAccountName = "account_name"
	SourceQRCode      = "qrcode"
	SourceGroup       = "group"
	SourceCard        = "card"
)

// Request statuses (R2).
const (
	RequestPending   = "pending"
	RequestAccepted  = "accepted"
	RequestRejected  = "rejected"
	RequestExpired   = "expired"
	RequestCancelled = "cancelled"
)

// Message permissions (R16, contract §4.3).
const (
	PermNormal    = "normal"
	PermNoMessage = "no_message"
	PermBlocked   = "blocked"
)

// Moment permissions (R16, contract §4.3).
const (
	MomentVisible = "visible"
	MomentHidden  = "hidden"
)

// CanSendMessage denial reasons (R18). Executed by the message module.
const (
	ReasonNotFriend          = "not_friend"
	ReasonBlockedByRecipient = "blocked_by_recipient"
	ReasonSenderBlocked      = "sender_blocked"
	ReasonRecipientNoMessage = "recipient_no_message"
)

const (
	maxVerifyRunes = 200
	maxRemarkRunes = 64
	maxTagRunes    = 32
	// RejectCooldown is how long an applicant must wait after a rejection (R6).
	RejectCooldown = 24 * time.Hour
	// RequestTTL is the pending lifetime before the worker expires a request (R2).
	RequestTTL = 7 * 24 * time.Hour
	// QRTokenTTL is the personal QR code validity (R1).
	QRTokenTTL = 7 * 24 * time.Hour
	// LookupPerMinute caps stranger lookups per user (R24).
	LookupPerMinute = 20
	// SearchScanCap bounds how many friends a single search request may scan
	// (R26 MVP: full scan of up to 100 friends is acceptable).
	SearchScanCap = 100
)

// Settings is one owner's directional view of a friend (R15). A missing row
// means DefaultSettings.
type Settings struct {
	Remark       string
	MessagePerm  string
	MomentPerm   string
	MomentNotify bool
}

// DefaultSettings is the state of a fresh friendship cycle (R8④, ruling 3).
func DefaultSettings() Settings {
	return Settings{MessagePerm: PermNormal, MomentPerm: MomentVisible, MomentNotify: true}
}

// normalizePair orders the two ids so every pair has exactly one row set (ADR-004).
func normalizePair(a, b int64) (lo, hi int64) {
	if a < b {
		return a, b
	}
	return b, a
}

// validateSource enforces the R1 source enumeration.
func validateSource(source string) error {
	if !validate.OneOf(source, SourcePhone, SourceAccountName, SourceQRCode, SourceGroup, SourceCard) {
		return apperrors.Invalid("source must be phone|account_name|qrcode|group|card")
	}
	return nil
}

// validateVerifyText enforces R7.
func validateVerifyText(s string) error {
	if !validate.RuneRange(s, 0, maxVerifyRunes) {
		return apperrors.Invalid(fmt.Sprintf("verify_text must be at most %d characters", maxVerifyRunes))
	}
	if !validate.NoControl(s) {
		return apperrors.Invalid("verify_text contains control or edge whitespace")
	}
	return nil
}

// SettingsPatch is a partial update; nil fields stay unchanged (R15-R19).
type SettingsPatch struct {
	Remark       *string
	MessagePerm  *string
	MomentPerm   *string
	MomentNotify *bool
}

// Validate checks the patch against the contract limits (R15, R16, R19).
func (p SettingsPatch) Validate() error {
	if p.Remark != nil {
		if !validate.RuneRange(*p.Remark, 0, maxRemarkRunes) {
			return apperrors.Invalid(fmt.Sprintf("remark must be at most %d characters", maxRemarkRunes))
		}
		if !validate.NoControl(*p.Remark) {
			return apperrors.Invalid("remark contains control or edge whitespace")
		}
	}
	if p.MessagePerm != nil && !validate.OneOf(*p.MessagePerm, PermNormal, PermNoMessage, PermBlocked) {
		return apperrors.Invalid("message_perm must be normal|no_message|blocked")
	}
	if p.MomentPerm != nil && !validate.OneOf(*p.MomentPerm, MomentVisible, MomentHidden) {
		return apperrors.Invalid("moment_perm must be visible|hidden")
	}
	return nil
}

// Apply overlays the patch onto a base settings value.
func (p SettingsPatch) Apply(base Settings) Settings {
	if p.Remark != nil {
		base.Remark = *p.Remark
	}
	if p.MessagePerm != nil {
		base.MessagePerm = *p.MessagePerm
	}
	if p.MomentPerm != nil {
		base.MomentPerm = *p.MomentPerm
	}
	if p.MomentNotify != nil {
		base.MomentNotify = *p.MomentNotify
	}
	return base
}

// validateTagName enforces R20 (1..32 runes, no control characters).
func validateTagName(name string) error {
	if !validate.RuneRange(name, 1, maxTagRunes) {
		return apperrors.Invalid(fmt.Sprintf("tag name must be 1..%d characters", maxTagRunes))
	}
	if !validate.NoControl(name) {
		return apperrors.Invalid("tag name contains control or edge whitespace")
	}
	return nil
}

// decideSend implements R18 and contract §4.4: any blocking direction wins.
// fromView is the sender's own settings about the recipient, toView the
// recipient's settings about the sender.
func decideSend(fromView, toView Settings) (bool, string) {
	switch {
	case toView.MessagePerm == PermBlocked:
		return false, ReasonBlockedByRecipient
	case fromView.MessagePerm == PermBlocked:
		return false, ReasonSenderBlocked
	case toView.MessagePerm == PermNoMessage:
		return false, ReasonRecipientNoMessage
	default:
		return true, ""
	}
}

// cooldownRemaining implements R6: time left before the applicant may retry.
func cooldownRemaining(rejectedAt, now time.Time) time.Duration {
	left := RejectCooldown - now.Sub(rejectedAt)
	if left < 0 {
		return 0
	}
	return left
}

// matchesSearch implements R26: case-insensitive substring over nickname and
// remark, on the already-normalized query.
func matchesSearch(q, nickname, remark string) bool {
	if q == "" {
		return false
	}
	q = strings.ToLower(q)
	return strings.Contains(strings.ToLower(nickname), q) || strings.Contains(strings.ToLower(remark), q)
}
