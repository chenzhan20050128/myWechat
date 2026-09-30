package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/example/wechat/internal/platform/mq"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// FailureClass is the classifier result for a handler error.
type FailureClass int

const (
	Transient FailureClass = iota
	Permanent
)

// Handler is the legacy non-transactional handler form.
type Handler func(ctx context.Context, ev mq.Event) error

// TxHandler processes an event using the provided transaction. Business
// effects must use tx so a handler failure can be rolled back independently.
type TxHandler func(ctx context.Context, tx mysqlx.Tx, ev mq.Event) error

// Classifier maps handler errors to transient vs permanent.
type Classifier func(err error) FailureClass

// Outcome distinguishes a completed side effect from a scheduled retry.
type Outcome int

const (
	OutcomeUnknown Outcome = iota
	OutcomeProcessed
	OutcomeRetried
	OutcomeFailed
)

// InboxConsumer runs the inbox framework for one consumer/queue pair.
type InboxConsumer struct {
	db           *sql.DB
	consumerName string
	classifier   Classifier
	now          func() time.Time
	maxAttempts  int
}

func NewInbox(db *sql.DB, consumer string, c Classifier, now func() time.Time) *InboxConsumer {
	return &InboxConsumer{db: db, consumerName: consumer, classifier: c, now: now, maxAttempts: 5}
}

// retryLadder returns the backoff for attempt (1-indexed).
func retryLadder(attempt int) time.Duration {
	switch (attempt - 1) % 3 {
	case 0:
		return time.Minute
	case 1:
		return 5 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// Dispatch is retained for handlers that already provide their own
// transaction. Production consumers should use DispatchTx.
func (c *InboxConsumer) Dispatch(ctx context.Context, ev mq.Event, h Handler) error {
	return c.DispatchTx(ctx, ev, func(ctx context.Context, _ mysqlx.Tx, ev mq.Event) error {
		return h(ctx, ev)
	})
}

// DispatchTx owns the inbox row and the handler transaction. A savepoint
// isolates handler writes on failure, allowing retry/dead-letter bookkeeping
// to commit without committing partial business effects.
func (c *InboxConsumer) DispatchTx(ctx context.Context, ev mq.Event, h TxHandler) error {
	_, err := c.DispatchTxDetailed(ctx, ev, h)
	return err
}

func (c *InboxConsumer) DispatchTxDetailed(ctx context.Context, ev mq.Event, h TxHandler) (Outcome, error) {
	outcome := OutcomeUnknown
	err := mysqlx.WithinTx(ctx, c.db, func(tx mysqlx.Tx) error {
		if err := c.receiveTx(ctx, tx, ev.EventID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SAVEPOINT inbox_handler`); err != nil {
			return fmt.Errorf("inbox: savepoint: %w", err)
		}
		handlerErr := h(ctx, tx, ev)
		if handlerErr == nil {
			if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT inbox_handler`); err != nil {
				return fmt.Errorf("inbox: release savepoint: %w", err)
			}
			if err := c.markProcessedTx(ctx, tx, ev.EventID); err != nil {
				return err
			}
			outcome = OutcomeProcessed
			return nil
		}

		if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT inbox_handler`); err != nil {
			return fmt.Errorf("inbox: rollback handler savepoint: %w", err)
		}
		if c.classifier(handlerErr) == Permanent {
			if err := c.markFailedTx(ctx, tx, ev, handlerErr); err != nil {
				return err
			}
			outcome = OutcomeFailed
		} else {
			dead, err := c.enqueueRetryTx(ctx, tx, ev, handlerErr)
			if err != nil {
				return err
			}
			if dead {
				outcome = OutcomeFailed
			} else {
				outcome = OutcomeRetried
			}
		}
		if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT inbox_handler`); err != nil {
			return fmt.Errorf("inbox: release failed savepoint: %w", err)
		}
		return nil
	})
	return outcome, err
}

var ErrDuplicateEvent = errors.New("runtime: duplicate event")

func (c *InboxConsumer) receiveTx(ctx context.Context, tx mysqlx.Tx, eventID string) error {
	if eventID == "" {
		return errors.New("inbox: event_id is required")
	}
	now := c.now()
	if _, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO inbox_events (consumer_name, event_id, status, first_received_at)
		VALUES (?, ?, 'processing', ?)`, c.consumerName, eventID, now); err != nil {
		return fmt.Errorf("inbox: mark received: %w", err)
	}
	var status string
	if err := tx.QueryRowContext(ctx, `
		SELECT status FROM inbox_events
		WHERE consumer_name = ? AND event_id = ? FOR UPDATE`,
		c.consumerName, eventID).Scan(&status); err != nil {
		return fmt.Errorf("inbox: read status: %w", err)
	}
	if status == "processed" || status == "failed" || status == "dead" {
		return ErrDuplicateEvent
	}
	if status != "processing" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE inbox_events SET status = 'processing'
			WHERE consumer_name = ? AND event_id = ?`, c.consumerName, eventID); err != nil {
			return fmt.Errorf("inbox: claim: %w", err)
		}
	}
	return nil
}

