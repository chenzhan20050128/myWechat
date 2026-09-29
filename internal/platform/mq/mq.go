// Package mq defines the async event publishing port. RabbitMQ is the
// production driver; `none` is the dev driver where the worker polls the
// MySQL outbox directly (contract §12.5.2; design-review D4). MQ is never a
// source of truth — MySQL Outbox is.
package mq

import "context"

// Event is the wire format published to brokers. Deliberately small:
// IDs and references only, never media payloads or business snapshots
// (contract §12.5.3).
type Event struct {
	EventID     string `json:"event_id"`
	Type        string `json:"type"`
	AggregateID string `json:"aggregate_id"`
	Version     int64  `json:"version"`
	Attempt     int    `json:"attempt"`
}

// Publisher dispatches already-committed outbox events.
type Publisher interface {
	Publish(ctx context.Context, queue string, e Event) error
}

// Fixed queue names (contract §12.5.3). Phase 2 consumers bind these.
const (
	QueueMessagePush   = "wechat.message.push"
	QueueNotification  = "wechat.notification"
	QueueMediaProcess  = "wechat.media.process"
	QueueLifecycle     = "wechat.lifecycle"
	QueueScheduler     = "wechat.scheduler"
	QueueContentService = "wechat.content.service"
)

type nopPublisher struct{}

func (nopPublisher) Publish(context.Context, string, Event) error { return nil }

// NewNop returns a publisher that accepts and discards (dev driver).
func NewNop() Publisher { return nopPublisher{} }
