// Package mysqlx opens MySQL with enforced contract parameters and provides
// the transaction helper with deadlock retry. SPEC-00 §2.4; ADR-006.
package mysqlx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

const maxTxRetries = 3 // contract §12.4: retry deadlocks with same idempotency key

// enforcedParams are the session settings every connection must carry (R11).
// transaction_isolation is the MySQL 8 name; the pre-8.0 tx_isolation alias
// was removed, and innodb_lock_wait_timeout is the 5s fail-fast budget the
// lock-order retry logic depends on (ADR-006, SPEC-04 R2).
var enforcedParams = map[string]string{
	"time_zone":                `'+00:00'`,
	"transaction_isolation":    `'READ-COMMITTED'`,
	"innodb_lock_wait_timeout": "5",
}

// NormalizeDSN validates a DSN and adds the enforced session parameters.
// Exported so the contract can be asserted without a live server.
func NormalizeDSN(dsn string) (string, error) {
	if dsn == "" {
		return "", fmt.Errorf("mysqlx: WECHAT_MYSQL_DSN is required")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("mysqlx: parse DSN: %w", err)
	}
	cfg.ParseTime = true
	cfg.Params = enforceParams(cfg.Params, enforcedParams)
	// R11: the connection is always utf8mb4/utf8mb4_bin, whatever the DSN says.
	if err := cfg.Apply(mysql.Charset("utf8mb4", "utf8mb4_bin")); err != nil {
		return "", fmt.Errorf("mysqlx: set charset: %w", err)
	}
	return cfg.FormatDSN(), nil
}

// Open validates/enriches the DSN and opens a pooled connection (R11).
func Open(dsn string, maxOpen, maxIdle int, connMaxLifetime time.Duration) (*sql.DB, error) {
	normalized, err := NormalizeDSN(dsn)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", normalized)
	if err != nil {
		return nil, fmt.Errorf("mysqlx: open: %w", err)
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(connMaxLifetime)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mysqlx: ping: %w", err)
	}
	return db, nil
}

// enforceParams overlays the mandatory session parameters onto the operator
// DSN. The enforced values win: a DSN that asks for local-time storage or a
// weaker isolation level would silently break ADR-005/ADR-006 invariants.
func enforceParams(base, enforced map[string]string) map[string]string {
	if base == nil {
		base = map[string]string{}
	}
	for k, v := range enforced {
		base[k] = v
	}
	return base
}

// Tx is the narrow transaction handle passed to store methods.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DBTX is anything that can execute SQL: *sql.DB or a Tx. Read-only helper
// queries outside transactions use this.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// WithinTx runs fn in a transaction, retrying on deadlock/lock-timeout (R12).
// fn must be idempotent-safe under retry (same idempotency key semantics).
func WithinTx(ctx context.Context, db *sql.DB, fn func(tx Tx) error) error {
	var lastErr error
	for attempt := 1; attempt <= maxTxRetries; attempt++ {
		lastErr = runOnce(ctx, db, fn)
		if lastErr == nil {
			return nil
		}
		if !isRetryable(lastErr) {
			return lastErr
		}
		time.Sleep(time.Duration(attempt) * 20 * time.Millisecond)
	}
	return lastErr
}

func runOnce(ctx context.Context, db *sql.DB, fn func(tx Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysqlx: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("mysqlx: commit: %w", err)
	}
	return nil
}

func isRetryable(err error) bool {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1213 || me.Number == 1205 // deadlock / lock wait timeout
	}
	return false
}

// Placeholders returns n comma-separated "?" markers for a parameterized
// IN (...) list. Values are still passed as args — never interpolated (R13).
func Placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// IsDuplicate reports a unique-key violation (errno 1062), optionally
// matched against a constraint name fragment.
func IsDuplicate(err error, constraintHint string) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) || me.Number != 1062 {
		return false
	}
	return constraintHint == "" || strings.Contains(me.Message, constraintHint)
}
