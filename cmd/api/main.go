// Command api is the HTTP composition root: it loads configuration, opens
// infrastructure, constructs every domain service in dependency order, mounts
// the route table, and runs the server with graceful shutdown (SPEC-12 §7).
package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/argon"
	"github.com/example/wechat/internal/audit"
	"github.com/example/wechat/internal/auth"
	"github.com/example/wechat/internal/contact"
	"github.com/example/wechat/internal/conversation"
	"github.com/example/wechat/internal/device"
	"github.com/example/wechat/internal/group"
	"github.com/example/wechat/internal/media"
	"github.com/example/wechat/internal/message"
	"github.com/example/wechat/internal/platform/cache"
	"github.com/example/wechat/internal/platform/clock"
	"github.com/example/wechat/internal/platform/config"
	"github.com/example/wechat/internal/platform/httpx"
	"github.com/example/wechat/internal/platform/logger"
	"github.com/example/wechat/internal/platform/mysqlx"
	"github.com/example/wechat/internal/platform/ratelimit"
	"github.com/example/wechat/internal/platform/sigtoken"
	"github.com/example/wechat/internal/platform/storage"
	"github.com/example/wechat/internal/user"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		panic("config: " + err.Error())
	}
	log := logger.New(cfg.Log.Level, cfg.Log.Format)

	db, err := mysqlx.Open(cfg.MySQL.DSN, cfg.MySQL.MaxOpenConns, cfg.MySQL.MaxIdleConns, cfg.MySQL.ConnMaxLifetime)
	if err != nil {
		log.Error("mysql: " + err.Error())
		os.Exit(1)
	}
	defer db.Close()

	clk := clock.System
	cacheStore := cache.New(cfg.Cache.Driver)

	var objectStore storage.ObjectStore
	var proxy storage.ProxyVerifier
	switch cfg.Storage.Driver {
	case "local":
		local, err := storage.NewLocal(cfg.Storage.LocalDir, cfg.Storage.PublicBaseURL,
			storage.NewSigner(cfg.Secret.Signing), clk.Now)
		if err != nil {
			log.Error("storage: " + err.Error())
			os.Exit(1)
		}
		objectStore, proxy = local, local
	case "s3":
		// Phase-2 adapter (SPEC-00). Until then the local driver is the only
		// one, and S3 deployments hand out pre-signed URLs (proxy == nil).
		log.Error("s3 storage driver not yet implemented; set WECHAT_STORAGE_DRIVER=local")
		os.Exit(1)
	}

	// Domain services, dependency order (SPEC-12 §7 step 4).
	auditSvc := audit.New(clk)
	convs := conversation.New(db, clk)
	devices := device.New(db, cacheStore, clk)
	mediaSvc := media.New(db, objectStore, proxy, cfg.Auth.DownloadURLTTL, clk)
	users := user.New(db, clk.Now, mediaSvc)
	qrCodec := sigtoken.New(cfg.Secret.Signing)
	authSvc := auth.New(db, devices, users, convs, auditSvc, cacheStore, log, clk, auth.Config{
		AccessTTL:     cfg.Auth.AccessTTL,
		RefreshTTL:    cfg.Auth.RefreshTTL,
		LoginMaxFails: cfg.Auth.LoginMaxFails,
		LoginFreeze:   cfg.Auth.LoginFreeze,
		Argon: argon.Params{
			Time:    cfg.Auth.ArgonTime,
			Memory:  cfg.Auth.ArgonMemoryKiB,
			Threads: cfg.Auth.ArgonThreads,
			KeyLen:  cfg.Auth.ArgonKeyLen,
			SaltLen: 16,
		},
	})
	contacts := contact.New(db, users, convs, auditSvc, cacheStore, qrCodec, clk)
	groups := group.New(db, convs, contacts, nil, clk.Now)
	messages := message.New(db, contacts, groupMessageAdapter{g: groups}, mediaSvc, convs, clk.Now)
	groups.SetMessenger(messages)

	// Rate limits (SPEC-12 §7 default table, fail-open).
	authLimiter := ratelimit.New(cacheStore, "auth", 20, 10*time.Minute)
	writeLimiter := ratelimit.New(cacheStore, "write", 100, time.Minute)
	chunkLimiter := ratelimit.New(cacheStore, "media_chunk", 600, time.Minute)
	readLimiter := ratelimit.New(cacheStore, "read", 300, time.Minute)

	mux := buildRouter(routerServices{
		auth: authSvc, devices: devices, users: users, contacts: contacts,
		groups: groups, media: mediaSvc, messages: messages,
		authn: authSvc,
		limiters: limiters{auth: authLimiter, write: writeLimiter, chunk: chunkLimiter, read: readLimiter},
	})

	srv := &http.Server{
		Addr:         cfg.HTTP.Addr,
		Handler:      mux,
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
		IdleTimeout:  120 * time.Second,
	}

	ctx := context.Background()
	go func() {
		log.Info("api listening", "addr", cfg.HTTP.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen: " + err.Error())
			os.Exit(1)
		}
	}()

	// Graceful shutdown (SPEC-12 §7 step 7): drain, then close.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	log.Info("shutting down")

	shutCtx, cancel := context.WithTimeout(ctx, cfg.HTTP.ShutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	log.Info("stopped")
}

