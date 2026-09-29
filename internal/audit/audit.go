// Package audit writes append-only audit records (contract §16.2). Two write
// modes: LogTx joins the caller's business transaction (facts that must never
// be lost, e.g. registration, password change); Log writes standalone rows on
// the pool (login attempts, downloads). Rows are queryable by the operator
// module (phase 6) only.
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/example/wechat/internal/platform/clock"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// Entry is one audit fact. Detail must be JSON-serializable; never put
// secrets (passwords, raw tokens) here.
type Entry struct {
	Type    string         // e.g. "auth.login.success"
	ActorID int64          // 0 = anonymous/system
	IP      string
	Detail  map[string]any // optional structured context
}

// Service writes audit rows.
type Service struct {
	now clock.Clock
}

// New builds the audit service with the given clock (ADR-005: app-supplied
// UTC times everywhere, including audit).
func New(c clock.Clock) *Service { return &Service{now: c} }

// LogTx appends an audit row inside the caller's transaction.
func (s *Service) LogTx(ctx context.Context, tx mysqlx.Tx, e Entry) error {
	return s.insert(ctx, tx, e)
}

// Log appends a standalone audit row on the pool.
func (s *Service) Log(ctx context.Context, db mysqlx.DBTX, e Entry) error {
	return s.insert(ctx, db, e)
}

func (s *Service) insert(ctx context.Context, db mysqlx.DBTX, e Entry) error {
	detail, err := json.Marshal(e.Detail)
	if err != nil {
		return fmt.Errorf("audit: marshal detail: %w", err)
	}
	if e.Detail == nil {
		detail = []byte("{}")
	}
	var actor any
	if e.ActorID > 0 {
		actor = e.ActorID
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO audit_logs (event_type, actor_id, actor_ip, detail, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		e.Type, actor, e.IP, detail, s.now.Now())
	if err != nil {
		return fmt.Errorf("audit: insert %s: %w", e.Type, err)
	}
	return nil
}
