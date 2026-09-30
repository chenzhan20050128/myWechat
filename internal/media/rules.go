// Package media owns upload sessions, chunked assembly, media objects and
// their business/user references, plus signed download URLs (SPEC-03).
// Bytes live in the storage port; MySQL rows are the only fact source for
// upload/object state.
package media

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/validate"
)

// Upload purposes (R2). Phase 1 validates the enumeration and ownership only.
const (
	PurposeMessage  = "message"
	PurposeMoment   = "moment"
	PurposeFavorite = "favorite"
	PurposeAvatar   = "avatar"
	PurposeTransfer = "transfer"
)

// Session statuses (R2/R6/R7).
const (
	SessionOpen       = "open"
	SessionAssembling = "assembling"
	SessionCompleted  = "completed"
	SessionAborted    = "aborted"
	SessionExpired    = "expired"
)

// AssemblyLeaseTTL bounds a server-side chunk assembly attempt.
const AssemblyLeaseTTL = 5 * time.Minute

// Object statuses (R10).
const (
	ObjectReady   = "ready"
	ObjectCleaned = "cleaned"
	ObjectDeleted = "deleted"
)

// Reference business types (R11).
const (
	RefAvatar = "avatar"
)

const (
	// MaxObjectSize is the single-object ceiling (contract §8.3, R2).
	MaxObjectSize = 200 << 20
	// MultiChunkThreshold: larger uploads must be chunked (R3).
	MultiChunkThreshold = 10 << 20
	// ChunkSize is the fixed part size above the threshold (R3).
	ChunkSize = 5 << 20
	// SessionTTL bounds how long an open upload may live (R1/R7).
	SessionTTL = 24 * time.Hour
	// MaxAvatarSize and the avatar MIME set (SPEC-01 R27).
	MaxAvatarSize = 10 << 20
	// maxFileNameRunes bounds the stored display name (R1).
	maxFileNameRunes = 255
	// sniffLen is how many leading bytes net/http needs to detect a type.
	sniffLen = 512
)

// allowedMIMEs is the R8 whitelist, mapped to canonical file extensions.
var allowedMIMEs = map[string]string{
	"image/jpeg":               "jpg",
	"image/png":                "png",
	"image/webp":               "webp",
	"image/gif":                "gif",
	"video/mp4":                "mp4",
	"audio/mpeg":               "mp3",
	"audio/aac":                "aac",
	"audio/amr":                "amr",
	"application/octet-stream": "bin",
}

// avatarMIMEs is the SPEC-01 R27 subset.
var avatarMIMEs = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/webp": true,
}

// validatePurpose enforces the R2 enumeration.
func validatePurpose(p string) error {
	if !validate.OneOf(p, PurposeMessage, PurposeMoment, PurposeFavorite, PurposeAvatar, PurposeTransfer) {
		return apperrors.Invalid("purpose must be message|moment|favorite|avatar|transfer")
	}
	return nil
}

// validateFileName enforces R1's stored display name.
func validateFileName(name string) error {
	if !validate.RuneRange(name, 1, maxFileNameRunes) || !validate.NoControl(name) {
		return apperrors.Invalid("file_name must be 1..255 characters without control characters")
	}
	return nil
}

// ValidateSHA256 enforces R9's 64-lowercase-hex declaration format.
func ValidateSHA256(s string) error {
	if !validate.LowerHex64(s) {
		return apperrors.Invalid("sha256 must be 64 lowercase hex characters")
	}
	return nil
}

// ChunkPlan is the derived upload geometry (R3).
type ChunkPlan struct {
	ChunkSize   int64
	TotalChunks int
}