// routerServices groups the handlers the route table needs.
type routerServices struct {
	auth     *auth.Service
	devices  *device.Service
	users    *user.Service
	contacts *contact.Service
	groups   *group.Service
	media    *media.Service
	messages *message.Service
	authn    httpx.Authenticator
	limiters limiters
}

type limiters struct {
	auth  *ratelimit.Limiter
	write *ratelimit.Limiter
	chunk *ratelimit.Limiter
	read  *ratelimit.Limiter
}

// groupMessageAdapter adapts *group.Service to message.Group without importing
// the group types across modules (ADR-013: message depends on group's surface).
type groupMessageAdapter struct{ g *group.Service }

func (a groupMessageAdapter) CanSend(ctx context.Context, groupID, userID int64) error {
	return a.g.CanSend(ctx, groupID, userID)
}
func (a groupMessageAdapter) IsModerator(ctx context.Context, groupID, userID int64) (bool, error) {
	return a.g.IsModerator(ctx, groupID, userID)
}
func (a groupMessageAdapter) GroupByConversation(ctx context.Context, conversationID int64) (message.GroupRowView, error) {
	g, err := a.g.GroupByConversation(ctx, conversationID)
	if err != nil {
		return message.GroupRowView{}, err
	}
	return message.GroupRowView{
		GroupID: g.ID, Name: g.Name, Status: g.Status, ConversationID: g.ConversationID,
	}, nil
}
func (a groupMessageAdapter) WasMemberAt(ctx context.Context, groupID, userID int64, at time.Time) (bool, error) {
	return a.g.WasMemberAt(ctx, groupID, userID, at)
}
func (a groupMessageAdapter) ListGroupConversationsForUser(ctx context.Context, userID int64) ([]message.GroupConvView, error) {
	rows, err := a.g.ListGroupConversationsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]message.GroupConvView, len(rows))
	for i, r := range rows {
		out[i] = message.GroupConvView{ConversationID: r.ConversationID, GroupID: r.GroupID, Name: r.Name}
	}
	return out, nil
}

// limit wraps a handler with a named limiter. The subject is the authenticated
// user id when present, otherwise the client IP — so the same middleware works
// for public (login) and authed routes alike (SPEC-12 §7 rate table).
func (s routerServices) limit(l *ratelimit.Limiter, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject := clientSubject(r)
		if p, ok := httpx.PrincipalFrom(r.Context()); ok && p.UserID > 0 {
			subject = "u" + strconv.FormatInt(p.UserID, 10)
		}
		ok, retry := l.Allow(r.Context(), subject)
		if !ok {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(retry.Seconds()), 10))
			httpx.Error(w, r, apperrors.New(apperrors.RateLimited, "too many requests"))
			return
		}
		h(w, r)
	})
}

// clientSubject extracts the bare client IP (no port) for rate-limit keying.
// Behind a proxy the real address arrives in X-Forwarded-For; direct conns
// arrive as host:port via RemoteAddr.
func clientSubject(r *http.Request) string {
	ip := httpx.ClientIP(r)
	if host, _, err := net.SplitHostPort(ip); err == nil {
		return host
	}
	return ip
}

