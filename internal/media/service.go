package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"time"

	"github.com/example/wechat/internal/platform/clock"
	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/ids"
	"github.com/example/wechat/internal/platform/mq"
	"github.com/example/wechat/internal/platform/mysqlx"
	"github.com/example/wechat/internal/platform/outbox"
	"github.com/example/wechat/internal/platform/storage"
)

// errLostRace marks a concurrent complete that found the session already
// transitioned; the caller returns the winner's object (§5).
var errLostRace = errors.New("media: session already completed")

// Service owns upload sessions, chunk assembly, media objects and references,
// and signed downloads (SPEC-03). Bytes live behind the storage port; every
// state decision is made from MySQL rows only.
type Service struct {
	db    *sql.DB
	store storage.ObjectStore
	// proxy is non-nil only when the driver serves bytes through the API
	// (local). The composition root decides; S3 hands out pre-signed URLs.
	proxy storage.ProxyVerifier
	now   clock.Clock
	ttl   time.Duration
}

// New wires the media service. downloadTTL bounds signed URL validity (R14).
func New(db *sql.DB, store storage.ObjectStore, proxy storage.ProxyVerifier,
	downloadTTL time.Duration, clk clock.Clock) *Service {
	return &Service{db: db, store: store, proxy: proxy, now: clk, ttl: downloadTTL}
}

// CreateInput is one upload-session declaration (R1).
type CreateInput struct {
	FileName string
	Size     int64
	MIME     string
	SHA256   string
	Purpose  string
}

// CreateSession validates the declaration and opens a chunked upload (R1-R3).
func (s *Service) CreateSession(ctx context.Context, owner int64, in CreateInput) (SessionRow, error) {
	if err := validateFileName(in.FileName); err != nil {
		return SessionRow{}, err
	}
	if err := validatePurpose(in.Purpose); err != nil {
		return SessionRow{}, err
	}
	if err := ValidateSHA256(in.SHA256); err != nil {
		return SessionRow{}, err
	}
	if err := validateDeclaredMIME(in.MIME); err != nil {
		return SessionRow{}, err
	}
	plan, err := planChunks(in.Size)
	if err != nil {
		return SessionRow{}, err
	}

	now := s.now.Now()
	row := SessionRow{
		ID: ids.New(), OwnerID: owner, FileName: in.FileName,
		DeclaredSize: in.Size, DeclaredMIME: in.MIME, DeclaredSHA: in.SHA256,
		Purpose: in.Purpose, ChunkSize: plan.ChunkSize, TotalChunks: plan.TotalChunks,
		Status: SessionOpen, ExpiresAt: now.Add(SessionTTL), CreatedAt: now, UpdatedAt: now,
	}
	if err := insertSession(ctx, s.db, row); err != nil {
		return SessionRow{}, err
	}
	return row, nil
}

// GetSession returns the session and its received-chunk bitmap (R5).
func (s *Service) GetSession(ctx context.Context, owner int64, id string) (SessionRow, []int, error) {
	row, err := s.sessionFor(ctx, owner, id)
	if err != nil {
		return SessionRow{}, nil, err
	}
	got, err := chunkBitmap(ctx, s.db, id)
	if err != nil {
		return SessionRow{}, nil, err
	}
	return row, got, nil
}

// sessionFor enforces ownership and the open/not-expired precondition (R4).
func (s *Service) sessionFor(ctx context.Context, owner int64, id string) (SessionRow, error) {
	row, err := findSession(ctx, s.db, id)
	if err != nil {
		return SessionRow{}, err
	}
	if row.OwnerID != owner {
		return SessionRow{}, apperrors.Unavail("upload session not found")
	}
	return row, nil
}

// PutChunk stores one part's bytes and records its arrival (R4). Writing the
// blob before the row keeps the bitmap honest: a crash in between merely makes
// the client re-send the part, which overwrites the same key.
func (s *Service) PutChunk(ctx context.Context, owner int64, id string, n int, body io.Reader) error {
	row, err := s.sessionFor(ctx, owner, id)
	if err != nil {
		return err
	}
	if row.Status != SessionOpen {
		return apperrors.Conflict("upload session is " + row.Status)
	}
	if !s.now.Now().Before(row.ExpiresAt) {
		return apperrors.Unavail("upload session expired")
	}
	want := expectedChunkSize(n, row.TotalChunks, row.ChunkSize, row.DeclaredSize)
	if want < 0 {
		return apperrors.Invalid(fmt.Sprintf("chunk index must be 1..%d", row.TotalChunks))
	}
	data, err := io.ReadAll(io.LimitReader(body, want+1))
	if err != nil {
		return apperrors.Wrap(apperrors.InvalidArgument, "read chunk", err)
	}
	if int64(len(data)) != want {
		return apperrors.Invalid(fmt.Sprintf("chunk %d must be exactly %d bytes", n, want))
	}
	key := chunkKey(id, n)
	if err := s.store.Put(ctx, key, bytes.NewReader(data), want, "application/octet-stream"); err != nil {
		return apperrors.Wrap(apperrors.InternalError, "store chunk", err)
	}
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return putChunk(ctx, tx, id, n, want, s.now.Now())
	})
}

