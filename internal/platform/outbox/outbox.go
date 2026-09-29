// Package outbox is the single event writer used by all modules so the
// outbox_events row format cannot drift between modules (design-review D11;
// contract §14.5: business data + outbox event commit in the same transaction).
package outbox

import (
	"context"
	"fmt"

	"github.com/example/wechat/internal/platform/ids"
	"github.com/example/wechat/internal/platform/mq"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// Event types are namespaced strings ("message.stored", "moment.published").
// The set of types grows per phase; queues map 1:1 in the worker wiring.
type Event struct {
	Type        string
	AggregateID string // domain ID as string
	Version     int64  // aggregate version for ordering, 0 when n/a
	Queue       string // target queue (mq.QueueXxx)
}

// Emit inserts an outbox row on the given transaction. The caller must be
// inside a mysqlx.WithinTx transaction; the event commits atomically with
// the caller's business writes.
func Emit(ctx context.Context, tx mysqlx.Tx, e Event) error {
	if e.Type == "" || e.AggregateID == "" || e.Queue == "" {
		return fmt.Errorf("outbox: type, aggregate_id and queue are required")
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_events
			(event_id, type, aggregate_id, version, queue, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'pending', UTC_TIMESTAMP(6), UTC_TIMESTAMP(6))`,
		ids.New(), e.Type, e.AggregateID, e.Version, e.Queue)
	if err != nil {
		return fmt.Errorf("outbox: insert: %w", err)
	}
	return nil
}

// Wire maps an outbox row to the broker wire event.
func Wire(eventID string, row Event, attempt int) mq.Event {
	return mq.Event{
		EventID:     eventID,
		Type:        row.Type,
		AggregateID: row.AggregateID,
		Version:     row.Version,
		Attempt:     attempt,
	}
}