// buildRouter mounts every route on the standard-library mux with the
// middleware chain RequestID → Recover → RateLimit → RequireAuth.
func buildRouter(s routerServices) http.Handler {
	authH := auth.NewHandler(s.auth)
	userH := user.NewHandler(s.users)
	contactH := contact.NewHandler(s.contacts)
	groupH := group.NewHandler(s.groups)
	mediaH := media.NewHandler(s.media)
	messageH := message.NewHandler(s.messages)
	deviceH := device.NewHandler(s.devices)

	mux := http.NewServeMux()

	// Root-level: health and readiness (no auth, no rate limit).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		// Phase 1: process is ready if it can serve. A real DB ping belongs to
		// the runtime module (task G) — keep the surface honest.
		httpx.JSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
	})

	// authed wraps a handler with RequireAuth and the general write limiter.
	authed := func(h http.HandlerFunc) http.Handler {
		return httpx.RequestID(httpx.Recover(httpx.RequireAuth(s.authn)(s.limit(s.limiters.write, h))))
	}
	// read applies the read limiter to an already-authed handler.
	read := func(h http.HandlerFunc) http.Handler {
		return httpx.RequestID(httpx.Recover(httpx.RequireAuth(s.authn)(s.limit(s.limiters.read, h))))
	}
	// chunk applies the dedicated media-chunk limiter (600/min) to upload parts.
	chunk := func(h http.HandlerFunc) http.Handler {
		return httpx.RequestID(httpx.Recover(httpx.RequireAuth(s.authn)(s.limit(s.limiters.chunk, h))))
	}
	// public wraps a handler with no auth (login/register) and the auth limiter.
	public := func(h http.HandlerFunc) http.Handler {
		return httpx.RequestID(httpx.Recover(s.limit(s.limiters.auth, h)))
	}

	// Auth (SPEC-01 §4).
	mux.Handle("POST /api/v1/auth/register", public(authH.Register))
	mux.Handle("POST /api/v1/auth/login", public(authH.Login))
	mux.Handle("POST /api/v1/auth/refresh", public(authH.Refresh))
	mux.Handle("POST /api/v1/auth/logout", authed(authH.Logout))
	mux.Handle("POST /api/v1/auth/password", authed(authH.ChangePassword))

	// Devices.
	mux.Handle("GET /api/v1/devices", read(deviceH.List))
	mux.Handle("DELETE /api/v1/devices/{deviceID}", authed(deviceH.Revoke))
	mux.Handle("POST /api/v1/devices/logout-others", authed(deviceH.LogoutOthers))

	// Users.
	mux.Handle("GET /api/v1/users/me", read(userH.GetMe))
	mux.Handle("PATCH /api/v1/users/me", authed(userH.UpdateMe))
	mux.Handle("POST /api/v1/users/me/avatar", authed(userH.SetAvatar))
	mux.Handle("GET /api/v1/users/{id}", read(userH.GetUser))

	// Contacts (SPEC-02 §4).
	mux.Handle("POST /api/v1/contacts/requests", authed(contactH.CreateRequest))
	mux.Handle("GET /api/v1/contacts/requests", read(contactH.ListRequests))
	mux.Handle("POST /api/v1/contacts/requests/{id}/accept", authed(contactH.AcceptRequest))
	mux.Handle("POST /api/v1/contacts/requests/{id}/reject", authed(contactH.RejectRequest))
	mux.Handle("POST /api/v1/contacts/requests/{id}/cancel", authed(contactH.CancelRequest))
	mux.Handle("DELETE /api/v1/contacts/friends/{friendID}", authed(contactH.DeleteFriend))
	mux.Handle("PATCH /api/v1/contacts/friends/{friendID}/settings", authed(contactH.UpdateFriendSettings))
	mux.Handle("GET /api/v1/contacts/friends", read(contactH.ListFriends))
	mux.Handle("GET /api/v1/contacts/friends/search", read(contactH.SearchFriends))
	mux.Handle("GET /api/v1/contacts/lookup", read(contactH.Lookup))
	mux.Handle("GET /api/v1/contacts/qrcode", read(contactH.QRCode))
	mux.Handle("GET /api/v1/contacts/tags", read(contactH.ListTags))
	mux.Handle("POST /api/v1/contacts/tags", authed(contactH.CreateTag))
	mux.Handle("PATCH /api/v1/contacts/tags/{id}", authed(contactH.RenameTag))
	mux.Handle("DELETE /api/v1/contacts/tags/{id}", authed(contactH.DeleteTag))
	mux.Handle("POST /api/v1/contacts/tags/{id}/members", authed(contactH.AddTagMembers))
	mux.Handle("DELETE /api/v1/contacts/tags/{id}/members", authed(contactH.RemoveTagMembers))

	// Groups (SPEC-05 §5).
	group.Mount(mux, groupH, authed)

	// Messages (SPEC-04 §5).
	message.Mount(mux, messageH, authed)

	// Media (SPEC-03 §4).
	mux.Handle("POST /api/v1/media/uploads", authed(mediaH.CreateUpload))
	mux.Handle("GET /api/v1/media/uploads/{id}", read(mediaH.GetUpload))
	mux.Handle("PUT /api/v1/media/uploads/{id}/chunks/{n}", chunk(mediaH.PutChunk))
	mux.Handle("POST /api/v1/media/uploads/{id}/complete", authed(mediaH.CompleteUpload))
	mux.Handle("POST /api/v1/media/uploads/{id}/abort", authed(mediaH.AbortUpload))
	mux.Handle("GET /api/v1/media/objects/{id}/download-url", read(mediaH.DownloadURL))
	// The byte proxy is public: the signed token is the credential (R16).
	mux.Handle("GET /api/v1/media/download", public(mediaH.Download))

	return httpx.RequestID(httpx.Recover(mux))
}