// planChunks derives the chunk geometry from the declared size: single part up
// to 10MB, fixed 5MB parts above (R3). Part numbering is S3-MultipartUpload
// shaped (design-review D3).
func planChunks(size int64) (ChunkPlan, error) {
	if size <= 0 || size > MaxObjectSize {
		return ChunkPlan{}, apperrors.Invalid(fmt.Sprintf("size must be 1..%d bytes", MaxObjectSize))
	}
	if size <= MultiChunkThreshold {
		return ChunkPlan{ChunkSize: size, TotalChunks: 1}, nil
	}
	total := int(math.Ceil(float64(size) / float64(ChunkSize)))
	return ChunkPlan{ChunkSize: ChunkSize, TotalChunks: total}, nil
}

// expectedChunkSize is the byte count chunk n of total must carry (R4).
func expectedChunkSize(n, total int, chunkSize int64, objectSize int64) int64 {
	if n < 1 || n > total {
		return -1
	}
	if n < total {
		return chunkSize
	}
	return objectSize - chunkSize*int64(total-1)
}

// sniffable reports which whitelisted types Go's detector can recognize from
// bytes. audio/aac and audio/amr are not sniffable, so for them the declared
// type is the only evidence available.
var sniffable = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true,
	"video/mp4": true, "audio/mpeg": true,
}

// validateDeclaredMIME enforces the R8 whitelist at session-creation time,
// before any bytes exist. The full byte-vs-declaration proof happens later in
// resolveMIME during assembly.
func validateDeclaredMIME(declared string) error {
	if _, ok := allowedMIMEs[declared]; !ok {
		return apperrors.Invalid("mime is not allowed")
	}
	return nil
}

// resolveMIME decides the authoritative stored MIME from the sniffed content
// and the client's declaration (R8/R9).
//
// The server never trusts a claim the bytes contradict: a sniffable declared
// type must match what the bytes actually are (junk cannot pose as an image),
// and a non-sniffable claim (aac/amr) is refused when the bytes turn out to be
// a recognizable type. A generic application/octet-stream declaration makes no
// claim, so the sniffed type stands.
func resolveMIME(sniffed, declared string) (string, error) {
	if _, ok := allowedMIMEs[declared]; !ok {
		return "", apperrors.Invalid("mime is not allowed")
	}
	if sniffed != "application/octet-stream" {
		if _, ok := allowedMIMEs[sniffed]; !ok {
			return "", apperrors.Invalid("content type is not allowed")
		}
	}
	if declared == "application/octet-stream" {
		return sniffed, nil
	}
	if mimeContradicts(declared, sniffed) {
		return "", apperrors.Invalid("declared mime does not match the actual content")
	}
	return declared, nil
}

// mimeContradicts reports a declaration the bytes refute: a sniffable type
// that did not sniff as itself, or an aac/amr claim over recognizable content.
func mimeContradicts(declared, sniffed string) bool {
	if sniffable[declared] {
		return sniffed != declared
	}
	return sniffed != "application/octet-stream"
}

// sniffMIME reads the leading bytes of a stream's first chunk.
func sniffMIME(head []byte) string {
	return strings.TrimSpace(strings.SplitN(http.DetectContentType(head), ";", 2)[0])
}

// checkAvatarObject enforces the SPEC-01 R27 object-side constraints.
func checkAvatarObject(size int64, mime, status string) error {
	if status != ObjectReady {
		return apperrors.Unavail("media object is not ready")
	}
	if size > MaxAvatarSize {
		return apperrors.Invalid("avatar must be at most 10MB")
	}
	if !avatarMIMEs[mime] {
		return apperrors.Invalid("avatar must be image/jpeg, image/png or image/webp")
	}
	return nil
}

// objectKey derives the storage key for a finished object. It carries only
// server-generated values — the MIME lives in the media_objects row, never in
// the key — so a hostile file name cannot reach the storage layer (R10).
func objectKey(now time.Time, uuid string) string {
	return fmt.Sprintf("obj/%04d/%02d/%s", now.Year(), int(now.Month()), uuid)
}

// chunkKey is the staging key for chunk n of an upload (R5).
func chunkKey(uploadID string, n int) string {
	return fmt.Sprintf("tmp/%s/%d", uploadID, n)
}
