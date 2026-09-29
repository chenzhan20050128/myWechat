// Package favorite owns user favorites (spec-07 §4) and the storage cleanup
// three-step job (spec-07 §5). Snapshots are immutable after creation; reads
// never join back to messages/moments.
package favorite

import (
	"strings"

	apperrors "github.com/example/wechat/internal/platform/errors"
)

const (
	MaxTagsPerUser   = 100
	MaxTagLen        = 32
	MaxChatRecords   = 100
	DefaultPageSize  = 50
	MaxPageSize      = 100
	MaxCleanupItems  = 1000
	GCRetentionDays  = 7
)

// ValidKinds is the whitelist of favorite kinds.
var ValidKinds = map[string]bool{
	"text": true, "emoji": true, "image": true, "video": true,
	"voice": true, "file": true, "link": true, "card": true,
	"location": true, "chat_record": true,
}

// ValidateTag enforces R6.
func ValidateTag(tag string) error {
	t := strings.TrimSpace(tag)
	if t == "" || len([]rune(t)) > MaxTagLen {
		return apperrors.Invalid("tag must be 1..32 chars")
	}
	return nil
}

// ValidateKind enforces R1.
func ValidateKind(kind string) error {
	if !ValidKinds[kind] {
		return apperrors.Invalid("unknown kind")
	}
	return nil
}