// CompleteUpload assembles the parts, re-derives size/mime/sha256 server-side
// and — only when everything matches — commits the object row, the session
// transition and the media.process event in one transaction (R6, §5).
func (s *Service) CompleteUpload(ctx context.Context, owner int64, id string) (int64, error) {
	row, err := s.sessionFor(ctx, owner, id)
	if err != nil {
		return 0, err
	}
	if row.Status == SessionCompleted {
		return row.MediaObjectID, nil
	}
	if row.Status != SessionOpen {
		return 0, apperrors.Conflict("upload session is " + row.Status)
	}
	if !s.now.Now().Before(row.ExpiresAt) {
		return 0, apperrors.Unavail("upload session expired")
	}
	missing, err := missingChunks(ctx, s.db, id, row.TotalChunks)
	if err != nil {
		return 0, err
	}
	if len(missing) > 0 {
		return 0, apperrors.Conflict(fmt.Sprintf("missing chunks: %v", missing))
	}

	pending, err := s.verifyAssembly(ctx, row)
	if err != nil {
		return 0, err
	}

	// One transaction commits the object row, the session transition and the
	// media.process event together. A lost race rolls all of them back, so a
	// loser never leaves an orphaned object row behind.
	var winner int64
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		objectID, err := insertObject(ctx, tx, ObjectRow{
			OwnerID: row.OwnerID, BucketKey: pending.key, Size: pending.size,
			SHA256: row.DeclaredSHA, MIME: pending.mime, Purpose: row.Purpose, CreatedAt: s.now.Now(),
		})
		if err != nil {
			return err
		}
		won, err := completeSession(ctx, tx, id, objectID, s.now.Now())
		if err != nil {
			return err
		}
		if !won {
			return errLostRace
		}
		winner = objectID
		return outbox.Emit(ctx, tx, outbox.Event{
			Type: "media.process", AggregateID: strconv.FormatInt(objectID, 10), Queue: mq.QueueMediaProcess,
		})
	})
	if errors.Is(err, errLostRace) {
		_ = s.store.Delete(ctx, pending.key)
		current, err := findSession(ctx, s.db, id)
		if err != nil {
			return 0, err
		}
		return current.MediaObjectID, nil
	}
	if err != nil {
		_ = s.store.Delete(ctx, pending.key)
		return 0, err
	}

	for n := 1; n <= row.TotalChunks; n++ {
		_ = s.store.Delete(ctx, chunkKey(id, n))
	}
	return winner, nil
}

// assembly is a verified blob waiting for its commit decision.
type assembly struct {
	key  string
	mime string
	size int64
}

// verifyAssembly streams the parts through the hash/size/head collector into
// the final object key and re-derives size/mime/sha256 server-side (R6/R8/R9).
// It touches storage only — no database rows — so the caller's transaction is
// the single commit point for the object's existence. The staged blob is
// deleted whenever verification fails.
func (s *Service) verifyAssembly(ctx context.Context, row SessionRow) (assembly, error) {
	keys := make([]string, row.TotalChunks)
	for n := 1; n <= row.TotalChunks; n++ {
		keys[n-1] = chunkKey(row.ID, n)
	}
	key := objectKey(s.now.Now(), ids.New())
	sink := &hashSink{sha: sha256.New()}

	if err := s.store.Put(ctx, key, io.TeeReader(&chunkSequence{store: s.store, keys: keys}, sink),
		row.DeclaredSize, row.DeclaredMIME); err != nil {
		_ = s.store.Delete(ctx, key)
		return assembly{}, apperrors.Wrap(apperrors.InternalError, "assemble object", err)
	}
	if sink.size != row.DeclaredSize {
		_ = s.store.Delete(ctx, key)
		return assembly{}, apperrors.Invalid(fmt.Sprintf(
			"declared size %d does not match actual %d", row.DeclaredSize, sink.size))
	}
	if sum := fmt.Sprintf("%x", sink.sha.Sum(nil)); sum != row.DeclaredSHA {
		_ = s.store.Delete(ctx, key)
		return assembly{}, apperrors.Invalid("declared sha256 does not match actual content")
	}
	mime, err := resolveMIME(sniffMIME(sink.head), row.DeclaredMIME)
	if err != nil {
		_ = s.store.Delete(ctx, key)
		return assembly{}, err
	}
	return assembly{key: key, mime: mime, size: sink.size}, nil
}

// AbortUpload discards the staged parts and closes the session (§4).
func (s *Service) AbortUpload(ctx context.Context, owner int64, id string) error {
	row, err := s.sessionFor(ctx, owner, id)
	if err != nil {
		return err
	}
	if row.Status != SessionOpen {
		return apperrors.Conflict("upload session is " + row.Status)
	}
	if err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return closeSession(ctx, tx, id, SessionAborted, s.now.Now())
	}); err != nil {
		return err
	}
	for n := 1; n <= row.TotalChunks; n++ {
		_ = s.store.Delete(ctx, chunkKey(id, n))
	}
	return nil
}

