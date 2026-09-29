// Package message owns message rows, conversation_seq allocation, references,
// forwards, pins, conversation settings and the 180-day retention worker
// (SPEC-04). MySQL rows are the only fact source; this package never touches
// media bytes — media references go through the media port.
package message

import (
	"encoding/json"
	"strings"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/validate"
)

// Message types (R7, §3 payload schema).
const (
	TypeText     = "text"
	TypeEmoji    = "emoji"
	TypeImage    = "image"
	TypeVideo    = "video"
	TypeVoice    = "voice"
	TypeFile     = "file"
	TypeCard     = "card"
	TypeLink     = "link"
	TypeSystem   = "system"
)

// Sender types.
const (
	SenderUser   = "user"
	SenderSystem = "system"
)

// Message statuses (R7/R9/R26).
const (
	StatusStored    = "stored"
	StatusDelivered = "delivered"
	StatusRecalled  = "recalled"
	StatusExpired   = "expired"
)

// Limits (SPEC-04 §3/§4).
const (
	RetentionDays     = 180
	RecallWindow     = 2 * time.Minute
	HistoryLimit      = 50
	MaxTextRunes      = 10000
	MaxEmojiRunes     = 64
	MaxTitleRunes     = 128
	MaxSummaryRunes   = 512
	MaxURLLen         = 2048
	MaxFileNameBytes  = 255
	MaxDurationVideo  = 600000
	MaxDurationVoice  = 60000
	MaxMessagePins    = 20
	MaxDigestRunes     = 200
	MaxSendTextPreview = 50
)

// Payload is the decoded JSON payload of a message. Field set is fixed by §3;
// unknown fields are rejected by DecodeBody (DisallowUnknownFields).
type Payload struct {
	// text / emoji
	Content string `json:"content,omitempty"`
	// emoji
	ImageObjectID string `json:"image_object_id,omitempty"`
	// image / video / voice / file
	MediaObjectID string `json:"media_object_id,omitempty"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	DurationMS    int    `json:"duration_ms,omitempty"`
	Size          int    `json:"size,omitempty"`
	FileName      string `json:"file_name,omitempty"`
	// card
	UserID      string `json:"user_id,omitempty"`
	Nickname    string `json:"nickname,omitempty"`
	AccountName string `json:"account_name,omitempty"`
	AvatarMediaID string `json:"avatar_media_id,omitempty"`
	// link
	URL     string `json:"url,omitempty"`
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	ThumbMediaID string `json:"thumb_media_id,omitempty"`
	// system
	Event string `json:"event,omitempty"`
}

// validatePayload enforces §3 per-type rules. It returns INVALID_ARGUMENT on
// any violation and never mutates the payload.
func validatePayload(msgType string, p *Payload) error {
	switch msgType {
	case TypeText:
		if !validate.RuneRange(p.Content, 1, MaxTextRunes) || !validate.NoControl(p.Content) {
			return apperrors.Invalid("text content must be 1..10000 runes without control characters")
		}
	case TypeEmoji:
		if !validate.RuneRange(p.Content, 1, MaxEmojiRunes) {
			return apperrors.Invalid("emoji content must be 1..64 runes")
		}
	case TypeImage:
		if p.MediaObjectID == "" {
			return apperrors.Invalid("image requires media_object_id")
		}
		if p.Width <= 0 || p.Height <= 0 {
			return apperrors.Invalid("image requires positive width/height")
		}
	case TypeVideo:
		if p.MediaObjectID == "" {
			return apperrors.Invalid("video requires media_object_id")
		}
		if p.DurationMS <= 0 || p.DurationMS > MaxDurationVideo {
			return apperrors.Invalid("video duration_ms must be 1..600000")
		}
	case TypeVoice:
		if p.MediaObjectID == "" {
			return apperrors.Invalid("voice requires media_object_id")
		}
		if p.DurationMS <= 0 || p.DurationMS > MaxDurationVoice {
			return apperrors.Invalid("voice duration_ms must be 1..60000")
		}
	case TypeFile:
		if p.MediaObjectID == "" {
			return apperrors.Invalid("file requires media_object_id")
		}
		if len(p.FileName) == 0 || len(p.FileName) > MaxFileNameBytes {
			return apperrors.Invalid("file_name must be 1..255 bytes")
		}
		if p.Size <= 0 {
			return apperrors.Invalid("file size must be positive")
		}
	case TypeCard:
		if p.UserID == "" || p.Nickname == "" || p.AccountName == "" {
			return apperrors.Invalid("card requires user_id/nickname/account_name")
		}
	case TypeLink:
		if len(p.URL) == 0 || len(p.URL) > MaxURLLen {
			return apperrors.Invalid("url must be 1..2048 chars")
		}
		if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
			return apperrors.Invalid("url must be http(s)")
		}
		if len([]rune(p.Title)) > MaxTitleRunes || len([]rune(p.Summary)) > MaxSummaryRunes {
			return apperrors.Invalid("title ≤128 runes, summary ≤512 runes")
		}
	case TypeSystem:
		if p.Event == "" {
			return apperrors.Invalid("system event required")
		}
	default:
		return apperrors.Invalid("unknown message type: " + msgType)
	}
	return nil
}

// digestOf builds the ≤200-rune snapshot for a reference reply (R10).
func digestOf(msgType string, p *Payload) string {
	var s string
	switch msgType {
	case TypeText, TypeEmoji:
		s = p.Content
	case TypeImage:
		s = "[图片]"
	case TypeVideo:
		s = "[视频]"
	case TypeVoice:
		s = "[语音]"
	case TypeFile:
		s = "[文件]"
	case TypeCard:
		s = "[名片]"
	case TypeLink:
		s = p.Title
	default:
		s = ""
	}
	r := []rune(s)
	if len(r) > MaxDigestRunes {
		r = r[:MaxDigestRunes]
	}
	return string(r)
}

// previewOf builds the ≤50-rune push preview (R21).
func previewOf(msgType string, p *Payload) string {
	r := []rune(p.Content)
	if len(r) > MaxSendTextPreview {
		r = r[:MaxSendTextPreview]
	}
	return string(r)
}

// decodePayload parses raw JSON into Payload; unknown fields are rejected.
func decodePayload(raw json.RawMessage) (*Payload, error) {
	p := &Payload{}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(p); err != nil {
		return nil, apperrors.Invalid("payload: " + err.Error())
	}
	return p, nil
}

// expiryAt is the fixed 180-day retention deadline (R28).
func expiryAt(now time.Time) time.Time {
	return now.AddDate(0, 0, RetentionDays)
}

// canRecall enforces the 2-minute window (R9).
func canRecall(createdAt, now time.Time) bool {
	return now.Sub(createdAt) <= RecallWindow
}

// normalizeHistoryLimit clamps a caller-supplied limit to 1..50 (R14).
func normalizeHistoryLimit(limit int) int {
	if limit <= 0 {
		return HistoryLimit
	}
	if limit > HistoryLimit {
		return HistoryLimit
	}
	return limit
}
