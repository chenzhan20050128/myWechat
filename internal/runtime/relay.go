// Package runtime is the cross-domain coordination layer: outbox relay,
// inbox consumer framework, scheduler and lifecycle batch jobs. It never
// carries business state itself — it only invokes worker entry points each
// domain exposes (SPEC-10 §1).
package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/example/wechat/internal/platform/mq"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// Relay drains the outbox_events table and publishes to the configured
// publisher. It is the single producer for every queue (SPEC-10 §4.1).
type Relay struct {
	db        *sql.DB
	publisher mq.Publisher
	owner     string
	batchSize int
	leaseTTL  time.Duration
	now       func() time.Time
}

const maxPublicationAttempts = 5

func NewRelay(db *sql.DB, pub mq.Publisher, owner string, now func() time.Time) *Relay {
	if owner == "" {
		owner = "relay-1"
	}
	return &Relay{db: db, publisher: pub, owner: owner, batchSize: 100, leaseTTL: 60 * time.Second, now: now}
}

// OutboxRow is one leased event.
type OutboxRow struct {
	EventID      string
	Type         string
	AggregateID  string
	Version      int64
	Queue        string
	Attempt      int
	LeaseVersion int64
}

// LeaseOutbound atomically claims up to batchSize pending (or stale publishing)
// events, marking them publishing under this owner (R1).
func (r *Relay) LeaseOutbound(ctx context.Context) ([]OutboxRow, error) {
	now := r.now()
	leaseUntil := now.Add(r.leaseTTL)
	var ids []string
	var rows []OutboxRow
	err := mysqlx.WithinTx(ctx, r.db, func(tx mysqlx.Tx) error {
		rs, err := tx.QueryContext(ctx, `
			SELECT event_id, type, aggregate_id, version, queue, attempt, lease_version
			FROM outbox_events
			WHERE status = 'pending' OR (status = 'publishing' AND lease_expires_at < UTC_TIMESTAMP(6))
			ORDER BY created_at, event_id
			LIMIT ? FOR UPDATE SKIP LOCKED`, r.batchSize)
		if err != nil {
			return fmt.Errorf("relay: lease query: %w", err)
		}
		defer rs.Close()
		ids = nil
		rows = nil
		for rs.Next() {
			var o OutboxRow
			if err := rs.Scan(&o.EventID, &o.Type, &o.AggregateID, &o.Version, &o.Queue, &o.Attempt, &o.LeaseVersion); err != nil {
				return err
			}
			ids = append(ids, o.EventID)
			rows = append(rows, o)
		}
		if len(ids) == 0 {
			return nil
		}
		ph := mysqlx.Placeholders(len(ids))
		args := []any{r.owner, leaseUntil}
		for _, id := range ids {
			args = append(args, id)
		}
		_, err = tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE outbox_events
			SET status = 'publishing', lease_owner = ?, lease_expires_at = ?,
				lease_version = lease_version + 1, updated_at = UTC_TIMESTAMP(6)
			WHERE event_id IN (%s)`, ph), args...)
		if err != nil {
			return fmt.Errorf("relay: mark publishing: %w", err)
		}
		for i := range rows {
			rows[i].LeaseVersion++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// PublishOne publishes a single leased event and confirms it (R2/R3).
func (r *Relay) PublishOne(ctx context.Context, o OutboxRow) error {
	ev := mq.Event{
		EventID: o.EventID, Type: o.Type, AggregateID: o.AggregateID,
		Version: o.Version, Attempt: o.Attempt, Queue: o.Queue,
	}
	if err := r.publisher.Publish(ctx, o.Queue, ev); err != nil {
		attempt := o.Attempt + 1
		var markErr error
		if attempt >= maxPublicationAttempts {
			_, markErr = r.db.ExecContext(ctx, `
				UPDATE outbox_events
				SET status = 'failed', attempt = ?, last_error = ?, lease_owner = NULL,
					lease_expires_at = NULL, updated_at = UTC_TIMESTAMP(6)
				WHERE event_id = ? AND status = 'publishing' AND lease_owner = ? AND lease_version = ?`,
				attempt, err.Error(), o.EventID, r.owner, o.LeaseVersion)
		} else {
			_, markErr = r.db.ExecContext(ctx, `
				UPDATE outbox_events
				SET status = 'publishing', attempt = ?, last_error = ?, lease_owner = ?,
					lease_expires_at = ?, updated_at = UTC_TIMESTAMP(6)
				WHERE event_id = ? AND status = 'publishing' AND lease_owner = ? AND lease_version = ?`,
				attempt, err.Error(), "retry:"+r.owner, r.now().Add(retryLadder(attempt)),
				o.EventID, r.owner, o.LeaseVersion)
		}
		if markErr != nil {
			return fmt.Errorf("relay: publish %s: %w (mark failed: %v)", o.EventID, err, markErr)
		}
		return err
	}
	now := r.now()
	res, err := r.db.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'published', published_at = ?, updated_at = ?, lease_expires_at = NULL
		WHERE event_id = ? AND status = 'publishing' AND lease_owner = ? AND lease_version = ?`,
		now, now, o.EventID, r.owner, o.LeaseVersion)
	if err != nil {
		return fmt.Errorf("relay: confirm: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("relay: confirm affected: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("relay: confirm %s: stale lease", o.EventID)
	}
	return nil
}

// RunOnce performs one drain cycle: lease → publish → confirm. Returns the
// number of events published (including failed).
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	rows, err := r.LeaseOutbound(ctx)
	if err != nil {
		return 0, err
	}
	published := 0
	var errs []error
	for _, o := range rows {
		if err := r.PublishOne(ctx, o); err != nil {
			errs = append(errs, err)
			continue
		}
		published++
	}
	return published, errors.Join(errs...)
}

// ErrNoRows is exposed for tests that need to distinguish empty leases.
var ErrNoRows = errors.New("runtime: no rows")
