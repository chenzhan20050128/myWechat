// Command worker runs background jobs: message retention sweep (R26), group
// todo overdue scan, group mute prune, contact stale request expiry. It is the
// single process all periodic jobs share (SPEC-10 will expand this scheduler).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
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
	"github.com/example/wechat/internal/platform/mq"
	"github.com/example/wechat/internal/platform/mysqlx"
	"github.com/example/wechat/internal/platform/outbox"
	"github.com/example/wechat/internal/platform/storage"
	"github.com/example/wechat/internal/runtime"
	"github.com/example/wechat/internal/ws"
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
	cacheStore, err := cache.New(cfg.Cache.Driver, cfg.Cache.RedisAddr, cfg.Cache.RedisDB)
	if err != nil {
		log.Error("cache: " + err.Error())
		os.Exit(1)
	}
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
	moments := moment.New(db, contactMomentAdapter{c: contacts}, mediaSvc, outboxEmitter{}, clk.Now)
	backups := backup.New(db, backupSourcesStub{}, clk.Now)
	contentSvc := content.New(db, operatorWhitelist{ids: cfg.Operator.IDs}, convs, outboxEmitter{}, clk.Now)
	hub := ws.NewHub()
	messages.SetPusher(hub)

	router := runtime.NewLocalRouter(db, "local-router", func(error) runtime.FailureClass {
		return runtime.Transient
	}, clk.Now)
	router.Handle(mq.QueueMessagePush, "message.stored", func(ctx context.Context, _ mysqlx.Tx, ev mq.Event) error {
		id, err := strconv.ParseInt(ev.AggregateID, 10, 64)
		if err != nil {
			return err
		}
		return messages.PushMessage(ctx, id, ev.Type)
	})
	router.Handle(mq.QueueMessagePush, "message.recalled", func(ctx context.Context, _ mysqlx.Tx, ev mq.Event) error {
		id, err := strconv.ParseInt(ev.AggregateID, 10, 64)
		if err != nil {
			return err
		}
		return messages.PushMessage(ctx, id, ev.Type)
	})
	router.HandleQueue(mq.QueueMediaProcess, func(ctx context.Context, tx mysqlx.Tx, ev mq.Event) error {
		id, err := strconv.ParseInt(ev.AggregateID, 10, 64)
		if err != nil {
			return err
		}
		return mediaSvc.ProcessTx(ctx, tx, id)
	})
	router.Handle(mq.QueueContentService, "official.article.published", func(ctx context.Context, tx mysqlx.Tx, ev mq.Event) error {
		id, err := strconv.ParseInt(ev.AggregateID, 10, 64)
		if err != nil {
			return err
		}
		_, err = contentSvc.FanoutArticleNotificationsTx(ctx, tx, id)
		return err
	})
	router.HandleQueue(mq.QueueNotification, func(context.Context, mysqlx.Tx, mq.Event) error {
		return nil
	})

	relay := runtime.NewRelay(db, router, workerID(), clk.Now)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startTask(ctx, log, "relay", 200*time.Millisecond, 5*time.Second, func(ctx context.Context) error {
		n, err := relay.RunOnce(ctx)
		if n > 0 {
			log.Info("published outbox events", "count", n)
		}
		return err
	})
	startTask(ctx, log, "inbox-retry", 10*time.Second, 5*time.Second, func(ctx context.Context) error {
		n, err := router.RunRetriesOnce(ctx, 100)
		if n > 0 {
			log.Info("completed retry events", "count", n)
		}
		return err
	})
	startTask(ctx, log, "message-retention", time.Minute, 30*time.Second, func(ctx context.Context) error {
		n, err := messages.SweepExpired(ctx, 1000)
		if n > 0 {
			log.Info("swept expired messages", "count", n)
		}
		return err
	})
	startTask(ctx, log, "group-todo-overdue", time.Minute, 30*time.Second, func(ctx context.Context) error {
		n, err := groups.ScanOverdueTodos(ctx)
		if n > 0 {
			log.Info("overdue todos", "count", n)
		}
		return err
	})
	startTask(ctx, log, "group-mute-gc", 24*time.Hour, 30*time.Second, func(ctx context.Context) error {
		n, err := groups.CleanupExpiredMutes(ctx)
		if n > 0 {
			log.Info("pruned expired mutes", "count", n)
		}
		return err
	})
	startTask(ctx, log, "contact-request-expiry", time.Minute, 30*time.Second, func(ctx context.Context) error {
		n, err := contacts.ExpireStaleRequests(ctx)
		if n > 0 {
			log.Info("expired stale contact requests", "count", n)
		}
		return err
	})
	startTask(ctx, log, "upload-expiry", 10*time.Minute, time.Minute, func(ctx context.Context) error {
		n, err := mediaSvc.ExpireUploads(ctx)
		if n > 0 {
			log.Info("expired upload sessions", "count", n)
		}
		return err
	})
	startTask(ctx, log, "media-gc", time.Minute, time.Minute, func(ctx context.Context) error {
		n, err := mediaSvc.ProcessGCTasks(ctx, 1000)
		if n > 0 {
			log.Info("processed media GC tasks", "count", n)
		}
		return err
	})
	startTask(ctx, log, "moment-schedule", 10*time.Second, 30*time.Second, func(ctx context.Context) error {
		n, err := moments.PublishDueSchedules(ctx, workerID())
		if n > 0 {
			log.Info("published scheduled moments", "count", n)
		}
		return err
	})
	startTask(ctx, log, "moment-lease-recovery", 10*time.Second, 30*time.Second, func(ctx context.Context) error {
		return moments.RecoverStaleLeases(ctx)
	})
	startTask(ctx, log, "backup-expiry", time.Hour, time.Minute, func(ctx context.Context) error {
		n, err := backups.ExpireBackups(ctx)
		if n > 0 {
			log.Info("expired backups", "count", n)
		}
		return err
	})

	log.Info("worker running")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	log.Info("shutting down")
}

func workerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

func startTask(ctx context.Context, log *slog.Logger, name string, interval, timeout time.Duration, task func(context.Context) error) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				taskCtx, cancel := context.WithTimeout(ctx, timeout)
				if err := runTask(taskCtx, task); err != nil {
					log.Error("worker task failed", "task", name, "error", err)
				}
				cancel()
			}
		}
	}()
}

func runTask(ctx context.Context, task func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return task(ctx)
}

// groupMessageAdapter mirrors cmd/api's adapter (kept duplicated here because
// the worker and api are separate binaries; when G lands the wiring will move
// to a shared composition-root package).
type groupMessageAdapter struct{ g *group.Service }

func (a groupMessageAdapter) CanSend(ctx context.Context, groupID, userID int64) error {
	return a.g.CanSend(ctx, groupID, userID)
}
func (a groupMessageAdapter) CanSendTx(ctx context.Context, tx mysqlx.Tx, groupID, userID int64) error {
	return a.g.CanSendTx(ctx, tx, groupID, userID)
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
func (a groupMessageAdapter) WasMemberAtMany(ctx context.Context, groupID, userID int64, ats []time.Time) ([]bool, error) {
	return a.g.WasMemberAtMany(ctx, groupID, userID, ats)
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

type outboxEmitter struct{}

func (outboxEmitter) Emit(ctx context.Context, tx mysqlx.Tx, eventType string, aggregateID int64, queue string) error {
	return outbox.Emit(ctx, tx, outbox.Event{
		Type: eventType, AggregateID: strconv.FormatInt(aggregateID, 10), Queue: queue,
	})
}

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
