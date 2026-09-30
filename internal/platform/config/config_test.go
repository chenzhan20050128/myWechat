package config

import (
	"strings"
	"testing"
)

func TestLoadRejectsUnimplementedProductionDrivers(t *testing.T) {
	t.Setenv("WECHAT_CACHE_DRIVER", "memory")
	t.Setenv("WECHAT_STORAGE_DRIVER", "local")
	t.Setenv("WECHAT_MQ_DRIVER", "rabbitmq")

	if _, err := Load(); err == nil || (err != nil && !strings.Contains(err.Error(), "rabbitmq is not implemented")) {
		t.Fatalf("Load() error = %v, want unimplemented rabbitmq", err)
	}
}

func TestLoadRejectsInvalidTrustedProxy(t *testing.T) {
	t.Setenv("WECHAT_CACHE_DRIVER", "memory")
	t.Setenv("WECHAT_STORAGE_DRIVER", "local")
	t.Setenv("WECHAT_MQ_DRIVER", "none")
	t.Setenv("WECHAT_TRUSTED_PROXIES", "10.0.0.0/8,not-an-address")

	if _, err := Load(); err == nil || (err != nil && !strings.Contains(err.Error(), "not-an-address")) {
		t.Fatalf("Load() error = %v, want invalid trusted proxy", err)
	}
}
