//go:build integration

package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example/wechat/internal/platform/clock"
	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/httpx"
	"github.com/example/wechat/internal/platform/mysqlx"
	"github.com/example/wechat/internal/platform/storage"
	"github.com/example/wechat/internal/user"
)

// env is one live media stack: real MySQL, real local object store, fake clock.
type env struct {
	db    *sql.DB
	svc   *Service
	store *storage.Local
	users *user.Service
	clk   *clock.Fake
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("WECHAT_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("WECHAT_TEST_MYSQL_DSN is not set")
	}
	db, err := mysqlx.Open(dsn, 8, 4, time.Minute)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := clock.NewFake(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC))
	store, err := storage.NewLocal(t.TempDir(), "https://cdn.example.test",
		storage.NewSigner("media-test-secret"), clk.Now)
	if err != nil {
		t.Fatalf("local store: %v", err)
	}
	svc := New(db, store, store, 15*time.Minute, clk)
	users := user.New(db, clk.Now, svc)
	return &env{db: db, svc: svc, store: store, users: users, clk: clk}
}

var seq int64

func (e *env) newUser(t *testing.T) int64 {
	t.Helper()
	seq++
	var id int64
	err := mysqlx.WithinTx(context.Background(), e.db, func(tx mysqlx.Tx) error {
		tag := fmt.Sprintf("%08d%04d", time.Now().UnixNano()%100000000, seq)
		var err error
		id, err = user.CreateAccountTx(context.Background(), tx, user.CreateAccountParams{
			Phone: "+86" + tag, AccountName: "m" + tag, PasswordHash: "x", Nickname: "nick" + tag,
		}, e.clk.Now())
		return err
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func shaOf(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// upload drives a full upload: declare, send every part, complete.
func (e *env) upload(t *testing.T, owner int64, data []byte, purpose, mime string, declaredSHA string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	row, err := e.svc.CreateSession(ctx, owner, CreateInput{
		FileName: "photo.bin", Size: int64(len(data)), MIME: mime, SHA256: declaredSHA, Purpose: purpose,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	for n := 1; n <= row.TotalChunks; n++ {
		lo := (n - 1) * int(row.ChunkSize)
		hi := lo + int(row.ChunkSize)
		if hi > len(data) {
			hi = len(data)
		}
		if err := e.svc.PutChunk(ctx, owner, row.ID, n, bytes.NewReader(data[lo:hi])); err != nil {
			t.Fatalf("put chunk %d: %v", n, err)
		}
	}
	objectID, err := e.svc.CompleteUpload(ctx, owner, row.ID)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	return objectID, row.ID
}

// A1: the chunk geometry follows the declared size (R3).
func TestA1ChunkGeometry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.newUser(t)

	small := make([]byte, 10<<20)
	objectID, _ := e.upload(t, owner, small, PurposeMessage, "application/octet-stream", shaOf(small))
	if objectID <= 0 {
		t.Fatal("single-part upload failed")
	}

	big := make([]byte, 12<<20)
	rand.Read(big)
	row, err := e.svc.CreateSession(ctx, owner, CreateInput{
		FileName: "big.bin", Size: int64(len(big)), MIME: "application/octet-stream",
		SHA256: shaOf(big), Purpose: PurposeMessage,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if row.TotalChunks != 3 || row.ChunkSize != 5<<20 {
		t.Fatalf("plan = %+v, want 3 parts of 5MB", row)
	}
	for n := 1; n <= 3; n++ {
		lo := (n - 1) * 5 << 20
		hi := lo + 5<<20
		if hi > len(big) {
			hi = len(big)
		}
		if err := e.svc.PutChunk(ctx, owner, row.ID, n, bytes.NewReader(big[lo:hi])); err != nil {
			t.Fatalf("put %d: %v", n, err)
		}
	}
	if _, err := e.svc.CompleteUpload(ctx, owner, row.ID); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if code := codeOf(t, e.svc.PutChunk(ctx, owner, row.ID, 1, bytes.NewReader(big[:5<<20]))); code != apperrors.StateConflict {
		t.Fatalf("put after complete: code = %s", code)
	}
}

// A2/R6: a declaration the content contradicts leaves no object behind and
// keeps the session open for a corrected retry.
func TestA2DeclarationMismatch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.newUser(t)

	pngData := pngBytes(t)
	row, err := e.svc.CreateSession(ctx, owner, CreateInput{
		FileName: "avatar.png", Size: int64(len(pngData)), MIME: "image/png",
		SHA256: shaOf(pngData), Purpose: PurposeAvatar,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PutChunk(ctx, owner, row.ID, 1, bytes.NewReader(pngData)); err != nil {
		t.Fatal(err)
	}
	// Corrupt the declaration twice; each attempt fails without leaving an
	// object behind, and the session stays open for a corrected retry.
	if err := e.completeTampered(ctx, owner, row.ID, int64(len(pngData))+1, shaOf(pngData)); codeOf(t, err) != apperrors.InvalidArgument {
		t.Fatalf("size mismatch: %v", err)
	}
	if err := e.completeTampered(ctx, owner, row.ID, int64(len(pngData)), strings.Repeat("a", 64)); codeOf(t, err) != apperrors.InvalidArgument {
		t.Fatalf("hash mismatch: %v", err)
	}
	if err := e.completeTampered(ctx, owner, row.ID, int64(len(pngData)), shaOf(pngData)); err != nil {
		t.Fatalf("corrected retry must succeed: %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM media_objects WHERE owner_id = ?`, owner); n != 1 {
		t.Fatalf("objects = %d, want 1", n)
	}
}

// completeTampered rewrites the session's declaration directly in the database
// and then attempts the complete, simulating a client whose bytes do not match
// what it declared.
func (e *env) completeTampered(ctx context.Context, owner int64, id string, size int64, sha string) error {
	if _, err := e.db.ExecContext(ctx, `
		UPDATE upload_sessions SET declared_size = ?, declared_sha256 = ? WHERE id = ?`, size, sha, id); err != nil {
		return err
	}
	_, err := e.svc.CompleteUpload(ctx, owner, id)
	return err
}

// A3/§5: completing twice returns the same object and produces one row.
func TestA3CompleteIsIdempotent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.newUser(t)

	data := pngBytes(t)
	row, err := e.svc.CreateSession(ctx, owner, CreateInput{
		FileName: "a.png", Size: int64(len(data)), MIME: "image/png", SHA256: shaOf(data), Purpose: PurposeAvatar,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PutChunk(ctx, owner, row.ID, 1, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	first, err := e.svc.CompleteUpload(ctx, owner, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.svc.CompleteUpload(ctx, owner, row.ID)
	if err != nil {
		t.Fatalf("re-complete: %v", err)
	}
	if first != second {
		t.Fatalf("ids differ: %d vs %d", first, second)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM media_objects WHERE owner_id = ?`, owner); n != 1 {
		t.Fatalf("objects = %d, want 1", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM outbox_events WHERE type = 'media.process' AND aggregate_id = ?`,
		strconv.FormatInt(first, 10)); n != 1 {
		t.Fatalf("media.process events = %d, want 1", n)
	}
}

// A3/§5: concurrent completers may stage the object, but exactly one commits it.
func TestA3ConcurrentCompleteAssemblesOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.newUser(t)
	data := pngBytes(t)
	row, err := e.svc.CreateSession(ctx, owner, CreateInput{
		FileName: "race.png", Size: int64(len(data)), MIME: "image/png",
		SHA256: shaOf(data), Purpose: PurposeAvatar,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PutChunk(ctx, owner, row.ID, 1, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	const completers = 12
	ids := make([]int64, completers)
	errs := make([]error, completers)
	var wg sync.WaitGroup
	for i := 0; i < completers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = e.svc.CompleteUpload(ctx, owner, row.ID)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("completer %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("completer %d got object %d, want %d", i, ids[i], ids[0])
		}
	}
	if n := e.count(t, `SELECT COUNT(*) FROM media_objects WHERE owner_id = ?`, owner); n != 1 {
		t.Fatalf("objects = %d, want 1", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM outbox_events WHERE type = 'media.process' AND aggregate_id = ?`,
		strconv.FormatInt(ids[0], 10)); n != 1 {
		t.Fatalf("media.process events = %d, want 1", n)
	}
}

// A4: sessions and objects are invisible to anyone but their owner.
func TestA4Ownership(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner, stranger := e.newUser(t), e.newUser(t)
	data := pngBytes(t)
	objectID, sessionID := e.upload(t, owner, data, PurposeAvatar, "image/png", shaOf(data))

	if _, _, err := e.svc.GetSession(ctx, stranger, sessionID); codeOf(t, err) != apperrors.ResourceUnavailable {
		t.Fatalf("stranger session: %v", err)
	}
	if err := e.svc.PutChunk(ctx, stranger, sessionID, 1, bytes.NewReader(data)); codeOf(t, err) != apperrors.ResourceUnavailable {
		t.Fatalf("stranger chunk: %v", err)
	}
	if _, err := e.svc.CompleteUpload(ctx, stranger, sessionID); codeOf(t, err) != apperrors.ResourceUnavailable {
		t.Fatalf("stranger complete: %v", err)
	}
	if err := e.svc.AbortUpload(ctx, stranger, sessionID); codeOf(t, err) != apperrors.ResourceUnavailable {
		t.Fatalf("stranger abort: %v", err)
	}
	// Not the owner and not bound as a visible avatar → FORBIDDEN.
	if _, err := e.svc.DownloadURL(ctx, stranger, objectID); codeOf(t, err) != apperrors.Forbidden {
		t.Fatalf("stranger download: %v", err)
	}
}

// A5: signed URLs are short-lived, tamper-proof and re-authorized per request.
func TestA5DownloadURLLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner, stranger := e.newUser(t), e.newUser(t)
	data := pngBytes(t)
	objectID, _ := e.upload(t, owner, data, PurposeAvatar, "image/png", shaOf(data))

	// Non-avatar object: stranger gets nothing.
	other, _ := e.upload(t, owner, make([]byte, 1024), PurposeMessage, "application/octet-stream", shaOf(make([]byte, 1024)))
	if _, err := e.svc.DownloadURL(ctx, stranger, other); codeOf(t, err) != apperrors.Forbidden {
		t.Fatalf("stranger on non-avatar object: %v", err)
	}
	if _, err := e.svc.DownloadURL(ctx, owner, other); err != nil {
		t.Fatalf("owner: %v", err)
	}

	// Bind it as an avatar: now any logged-in user may fetch (R12 phase 1).
	if err := e.users.SetAvatar(ctx, owner, objectID); err != nil {
		t.Fatalf("bind avatar: %v", err)
	}
	link, err := e.svc.DownloadURL(ctx, stranger, objectID)
	if err != nil {
		t.Fatalf("avatar download for stranger: %v", err)
	}
	u, err := url.Parse(link)
	if err != nil || u.Host != "cdn.example.test" {
		t.Fatalf("url = %q", link)
	}
	token := u.Query().Get("token")

	// The proxy streams the exact bytes (R16).
	body, obj, err := e.svc.ProxyDownload(ctx, token)
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("proxied bytes differ from the uploaded object")
	}
	if obj.MIME != "image/png" {
		t.Fatalf("mime = %q", obj.MIME)
	}

	if _, _, err := e.svc.ProxyDownload(ctx, token+"x"); codeOf(t, err) != apperrors.Forbidden {
		t.Fatalf("tampered token: %v", err)
	}
	e.clk.Advance(16 * time.Minute)
	if _, _, err := e.svc.ProxyDownload(ctx, token); codeOf(t, err) != apperrors.Forbidden {
		t.Fatalf("expired token: %v", err)
	}

	// R15: a cleaned object refuses a fresh URL even for its owner.
	if _, err := e.db.ExecContext(ctx,
		`UPDATE media_objects SET status = 'cleaned' WHERE id = ?`, objectID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.DownloadURL(ctx, owner, objectID); codeOf(t, err) != apperrors.ResourceUnavailable {
		t.Fatalf("cleaned object: %v", err)
	}
}

// A6/R7: the expiry sweep deletes staged parts and closes the session.
func TestA6ExpireUploads(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.newUser(t)

	data := make([]byte, 12<<20)
	row, err := e.svc.CreateSession(ctx, owner, CreateInput{
		FileName: "big.bin", Size: int64(len(data)), MIME: "application/octet-stream",
		SHA256: shaOf(data), Purpose: PurposeMessage,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PutChunk(ctx, owner, row.ID, 1, bytes.NewReader(data[:5<<20])); err != nil {
		t.Fatal(err)
	}

	e.clk.Advance(SessionTTL + time.Minute)
	if _, err := e.svc.ExpireUploads(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, err := e.svc.GetSession(ctx, owner, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != SessionExpired {
		t.Fatalf("status = %s", got.Status)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM upload_chunks WHERE upload_id = ?`, row.ID); n != 0 {
		t.Fatalf("chunk rows after expiry = %d", n)
	}
	if err := e.svc.PutChunk(ctx, owner, row.ID, 2, bytes.NewReader(data[5<<20:10<<20])); codeOf(t, err) != apperrors.StateConflict {
		t.Fatalf("put on expired session: %v", err)
	}
}

// Abort discards staged parts (§4).
func TestAbortUpload(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.newUser(t)

	data := pngBytes(t)
	row, err := e.svc.CreateSession(ctx, owner, CreateInput{
		FileName: "x.png", Size: int64(len(data)), MIME: "image/png", SHA256: shaOf(data), Purpose: PurposeAvatar,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PutChunk(ctx, owner, row.ID, 1, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AbortUpload(ctx, owner, row.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM upload_chunks WHERE upload_id = ?`, row.ID); n != 0 {
		t.Fatalf("chunk rows = %d", n)
	}
	if _, err := e.svc.CompleteUpload(ctx, owner, row.ID); codeOf(t, err) != apperrors.StateConflict {
		t.Fatalf("complete after abort: %v", err)
	}
	if code := codeOf(t, e.svc.AbortUpload(ctx, owner, row.ID)); code != apperrors.StateConflict {
		t.Fatalf("double abort: %s", code)
	}
}

func TestExpireUploadsKeepsCompletedSession(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	objectID, sessionID := e.upload(t, owner, pngBytes(t), PurposeAvatar, "image/png", shaOf(pngBytes(t)))

	e.clk.Advance(SessionTTL + time.Minute)
	n, err := e.svc.ExpireUploads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := e.svc.GetSession(context.Background(), owner, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != SessionCompleted || got.MediaObjectID != objectID {
		t.Fatalf("completed session changed: %+v", got)
	}
	_ = n
}

func TestGCTaskCleansUnreferencedObject(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.newUser(t)
	data := pngBytes(t)
	objectID, _ := e.upload(t, owner, data, PurposeMessage, "application/octet-stream", shaOf(data))
	obj, err := findObject(ctx, e.db, objectID)
	if err != nil {
		t.Fatal(err)
	}

	err = mysqlx.WithinTx(ctx, e.db, func(tx mysqlx.Tx) error {
		return e.svc.EnqueueGCTx(ctx, tx, []int64{objectID}, e.clk.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.ProcessGCTasks(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("processed = %d, want 1", n)
	}
	var status string
	if err := e.db.QueryRowContext(ctx, `SELECT status FROM media_objects WHERE id = ?`, objectID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != ObjectCleaned {
		t.Fatalf("object status = %q", status)
	}
	if exists, err := e.store.Exists(ctx, obj.BucketKey); err != nil || exists {
		t.Fatalf("object still exists: exists=%v err=%v", exists, err)
	}
	var taskStatus string
	if err := e.db.QueryRowContext(ctx, `SELECT status FROM media_gc_tasks WHERE object_id = ?`, objectID).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if taskStatus != "done" {
		t.Fatalf("GC task status = %q", taskStatus)
	}
}

// SPEC-01 R27: the avatar binding validates ownership, status, type and size,
// and commits references + profile atomically.
func TestR27AvatarBinding(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner, stranger := e.newUser(t), e.newUser(t)

	pngData := pngBytes(t)
	objectID, _ := e.upload(t, owner, pngData, PurposeAvatar, "image/png", shaOf(pngData))
	if err := e.users.SetAvatar(ctx, owner, objectID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM media_references WHERE object_id = ? AND biz_type = 'avatar'`, objectID); n != 1 {
		t.Fatalf("avatar references = %d", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM media_user_references WHERE object_id = ? AND user_id = ? AND revoked_at IS NULL`, objectID, owner); n != 1 {
		t.Fatalf("user references = %d", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM user_profiles WHERE user_id = ? AND avatar_media_id = ?`, owner, objectID); n != 1 {
		t.Fatal("profile avatar was not bound")
	}
	// Re-binding the same object is idempotent and appends no duplicate grant.
	if err := e.users.SetAvatar(ctx, owner, objectID); err != nil {
		t.Fatalf("re-bind: %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM media_user_references WHERE object_id = ? AND user_id = ? AND revoked_at IS NULL`, objectID, owner); n != 1 {
		t.Fatalf("duplicate grants appeared: %d", n)
	}

	if err := e.users.SetAvatar(ctx, owner, 999999999); codeOf(t, err) != apperrors.ResourceUnavailable {
		t.Fatalf("unknown object: %v", err)
	}

	gif := append([]byte("GIF89a"), make([]byte, 32)...)
	gifID, _ := e.upload(t, owner, gif, PurposeAvatar, "image/gif", shaOf(gif))
	if err := e.users.SetAvatar(ctx, owner, gifID); codeOf(t, err) != apperrors.InvalidArgument {
		t.Fatalf("gif avatar: %v", err)
	}

	if err := e.users.SetAvatar(ctx, stranger, objectID); codeOf(t, err) != apperrors.ResourceUnavailable {
		t.Fatalf("stranger's object: %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM user_profiles WHERE user_id = ? AND avatar_media_id IS NOT NULL`, stranger); n != 0 {
		t.Fatal("a failed binding must not write the profile")
	}
}

// ---------------------------------------------------------------------------
// HTTP surface (SPEC-03 §4).

type fakeAuthn struct{}

func (fakeAuthn) Authenticate(_ context.Context, token string) (httpx.Principal, error) {
	id, err := strconv.ParseInt(token, 10, 64)
	if err != nil || id <= 0 {
		return httpx.Principal{}, apperrors.Unauth("bad test token")
	}
	return httpx.Principal{UserID: id, DeviceID: "test", SessionID: 1}, nil
}

func (e *env) router() http.Handler {
	h := NewHandler(e.svc)
	auth := httpx.RequireAuth(fakeAuthn{})
	mux := http.NewServeMux()
	route := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, auth(fn)) }
	route("POST /api/v1/media/uploads", h.CreateUpload)
	route("GET /api/v1/media/uploads/{id}", h.GetUpload)
	route("PUT /api/v1/media/uploads/{id}/chunks/{n}", h.PutChunk)
	route("POST /api/v1/media/uploads/{id}/complete", h.CompleteUpload)
	route("POST /api/v1/media/uploads/{id}/abort", h.AbortUpload)
	route("GET /api/v1/media/objects/{id}/download-url", h.DownloadURL)
	// The byte proxy is public: the signed token is the credential (R16).
	mux.Handle("GET /api/v1/media/download", http.HandlerFunc(h.Download))
	return mux
}

type apiResp struct {
	status int
	code   string
	raw    string
	data   json.RawMessage
}

func (e *env) do(t *testing.T, method, path string, asUser int64, body string, contentType string) apiResp {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+strconv.FormatInt(asUser, 10))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	e.router().ServeHTTP(rec, req)
	var env struct {
		Code string          `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil && rec.Code != http.StatusNoContent {
		t.Fatalf("%s %s: bad envelope %q: %v", method, path, rec.Body.String(), err)
	}
	return apiResp{status: rec.Code, code: env.Code, raw: rec.Body.String(), data: env.Data}
}

func TestHTTPUploadLifecycle(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t)
	pngData := pngBytes(t)

	created := e.do(t, http.MethodPost, "/api/v1/media/uploads", owner, fmt.Sprintf(
		`{"file_name":"pic.png","size":%d,"mime":"image/png","sha256":%q,"purpose":"avatar"}`,
		len(pngData), shaOf(pngData)), "application/json")
	if created.status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", created.status, created.raw)
	}
	var session struct {
		UploadID    string `json:"upload_id"`
		ChunkSize   string `json:"chunk_size"`
		TotalChunks int    `json:"total_chunks"`
	}
	if err := json.Unmarshal(created.data, &session); err != nil {
		t.Fatal(err)
	}
	if session.TotalChunks != 1 || session.ChunkSize != strconv.Itoa(len(pngData)) {
		t.Fatalf("session = %+v", session)
	}

	if res := e.do(t, http.MethodPut, "/api/v1/media/uploads/"+session.UploadID+"/chunks/1", owner,
		string(pngData), "application/octet-stream"); res.status != http.StatusNoContent {
		t.Fatalf("put chunk: status=%d body=%s", res.status, res.raw)
	}
	if res := e.do(t, http.MethodPut, "/api/v1/media/uploads/"+session.UploadID+"/chunks/9", owner,
		"x", "application/octet-stream"); res.code != "INVALID_ARGUMENT" {
		t.Fatalf("out-of-range chunk: %s", res.raw)
	}

	status := e.do(t, http.MethodGet, "/api/v1/media/uploads/"+session.UploadID, owner, "", "")
	if !strings.Contains(status.raw, `"received_chunks":[1]`) {
		t.Fatalf("bitmap: %s", status.raw)
	}

	completed := e.do(t, http.MethodPost, "/api/v1/media/uploads/"+session.UploadID+"/complete", owner, "", "")
	if completed.status != http.StatusOK || !strings.Contains(completed.raw, `"media_object_id":`) {
		t.Fatalf("complete: status=%d body=%s", completed.status, completed.raw)
	}
	var done struct {
		MediaObjectID string `json:"media_object_id"`
	}
	if err := json.Unmarshal(completed.data, &done); err != nil {
		t.Fatal(err)
	}

	link := e.do(t, http.MethodGet, "/api/v1/media/objects/"+done.MediaObjectID+"/download-url", owner, "", "")
	var dl struct {
		URL string `json:"url"`
	}
	if link.status != http.StatusOK {
		t.Fatalf("download-url: status=%d body=%s", link.status, link.raw)
	}
	if err := json.Unmarshal(link.data, &dl); err != nil {
		t.Fatal(err)
	}

	// The signed proxy URL streams the exact bytes without any auth header.
	req := httptest.NewRequest(http.MethodGet, dl.URL, nil)
	rec := httptest.NewRecorder()
	e.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Bytes() == nil || !bytes.Equal(rec.Body.Bytes(), pngData) {
		t.Fatalf("proxy: status=%d len=%d", rec.Code, rec.Body.Len())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q", ct)
	}

	if res := e.do(t, http.MethodGet, "/api/v1/media/download?token=bogus", owner, "", ""); res.status != http.StatusForbidden {
		t.Fatalf("bogus token: status=%d", res.status)
	}
}