// ExpireUploads sweeps open sessions past their deadline (R7). Scheduling
// belongs to the phase-2 worker; the rule lives here with its tables.
func (s *Service) ExpireUploads(ctx context.Context) (int, error) {
	now := s.now.Now()
	stale, err := expireStaleSessions(ctx, s.db, now)
	if err != nil {
		return 0, err
	}
	for _, id := range stale {
		row, err := findSession(ctx, s.db, id)
		if err != nil {
			return 0, err
		}
		if err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
			return closeSession(ctx, tx, id, SessionExpired, now)
		}); err != nil {
			return 0, err
		}
		for n := 1; n <= row.TotalChunks; n++ {
			_ = s.store.Delete(ctx, chunkKey(id, n))
		}
	}
	return len(stale), nil
}

// DownloadURL authorizes the request and returns a fresh short-lived URL (R14,
// R15). URLs are never persisted, so a revoked permission simply fails the
// next issuance.
func (s *Service) DownloadURL(ctx context.Context, requester, objectID int64) (string, error) {
	obj, err := findObject(ctx, s.db, objectID)
	if err != nil {
		return "", err
	}
	if obj.Status != ObjectReady {
		return "", apperrors.Unavail("media object is not available")
	}
	if obj.OwnerID != requester {
		avatar, err := isAvatarObject(ctx, s.db, objectID)
		if err != nil {
			return "", err
		}
		if !avatar {
			return "", apperrors.New(apperrors.Forbidden, "not allowed to download this object")
		}
	}
	url, err := s.store.SignGetURL(ctx, obj.BucketKey, s.ttl)
	if err != nil {
		return "", apperrors.Wrap(apperrors.InternalError, "sign download", err)
	}
	return url, nil
}

// ProxyDownload verifies a proxy token and streams the object bytes (R16).
// It exists only for drivers that serve through the API; the composition root
// wires the proxy verifier, so S3 deployments answer RESOURCE_UNAVAILABLE.
func (s *Service) ProxyDownload(ctx context.Context, token string) (io.ReadCloser, ObjectRow, error) {
	if s.proxy == nil {
		return nil, ObjectRow{}, apperrors.Unavail("download proxy is not enabled")
	}
	key, err := s.proxy.VerifyToken(token, s.now.Now())
	if err != nil {
		return nil, ObjectRow{}, apperrors.New(apperrors.Forbidden, "invalid or expired download token")
	}
	obj, err := findObjectByKey(ctx, s.db, key)
	if err != nil {
		return nil, ObjectRow{}, err
	}
	if obj.Status != ObjectReady {
		return nil, ObjectRow{}, apperrors.Unavail("media object is not available")
	}
	body, err := s.store.Get(ctx, key)
	if err != nil {
		return nil, ObjectRow{}, apperrors.Wrap(apperrors.InternalError, "read object", err)
	}
	return body, obj, nil
}

// BindAvatarTx enforces the SPEC-01 R27 object-side constraints and records the
// avatar business + user references on the caller's transaction, so the user
// module can commit profile and references atomically (ADR-001: media tables
// are written only by media).
func (s *Service) BindAvatarTx(ctx context.Context, tx mysqlx.Tx, objectID, userID int64) error {
	obj, err := findObject(ctx, tx, objectID)
	if err != nil {
		return err
	}
	if obj.OwnerID != userID {
		return apperrors.Unavail("media object not found")
	}
	if err := checkAvatarObject(obj.Size, obj.MIME, obj.Status); err != nil {
		return err
	}
	refID, err := insertReference(ctx, tx, objectID, RefAvatar, strconv.FormatInt(userID, 10), s.now.Now())
	if err != nil {
		return err
	}
	return ensureUserReference(ctx, tx, objectID, userID, refID, s.now.Now())
}

// hashSink accumulates the byte count, the content hash and the leading bytes
// while the assembled stream passes through (R6/R9).
type hashSink struct {
	sha  hash.Hash
	size int64
	head []byte
}

func (h *hashSink) Write(p []byte) (int, error) {
	if _, err := h.sha.Write(p); err != nil {
		return 0, err
	}
	if room := sniffLen - len(h.head); room > 0 {
		h.head = append(h.head, p[:min(room, len(p))]...)
	}
	h.size += int64(len(p))
	return len(p), nil
}

// chunkSequence concatenates the stored parts, opening one at a time so an
// assembly never holds more than one part handle.
type chunkSequence struct {
	store storage.ObjectStore
	keys  []string
	cur   io.ReadCloser
}

func (c *chunkSequence) Read(p []byte) (int, error) {
	for {
		if c.cur == nil {
			if len(c.keys) == 0 {
				return 0, io.EOF
			}
			rc, err := c.store.Get(context.Background(), c.keys[0])
			if err != nil {
				return 0, fmt.Errorf("media: open chunk: %w", err)
			}
			c.cur = rc
			c.keys = c.keys[1:]
		}
		n, err := c.cur.Read(p)
		if n > 0 {
			return n, nil
		}
		if errors.Is(err, io.EOF) {
			_ = c.cur.Close()
			c.cur = nil
			continue
		}
		if err != nil {
			return 0, err
		}
	}
}
