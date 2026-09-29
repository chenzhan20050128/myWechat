package mysqlx

import (
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestPlaceholders(t *testing.T) {
	cases := map[int]string{0: "", 1: "?", 2: "?,?", 3: "?,?,?"}
	for n, want := range cases {
		if got := Placeholders(n); got != want {
			t.Fatalf("Placeholders(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestPlaceholdersNegative(t *testing.T) {
	if got := Placeholders(-1); got != "" {
		t.Fatalf("Placeholders(-1) = %q, want empty", got)
	}
}

// TestNormalizeDSNEnforcesSessionContract pins R11: whatever the operator puts
// in WECHAT_MYSQL_DSN, the connection still runs UTC, READ COMMITTED, utf8mb4
// and a 5s lock-wait budget (ADR-005/006, SPEC-04 R2).
func TestNormalizeDSNEnforcesSessionContract(t *testing.T) {
	got, err := NormalizeDSN("user:pw@tcp(127.0.0.1:3306)/wechat_dev")
	if err != nil {
		t.Fatalf("NormalizeDSN: %v", err)
	}
	cfg, err := mysql.ParseDSN(got)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if !cfg.ParseTime {
		t.Error("parseTime must be enabled")
	}
	if cfg.Collation != "utf8mb4_bin" {
		t.Errorf("collation = %q, want utf8mb4_bin", cfg.Collation)
	}
	want := map[string]string{
		"time_zone":                `'+00:00'`,
		"transaction_isolation":    `'READ-COMMITTED'`,
		"innodb_lock_wait_timeout": "5",
	}
	for k, v := range want {
		if cfg.Params[k] != v {
			t.Errorf("param %s = %q, want %q", k, cfg.Params[k], v)
		}
	}
}

// TestNormalizeDSNOverridesOperatorParams stops an operator DSN from silently
// weakening the isolation or timezone contract.
func TestNormalizeDSNOverridesOperatorParams(t *testing.T) {
	got, err := NormalizeDSN("user:pw@tcp(127.0.0.1:3306)/wechat_dev?time_zone=%27%2B08%3A00%27&transaction_isolation=%27SERIALIZABLE%27")
	if err != nil {
		t.Fatalf("NormalizeDSN: %v", err)
	}
	cfg, err := mysql.ParseDSN(got)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if cfg.Params["time_zone"] != `'+00:00'` {
		t.Errorf("time_zone = %q, want the enforced +00:00", cfg.Params["time_zone"])
	}
	if cfg.Params["transaction_isolation"] != `'READ-COMMITTED'` {
		t.Errorf("transaction_isolation = %q, want the enforced READ-COMMITTED", cfg.Params["transaction_isolation"])
	}
}

func TestNormalizeDSNRejectsEmptyAndMalformed(t *testing.T) {
	if _, err := NormalizeDSN(""); err == nil {
		t.Error("empty DSN must be rejected")
	}
	if _, err := NormalizeDSN("not a dsn"); err == nil {
		t.Error("malformed DSN must be rejected")
	}
}
