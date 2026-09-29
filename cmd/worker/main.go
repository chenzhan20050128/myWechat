// Command worker runs background jobs: message retention sweep (R26), group
// todo overdue scan, group mute prune, contact stale request expiry. It is the
// single process all periodic jobs share (SPEC-10 will expand this scheduler).
package main

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/example/wechat/internal/backup"
	"github.com/example/wechat/internal/contact"
	"github.com/example/wechat/internal/content"
	"github.com/example/wechat/internal/conversation"
	"github.com/example/wechat/internal/group"
	"github.com/example/wechat/internal/media"
	"github.com/example/wechat/internal/message"
	"github.com/example/wechat/internal/moment"
	"github.com/example/wechat/internal/platform/cache"
	"github.com/example/wechat/internal/platform/clock"
	"github.com/example/wechat/internal/platform/config"
	"github.com/example/wechat/internal/platform/logger"
	"github.com/example/wechat/internal/platform/mysqlx"
	"github.com/example/wechat/internal/platform/storage"
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
	_ = cacheStore

	var objectStore storage.ObjectStore
	switch cfg.Storage.Driver {
	case "local":
		local, err := storage.NewLocal(cfg.Storage.LocalDir, cfg.Storage.PublicBaseURL,
			storage.NewSigner(cfg.Secret.Signing), clk.Now)
		if err != nil {
			log.Error("storage: " + err.Error())
			os.Exit(1)
		}
		objectStore = local
	default:
		log.Error("unsupported storage driver: " + cfg.Storage.Driver)
		os.Exit(1)
	}

	convs := conversation.New(db, clk)
	mediaSvc := media.New(db, objectStore, nil, cfg.Auth.DownloadURLTTL, clk)
	contacts := contact.New(db, nil, convs, nil, cacheStore, nil, clk)
	groups := group.New(db, convs, contacts, nil, clk.Now)
	messages := message.New(db, contacts, groupMessageAdapter{g: groups}, mediaSvc, convs, clk.Now)
	groups.SetMessenger(messages)
	moments := moment.New(db, contactMomentAdapter{c: contacts}, mediaSvc, nil, clk.Now)
	backups := backup.New(db, backupSourcesStub{}, clk.Now)
	contentSvc := content.New(db, operatorWhitelist{ids: cfg.Operator.IDs}, convs, nil, clk.Now)
	_ = contentSvc

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := messages.SweepExpired(ctx, 1000); err != nil {
					log.Error("sweep messages: " + err.Error())
				} else if n > 0 {
					log.Info("swept expired messages", "count", n)
				}
				if n, err := groups.ScanOverdueTodos(ctx); err != nil {
					log.Error("scan overdue todos: " + err.Error())
				} else if n > 0 {
					log.Info("overdue todos", "count", n)
				}
				if n, err := groups.CleanupExpiredMutes(ctx); err != nil {
					log.Error("prune mutes: " + err.Error())
				} else if n > 0 {
					log.Info("pruned expired mutes", "count", n)
				}
				if n, err := contacts.ExpireStaleRequests(ctx); err != nil {
					log.Error("expire contact requests: " + err.Error())
				} else if n > 0 {
					log.Info("expired stale contact requests", "count", n)
				}
				if n, err := mediaSvc.ExpireUploads(ctx); err != nil {
					log.Error("expire uploads: " + err.Error())
				} else if n > 0 {
					log.Info("expired upload sessions", "count", n)
				}
				if n, err := moments.PublishDueSchedules(ctx, "worker-1"); err != nil {
					log.Error("publish due moment schedules: " + err.Error())
				} else if n > 0 {
					log.Info("published scheduled moments", "count", n)
				}
				if err := moments.RecoverStaleLeases(ctx); err != nil {
					log.Error("recover moment schedule leases: " + err.Error())
				}
				if n, err := backups.ExpireBackups(ctx); err != nil {
					log.Error("expire backups: " + err.Error())
				} else if n > 0 {
					log.Info("expired backups", "count", n)
				}
			}
		}
	}()

	log.Info("worker running")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	log.Info("shutting down")
}

// groupMessageAdapter mirrors cmd/api's adapter (kept duplicated here because
// the worker and api are separate binaries; when G lands the wiring will move
// to a shared composition-root package).
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

// contactMomentAdapter mirrors cmd/api's adapter.
type contactMomentAdapter struct{ c *contact.Service }

func (a contactMomentAdapter) ActiveFriendsWithEpoch(ctx context.Context, authorID int64) ([]moment.FriendEpoch, error) {
	rows, err := a.c.ActiveFriendsWithEpoch(ctx, authorID)
	if err != nil {
		return nil, err
	}
	out := make([]moment.FriendEpoch, len(rows))
	for i, r := range rows {
		out[i] = moment.FriendEpoch{UserID: r.UserID, Epoch: r.Epoch}
	}
	return out, nil
}

func (a contactMomentAdapter) IsFriendCurrentEpoch(ctx context.Context, authorID, viewerID, epoch int64) (bool, error) {
	return a.c.IsFriendCurrentEpoch(ctx, authorID, viewerID, epoch)
}

func (a contactMomentAdapter) MomentPerm(ctx context.Context, authorID, viewerID int64) (bool, bool, bool, error) {
	return a.c.MomentPerm(ctx, authorID, viewerID)
}

func (a contactMomentAdapter) ExpandTag(ctx context.Context, ownerID, tagID int64) ([]moment.FriendEpoch, error) {
	rows, err := a.c.ExpandTag(ctx, ownerID, tagID)
	if err != nil {
		return nil, err
	}
	out := make([]moment.FriendEpoch, len(rows))
	for i, r := range rows {
		out[i] = moment.FriendEpoch{UserID: r.UserID, Epoch: r.Epoch}
	}
	return out, nil
}

// operatorWhitelist mirrors cmd/api's adapter. The worker only runs
// FanoutArticleNotifications, which never calls IsOperator; the stub exists
// solely to satisfy the content.Service constructor.
type operatorWhitelist struct{ ids map[int64]bool }

func (w operatorWhitelist) IsOperator(_ context.Context, userID int64) bool {
	return w.ids[userID]
}

// backupSourcesStub mirrors cmd/api's stub.
type backupSourcesStub struct{}

func (backupSourcesStub) Profile(_ context.Context, _ int64) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (backupSourcesStub) ContactSettings(_ context.Context, _ int64) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (backupSourcesStub) Favorites(_ context.Context, _ int64) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (backupSourcesStub) OwnMoments(_ context.Context, _ int64) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (backupSourcesStub) ReadableMessages(_ context.Context, _ int64) (json.RawMessage, error) {
	return json.RawMessage(`[]`), nil
}
