package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/example/wechat/internal/platform/mq"
)

// FailureClass is the classifier result for a handler error.
type FailureClass int

const (
	// Transient means the error is retryable (queued into async_retry_tasks).
	Transient FailureClass = iota
	// Permanent means the event is marked failed and dead-lettered if retries exhausted.
	Permanent
)

// Handler processes a delivered event. It owns its business transaction.
type Handler func(ctx context.Context, ev mq.Event) error

// Classifier maps handler errors to transient vs permanent.
type Classifier func(err error) FailureClass

// InboxConsumer runs the inbox framework (SPEC-10 §4.2). It is
// consumer-name-scoped: each queue gets its own instance.
type InboxConsumer struct {
	db           *sql.DB
	consumerName string
	classifier  Classifier
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

// Dispatch runs the full consume pipeline for one event. It is exported so
// the none-mode local router can invoke the same code path.
func (c *InboxConsumer) Dispatch(ctx context.Context, ev mq.Event, h Handler) error {
	if err := c.markReceived(ctx, ev.EventID); err != nil {
		return err
	}
	err := h(ctx, ev)
	if err == nil {
		return c.markProcessed(ctx, ev.EventID)
	}
	class := c.classifier(err)
	if class == Permanent {
		return c.markFailed(ctx, ev.EventID, err)
	}
	return c.enqueueRetry(ctx, ev, err)
}

func (c *InboxConsumer) markReceived(ctx context.Context, eventID string) error {
	now := c.now()
	_, err := c.db.ExecContext(ctx, `
		INSERT IGNORE INTO inbox_events (consumer_name, event_id, status, first_received_at)
		VALUES (?, ?, 'received', ?)`, c.consumerName, eventID, now)
	if err != nil {
		return fmt.Errorf("inbox: mark received: %w", err)
	}
	var status string
	err = c.db.QueryRowContext(ctx,
		`SELECT status FROM inbox_events WHERE consumer_name = ? AND event_id = ?`,
		c.consumerName, eventID).Scan(&status)
	if err != nil {
		return fmt.Errorf("inbox: read status: %w", err)
	}
	if status == "processed" || status == "failed" {
		return ErrDuplicateEvent
	}
	return nil
}

var ErrDuplicateEvent = errors.New("runtime: duplicate event")

func (c *InboxConsumer) markProcessed(ctx context.Context, eventID string) error {
	now := c.now()
	_, err := c.db.ExecContext(ctx, `
		UPDATE inbox_events SET status = 'processed', processed_at = ?
		WHERE consumer_name = ? AND event_id = ?`, now, c.consumerName, eventID)
	if err != nil {
		return fmt.Errorf("inbox: mark processed: %w", err)
	}
	return nil
}

func (c *InboxConsumer) markFailed(ctx context.Context, eventID string, cause error) error {
	now := c.now()
	_, err := c.db.ExecContext(ctx, `
		UPDATE inbox_events SET status = 'failed', last_error = ?
		WHERE consumer_name = ? AND event_id = ?`, cause.Error(), c.consumerName, eventID)
	if err != nil {
		return fmt.Errorf("inbox: mark failed: %w", err)
	}
	_, err = c.db.ExecContext(ctx, `
		INSERT IGNORE INTO async_dead_letters (event_id, consumer_name, last_error, failed_at)
		VALUES (?, ?, ?, ?)`, eventID, c.consumerName, cause.Error(), now)
	return err
}

func (c *InboxConsumer) enqueueRetry(ctx context.Context, ev mq.Event, cause error) error {
	now := c.now()
	var attempt int
	err := c.db.QueryRowContext(ctx,
		`SELECT attempt_count FROM async_retry_tasks WHERE consumer_name = ? AND event_id = ?`,
		c.consumerName, ev.EventID).Scan(&attempt)
	if errors.Is(err, sql.ErrNoRows) {
		attempt = 0
	} else if err != nil {
		return fmt.Errorf("inbox: read retry: %w", err)
	}
	attempt++
	if attempt > c.maxAttempts {
		return c.markFailed(ctx, ev.EventID, cause)
	}
	next := now.Add(retryLadder(attempt))
	_, err = c.db.ExecContext(ctx, `
		INSERT INTO async_retry_tasks
			(consumer_name, event_id, attempt_count, next_attempt_at, last_error, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)
		ON DUPLICATE KEY UPDATE attempt_count = VALUES(attempt_count),
			next_attempt_at = VALUES(next_attempt_at),
			last_error = VALUES(last_error),
			status = 'pending',
			updated_at = VALUES(updated_at)`,
		c.consumerName, ev.EventID, attempt, next, cause.Error(), now, now)
	if err != nil {
		return fmt.Errorf("inbox: enqueue retry: %w", err)
	}
	return nil
}

// DueRetryTasks returns retry tasks whose next_attempt_at has passed.
func (c *InboxConsumer) DueRetryTasks(ctx context.Context, limit int) ([]mq.Event, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT event_id FROM async_retry_tasks
		WHERE consumer_name = ? AND status = 'pending' AND next_attempt_at <= UTC_TIMESTAMP(6)
		ORDER BY next_attempt_at
		LIMIT ? FOR UPDATE SKIP LOCKED`, c.consumerName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mq.Event
	for rows.Next() {
		var eid string
		if err := rows.Scan(&eid); err != nil {
			return nil, err
		}
		out = append(out, mq.Event{EventID: eid})
	}
	return out, nil
}

// MarkRetryDone clears a retry row after successful redelivery.
func (c *InboxConsumer) MarkRetryDone(ctx context.Context, eventID string) error {
	_, err := c.db.ExecContext(ctx, `
		UPDATE async_retry_tasks SET status = 'done', updated_at = UTC_TIMESTAMP(6)
		WHERE consumer_name = ? AND event_id = ?`, c.consumerName, eventID)
	return err
}
