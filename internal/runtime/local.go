package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/example/wechat/internal/platform/mq"
)

// LocalRouter is the MQ_DRIVER=none adapter. It invokes the same transactional
// handlers as broker consumers while preserving at-least-once semantics.
type LocalRouter struct {
	inbox  *InboxConsumer
	routes map[string]TxHandler
}

// NewLocalRouter builds a direct router with an inbox for one logical queue.
func NewLocalRouter(db *sql.DB, consumer string, classifier Classifier, now func() time.Time) *LocalRouter {
	return &LocalRouter{
		inbox:  NewInbox(db, consumer, classifier, now),
		routes: make(map[string]TxHandler),
	}
}

// Handle registers a handler for one queue/event type pair.
func (r *LocalRouter) Handle(queue, eventType string, h TxHandler) {
	r.routes[queue+"\x00"+eventType] = h
}

// HandleQueue registers a fallback for every event type delivered on a queue.
func (r *LocalRouter) HandleQueue(queue string, h TxHandler) {
	r.routes[queue+"\x00*"] = h
}

// Publish routes the event synchronously. Duplicate inbox deliveries are
// acknowledged because the business effect is already durable.
func (r *LocalRouter) Publish(ctx context.Context, queue string, ev mq.Event) error {
	h := r.routes[queue+"\x00"+ev.Type]
	if h == nil {
		h = r.routes[queue+"\x00*"]
	}
	if h == nil {
		return fmt.Errorf("runtime: no local handler for queue %q event %q", queue, ev.Type)
	}
	outcome, err := r.inbox.DispatchTxDetailed(ctx, ev, h)
	if errors.Is(err, ErrDuplicateEvent) {
		return nil
	}
	if err == nil && (outcome == OutcomeRetried || outcome == OutcomeFailed) {
		return nil
	}
	return err
}

// RunRetriesOnce re-dispatches due retry envelopes. Only a completed handler
// clears the retry row; another transient failure leaves the next backoff in place.
func (r *LocalRouter) RunRetriesOnce(ctx context.Context, limit int) (int, error) {
	events, queues, err := r.inbox.LeaseDueRetries(ctx, limit)
	if err != nil {
		return 0, err
	}
	processed := 0
	for i, ev := range events {
		h := r.routes[queues[i]+"\x00"+ev.Type]
		if h == nil {
			h = r.routes[queues[i]+"\x00*"]
		}
		if h == nil {
			return processed, fmt.Errorf("runtime: no retry handler for queue %q event %q", queues[i], ev.Type)
		}
		outcome, err := r.inbox.DispatchTxDetailed(ctx, ev, h)
		if errors.Is(err, ErrDuplicateEvent) {
			if err := r.inbox.MarkRetryDone(ctx, ev.EventID); err != nil {
				return processed, err
			}
			processed++
			continue
		}
		if err != nil {
			return processed, err
		}
		if outcome == OutcomeProcessed {
			if err := r.inbox.MarkRetryDone(ctx, ev.EventID); err != nil {
				return processed, err
			}
			processed++
		}
	}
	return processed, nil
}