func (c *InboxConsumer) markProcessedTx(ctx context.Context, tx mysqlx.Tx, eventID string) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE inbox_events SET status = 'processed', processed_at = ?, last_error = NULL
		WHERE consumer_name = ? AND event_id = ?`, c.now(), c.consumerName, eventID)
	if err != nil {
		return fmt.Errorf("inbox: mark processed: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return fmt.Errorf("inbox: processed affected: %w", err)
		}
		return ErrDuplicateEvent
	}
	return nil
}

func (c *InboxConsumer) markFailedTx(ctx context.Context, tx mysqlx.Tx, ev mq.Event, cause error) error {
	now := c.now()
	res, err := tx.ExecContext(ctx, `
		UPDATE inbox_events SET status = 'failed', last_error = ?
		WHERE consumer_name = ? AND event_id = ?`, cause.Error(), c.consumerName, ev.EventID)
	if err != nil {
		return fmt.Errorf("inbox: mark failed: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return fmt.Errorf("inbox: failed affected: %w", err)
		}
		return ErrDuplicateEvent
	}
	envelope, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("inbox: marshal dead letter: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO async_dead_letters (event_id, consumer_name, last_error, failed_at, envelope)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE last_error = VALUES(last_error), failed_at = VALUES(failed_at), envelope = VALUES(envelope)`,
		ev.EventID, c.consumerName, cause.Error(), now, envelope); err != nil {
		return fmt.Errorf("inbox: dead letter: %w", err)
	}
	return nil
}

func (c *InboxConsumer) enqueueRetryTx(ctx context.Context, tx mysqlx.Tx, ev mq.Event, cause error) (bool, error) {
	now := c.now()
	var attempt int
	err := tx.QueryRowContext(ctx, `
		SELECT attempt_count FROM async_retry_tasks
		WHERE consumer_name = ? AND event_id = ? FOR UPDATE`,
		c.consumerName, ev.EventID).Scan(&attempt)
	if errors.Is(err, sql.ErrNoRows) {
		attempt = 0
	} else if err != nil {
		return false, fmt.Errorf("inbox: read retry: %w", err)
	}
	attempt++
	if attempt > c.maxAttempts {
		if err := c.markFailedTx(ctx, tx, ev, cause); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE async_retry_tasks SET status = 'dead', lease_until = NULL, updated_at = ?
			WHERE consumer_name = ? AND event_id = ?`, c.now(), c.consumerName, ev.EventID); err != nil {
			return false, err
		}
		return true, nil
	}
	next := now.Add(retryLadder(attempt))
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO async_retry_tasks
			(consumer_name, event_id, queue, event_type, aggregate_id, version,
			 attempt_count, next_attempt_at, last_error, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?) AS v
		ON DUPLICATE KEY UPDATE
			queue = v.queue, event_type = v.event_type, aggregate_id = v.aggregate_id,
			version = v.version, attempt_count = v.attempt_count,
			next_attempt_at = v.next_attempt_at, last_error = v.last_error,
			status = 'pending', lease_until = NULL, updated_at = v.updated_at`,
		c.consumerName, ev.EventID, ev.Queue, ev.Type, ev.AggregateID, ev.Version,
		attempt, next, cause.Error(), now, now); err != nil {
		return false, fmt.Errorf("inbox: enqueue retry: %w", err)
	}
	return false, nil
}

// LeaseDueRetries atomically claims retry rows and returns the original
// envelope. Stale dispatching rows are reclaimable after their lease expires.
func (c *InboxConsumer) LeaseDueRetries(ctx context.Context, limit int) ([]mq.Event, []string, error) {
	var events []mq.Event
	var queues []string
	now := c.now()
	leaseUntil := now.Add(time.Minute)
	err := mysqlx.WithinTx(ctx, c.db, func(tx mysqlx.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT queue, event_id, event_type, aggregate_id, version, attempt_count
			FROM async_retry_tasks
			WHERE consumer_name = ?
			  AND ((status = 'pending' AND next_attempt_at <= ?)
			    OR (status = 'dispatching' AND lease_until < ?))
			ORDER BY next_attempt_at, event_id
			LIMIT ? FOR UPDATE SKIP LOCKED`,
			c.consumerName, now, now, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var queue, eventID, eventType, aggregateID string
			var version int64
			var attempt int
			if err := rows.Scan(&queue, &eventID, &eventType, &aggregateID, &version, &attempt); err != nil {
				return err
			}
			events = append(events, mq.Event{
				EventID: eventID, Type: eventType, AggregateID: aggregateID,
				Version: version, Attempt: attempt,
			})
			queues = append(queues, queue)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		for _, ev := range events {
			if _, err := tx.ExecContext(ctx, `
				UPDATE async_retry_tasks
				SET status = 'dispatching', lease_until = ?, dispatched_at = ?, updated_at = ?
				WHERE consumer_name = ? AND event_id = ?`,
				leaseUntil, now, now, c.consumerName, ev.EventID); err != nil {
				return err
			}
		}
		return nil
	})
	return events, queues, err
}

// MarkRetryDone clears a retry row after successful redelivery.
func (c *InboxConsumer) MarkRetryDone(ctx context.Context, eventID string) error {
	res, err := c.db.ExecContext(ctx, `
		UPDATE async_retry_tasks SET status = 'done', lease_until = NULL, updated_at = ?
		WHERE consumer_name = ? AND event_id = ?`, c.now(), c.consumerName, eventID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrDuplicateEvent
	}
	return nil
}
