// Package config loads and validates process configuration from environment
// variables (12-factor). See docs/specs/00-platform.md §2.1.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the root configuration for all three processes (api/gateway/worker).
type Config struct {
	HTTP    HTTP
	MySQL   MySQL
	Cache   Cache
	Storage Storage
	MQ      MQ
	Auth    Auth
	Secret  Secret
	Log     Log
}

// Secret holds the HMAC keys for platform-signed tokens (SPEC-00 §2.6/§2.9).
type Secret struct {
	// Signing is the single key behind every signed token: download URLs and
	// personal QR codes alike.
	Signing string
}

type HTTP struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
}

type MySQL struct {
	DSN             string // must already carry parseTime/time_zone/tx_isolation params; mysqlx.Open enforces them
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

type Cache struct {
	Driver    string // redis | memory
	RedisAddr string
	RedisDB   int
}

type Storage struct {
	Driver     string // s3 | local
	LocalDir   string
	S3Endpoint string
	S3Bucket   string
	S3Key      string
	S3Secret   string
	S3UseSSL   bool
	// PublicBaseURL is the externally reachable origin of this API, used to
	// build absolute download URLs for the local driver (which serves bytes
	// through the API proxy instead of pre-signing S3 URLs).
	PublicBaseURL string
}

type MQ struct {
	Driver      string // rabbitmq | none
	RabbitMQURL string
}

type Auth struct {
	AccessTTL       time.Duration
	RefreshTTL      time.Duration
	LoginMaxFails   int
	LoginFreeze     time.Duration
	ArgonTime       uint32
	ArgonMemoryKiB  uint32
	ArgonThreads    uint8
	ArgonKeyLen     uint32
	// DownloadURLTTL bounds how long a signed download URL stays valid (R14).
	DownloadURLTTL time.Duration
}

type Log struct {
	Level  string // debug | info | warn | error
	Format string // json | text
}

// Load parses WECHAT_* environment variables and validates them (R1, R2).
func Load() (*Config, error) {
	c := &Config{
		HTTP: HTTP{
			Addr:            env("WECHAT_HTTP_ADDR", ":8080"),
			ReadTimeout:     envDur("WECHAT_HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:    envDur("WECHAT_HTTP_WRITE_TIMEOUT", 30*time.Second),
			ShutdownTimeout: envDur("WECHAT_HTTP_SHUTDOWN_TIMEOUT", 10*time.Second),
		},
		MySQL: MySQL{
			DSN:             os.Getenv("WECHAT_MYSQL_DSN"),
			MaxOpenConns:    envInt("WECHAT_MYSQL_MAX_OPEN", 32),
			MaxIdleConns:    envInt("WECHAT_MYSQL_MAX_IDLE", 8),
			ConnMaxLifetime: envDur("WECHAT_MYSQL_CONN_MAX_LIFETIME", 30*time.Minute),
		},
		Cache: Cache{
			Driver:    env("WECHAT_CACHE_DRIVER", "memory"),
			RedisAddr: env("WECHAT_REDIS_ADDR", "127.0.0.1:6379"),
			RedisDB:   envInt("WECHAT_REDIS_DB", 0),
		},
		Storage: Storage{
			Driver:     env("WECHAT_STORAGE_DRIVER", "local"),
			LocalDir:   env("WECHAT_STORAGE_LOCAL_DIR", "./data/objects"),
			S3Endpoint: os.Getenv("WECHAT_S3_ENDPOINT"),
			S3Bucket:   os.Getenv("WECHAT_S3_BUCKET"),
			S3Key:      os.Getenv("WECHAT_S3_KEY"),
			S3Secret:   os.Getenv("WECHAT_S3_SECRET"),
			S3UseSSL:   envBool("WECHAT_S3_USE_SSL", true),
			PublicBaseURL: env("WECHAT_PUBLIC_BASE_URL", "http://127.0.0.1:8080"),
		},
		MQ: MQ{
			Driver:      env("WECHAT_MQ_DRIVER", "none"),
			RabbitMQURL: env("WECHAT_RABBITMQ_URL", "amqp://guest:guest@127.0.0.1:5672/"),
		},
		Auth: Auth{
			AccessTTL:      envDur("WECHAT_AUTH_ACCESS_TTL", 30*time.Minute),
			RefreshTTL:     envDur("WECHAT_AUTH_REFRESH_TTL", 30*24*time.Hour),
			LoginMaxFails:  envInt("WECHAT_AUTH_LOGIN_MAX_FAILS", 10),
			LoginFreeze:    envDur("WECHAT_AUTH_LOGIN_FREEZE", 10*time.Minute),
			ArgonTime:      uint32(envInt("WECHAT_ARGON_TIME", 3)),
			ArgonMemoryKiB: uint32(envInt("WECHAT_ARGON_MEMORY_KIB", 64*1024)),
			ArgonThreads:   uint8(envInt("WECHAT_ARGON_THREADS", 2)),
			ArgonKeyLen:    uint32(envInt("WECHAT_ARGON_KEY_LEN", 32)),
			DownloadURLTTL: envDur("WECHAT_DOWNLOAD_URL_TTL", 15*time.Minute),
		},
		Secret: Secret{
			Signing: env("WECHAT_SIGNING_SECRET", "dev-only-signing-secret-change-me"),
		},
		Log: Log{
			Level:  env("WECHAT_LOG_LEVEL", "info"),
			Format: env("WECHAT_LOG_FORMAT", "json"),
		},
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	switch c.Cache.Driver {
	case "redis", "memory":
	default:
		return fmt.Errorf("config: WECHAT_CACHE_DRIVER must be redis|memory, got %q", c.Cache.Driver)
	}
	switch c.Storage.Driver {
	case "s3", "local":
	default:
		return fmt.Errorf("config: WECHAT_STORAGE_DRIVER must be s3|local, got %q", c.Storage.Driver)
	}
	switch c.MQ.Driver {
	case "rabbitmq", "none":
	default:
		return fmt.Errorf("config: WECHAT_MQ_DRIVER must be rabbitmq|none, got %q", c.MQ.Driver)
	}
	if c.Auth.AccessTTL <= 0 || c.Auth.RefreshTTL <= c.Auth.AccessTTL {
		return fmt.Errorf("config: refresh TTL must exceed access TTL")
	}
	if c.Secret.Signing == "" {
		return fmt.Errorf("config: WECHAT_SIGNING_SECRET is required")
	}
	if c.Storage.Driver == "s3" && (c.Storage.S3Endpoint == "" || c.Storage.S3Bucket == "") {
		return fmt.Errorf("config: s3 driver requires WECHAT_S3_ENDPOINT and WECHAT_S3_BUCKET")
	}
	return nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
