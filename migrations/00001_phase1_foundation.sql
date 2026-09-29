-- +goose Up
-- Phase 1 foundation: identity, relationships, media base, audit, outbox.
-- Conventions (docs/ARCHITECTURE.md ADR-005/006/011):
--   * engine InnoDB, charset utf8mb4; identifier columns carry column-level
--     utf8mb4_bin collation so case/accent folding can never cause identity
--     collisions even if application normalization regresses (design-review D6)
--   * times are DATETIME(6) UTC, always app-supplied (no hidden defaults);
--     MySQL session runs with time_zone='+00:00'
--   * ids are BIGINT UNSIGNED AUTO_INCREMENT; cross-system keys are UUID
--     strings (ASCII)

CREATE TABLE users (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  phone           VARCHAR(16)  NOT NULL COLLATE utf8mb4_bin,
  account_name    VARCHAR(20)  NOT NULL COLLATE utf8mb4_bin,
  password_hash   VARCHAR(255) NOT NULL,
  must_change_password TINYINT UNSIGNED NOT NULL DEFAULT 0,
  status          VARCHAR(16)  NOT NULL DEFAULT 'active', -- active|frozen|closed
  created_at      DATETIME(6)  NOT NULL,
  updated_at      DATETIME(6)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_users_phone (phone),
  UNIQUE KEY uk_users_account_name (account_name)
);

CREATE TABLE user_profiles (
  user_id         BIGINT UNSIGNED NOT NULL,
  nickname        VARCHAR(64)  NOT NULL,   -- 2..32 runes (stored as utf8mb4)
  gender          VARCHAR(16)  NOT NULL DEFAULT 'unspecified', -- unspecified|male|female
  region_country  VARCHAR(64)  NOT NULL DEFAULT '',
  region_province VARCHAR(64)  NOT NULL DEFAULT '',
  region_city     VARCHAR(64)  NOT NULL DEFAULT '',
  signature       VARCHAR(255) NOT NULL DEFAULT '',  -- <=100 runes
  status_text     VARCHAR(128) NOT NULL DEFAULT '',  -- <=32 runes
  avatar_media_id BIGINT UNSIGNED NULL,
  created_at      DATETIME(6)  NOT NULL,
  updated_at      DATETIME(6)  NOT NULL,
  PRIMARY KEY (user_id),
  CONSTRAINT fk_profiles_user FOREIGN KEY (user_id) REFERENCES users (id)
) ;

CREATE TABLE user_devices (
  id              VARCHAR(36)  NOT NULL COLLATE utf8mb4_bin, -- server-generated UUID
  user_id         BIGINT UNSIGNED NOT NULL,
  device_name     VARCHAR(128) NOT NULL DEFAULT 'unknown',   -- <=64 runes
  platform        VARCHAR(16)  NOT NULL DEFAULT 'unknown',   -- ios|android|windows|mac|web|unknown
  first_login_at  DATETIME(6)  NOT NULL,
  last_active_at  DATETIME(6)  NOT NULL,
  last_ip         VARCHAR(45)  NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  KEY idx_devices_user (user_id)
);

CREATE TABLE user_sessions (
  id                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id             BIGINT UNSIGNED NOT NULL,
  device_id           VARCHAR(36) NOT NULL COLLATE utf8mb4_bin,
  access_token_hash   CHAR(64)    NOT NULL COLLATE utf8mb4_bin,
  access_expires_at   DATETIME(6) NOT NULL,
  refresh_token_hash  CHAR(64)    NOT NULL COLLATE utf8mb4_bin,
  prev_refresh_token_hash CHAR(64) NULL COLLATE utf8mb4_bin, -- reuse detection (design-review D2)
  refresh_expires_at  DATETIME(6) NOT NULL,
  created_at          DATETIME(6) NOT NULL,
  last_seen_at        DATETIME(6) NOT NULL,
  last_ip             VARCHAR(45) NOT NULL DEFAULT '',
  revoked_at          DATETIME(6) NULL,
  revoke_reason       VARCHAR(64) NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_sessions_access (access_token_hash),
  UNIQUE KEY uk_sessions_refresh (refresh_token_hash),
  KEY idx_sessions_user_active (user_id, revoked_at)
);

-- Placeholder row per account; moment module (phase 3) owns the semantics.
CREATE TABLE user_moment_settings (
  user_id        BIGINT UNSIGNED NOT NULL,
  notify_enabled TINYINT UNSIGNED NOT NULL DEFAULT 1,
  created_at     DATETIME(6) NOT NULL,
  updated_at     DATETIME(6) NOT NULL,
  PRIMARY KEY (user_id)
);

-- ---------------------------------------------------------------------------
-- Conversations (phase-1 minimal slice: direct + transfer creation only)
-- direct_key = SHA2(CONCAT(LEAST(a, b), ':', GREATEST(a, b)), 256) — binary.
CREATE TABLE conversations (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  type              VARCHAR(20) NOT NULL, -- direct|group|transfer|official_service
  transfer_owner_id BIGINT UNSIGNED NULL, -- set for type=transfer, unique
  direct_key        BINARY(32)   NULL,    -- set for type=direct, unique
  last_seq          BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at        DATETIME(6)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_conversations_transfer_owner (transfer_owner_id),
  UNIQUE KEY uk_conversations_direct_key (direct_key)
);

CREATE TABLE conversation_members (
  conversation_id  BIGINT UNSIGNED NOT NULL,
  user_id          BIGINT UNSIGNED NOT NULL,
  membership_epoch INT UNSIGNED NOT NULL DEFAULT 1,
  joined_at        DATETIME(6) NOT NULL,
  left_at          DATETIME(6) NULL,
  PRIMARY KEY (conversation_id, user_id, membership_epoch),
  KEY idx_convmembers_user (user_id, conversation_id)
);

-- ---------------------------------------------------------------------------
-- Friendships (cross-review ruling 3: epoch snapshots, never reused)
CREATE TABLE friendship_epochs (
  user_low      BIGINT UNSIGNED NOT NULL,
  user_high     BIGINT UNSIGNED NOT NULL,
  current_epoch INT UNSIGNED    NOT NULL DEFAULT 0,
  PRIMARY KEY (user_low, user_high),
  KEY idx_fepochs_high (user_high, user_low)
);

CREATE TABLE friendships (
  user_low      BIGINT UNSIGNED NOT NULL,
  user_high     BIGINT UNSIGNED NOT NULL,
  friendship_epoch INT UNSIGNED NOT NULL,
  status        VARCHAR(16) NOT NULL, -- active|deleted
  started_at    DATETIME(6) NOT NULL,
  ended_at      DATETIME(6) NULL,
  PRIMARY KEY (user_low, user_high, friendship_epoch),
  KEY idx_friendships_high (user_high, user_low, friendship_epoch)
);

-- Directional per-owner settings; rows survive friendship deletion and are
-- RESET to defaults when a new epoch starts (SPEC-02 R15).
CREATE TABLE friend_settings (
  owner_id      BIGINT UNSIGNED NOT NULL,
  friend_id     BIGINT UNSIGNED NOT NULL,
  remark        VARCHAR(128) NOT NULL DEFAULT '',  -- <=64 runes
  message_perm  VARCHAR(16)  NOT NULL DEFAULT 'normal',  -- normal|no_message|blocked
  moment_perm   VARCHAR(16)  NOT NULL DEFAULT 'visible', -- visible|hidden
  moment_notify TINYINT UNSIGNED NOT NULL DEFAULT 1,
  created_at    DATETIME(6) NOT NULL,
  updated_at    DATETIME(6) NOT NULL,
  PRIMARY KEY (owner_id, friend_id),
  KEY idx_fsettings_friend (friend_id)
);

CREATE TABLE friend_requests (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  applicant_id  BIGINT UNSIGNED NOT NULL,
  target_id     BIGINT UNSIGNED NOT NULL,
  source        VARCHAR(16) NOT NULL, -- phone|account_name|qrcode|group|card
  verify_text   VARCHAR(255) NOT NULL DEFAULT '', -- <=200 runes
  status        VARCHAR(16) NOT NULL, -- pending|accepted|rejected|expired|cancelled
  pending_guard TINYINT UNSIGNED AS (CASE WHEN status = 'pending' THEN 1 ELSE NULL END) STORED,
  rejected_at   DATETIME(6) NULL,
  created_at    DATETIME(6) NOT NULL,
  updated_at    DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_frequests_pending (applicant_id, target_id, pending_guard),
  KEY idx_frequests_target (target_id, status, created_at),
  KEY idx_frequests_applicant (applicant_id, status, created_at)
);

CREATE TABLE contact_tags (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  owner_id   BIGINT UNSIGNED NOT NULL,
  name       VARCHAR(128) NOT NULL, -- <=32 runes
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_ctags_owner_name (owner_id, name)
);

CREATE TABLE contact_tag_members (
  tag_id    BIGINT UNSIGNED NOT NULL,
  friend_id BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (tag_id, friend_id),
  KEY idx_ctagmembers_friend (friend_id)
);

-- ---------------------------------------------------------------------------
-- Media (phase 1: upload sessions + objects + references; variants empty)
CREATE TABLE upload_sessions (
  id               CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  owner_id         BIGINT UNSIGNED NOT NULL,
  file_name        VARCHAR(255) NOT NULL,
  declared_size    BIGINT UNSIGNED NOT NULL,
  declared_mime    VARCHAR(128) NOT NULL,
  declared_sha256  CHAR(64) COLLATE utf8mb4_bin NOT NULL,
  purpose          VARCHAR(16) NOT NULL, -- message|moment|favorite|avatar|transfer
  chunk_size       BIGINT UNSIGNED NOT NULL,
  total_chunks     INT UNSIGNED NOT NULL,
  status           VARCHAR(16) NOT NULL, -- open|completed|aborted|expired
  media_object_id  BIGINT UNSIGNED NULL,
  expires_at       DATETIME(6) NOT NULL,
  created_at       DATETIME(6) NOT NULL,
  updated_at       DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_uploads_owner (owner_id, status),
  KEY idx_uploads_expires (status, expires_at)
);

CREATE TABLE upload_chunks (
  upload_id   CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  chunk_index INT UNSIGNED NOT NULL,
  size        BIGINT UNSIGNED NOT NULL,
  received_at DATETIME(6) NOT NULL,
  PRIMARY KEY (upload_id, chunk_index)
);

CREATE TABLE media_objects (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  owner_id   BIGINT UNSIGNED NOT NULL,
  bucket_key VARCHAR(255) COLLATE utf8mb4_bin NOT NULL,
  size       BIGINT UNSIGNED NOT NULL,
  sha256     CHAR(64) COLLATE utf8mb4_bin NOT NULL,
  mime       VARCHAR(128) NOT NULL,
  purpose    VARCHAR(16) NOT NULL,
  status     VARCHAR(16) NOT NULL, -- ready|cleaned|deleted
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_media_bucket_key (bucket_key),
  KEY idx_media_owner (owner_id, created_at),
  KEY idx_media_sha (sha256)
);

CREATE TABLE media_variants (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  object_id  BIGINT UNSIGNED NOT NULL,
  kind       VARCHAR(16) NOT NULL, -- thumbnail|play|cover (phase 2)
  bucket_key VARCHAR(255) COLLATE utf8mb4_bin NOT NULL,
  size       BIGINT UNSIGNED NOT NULL,
  width      INT UNSIGNED NULL,
  height     INT UNSIGNED NULL,
  duration_ms INT UNSIGNED NULL,
  status     VARCHAR(16) NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_variants_object (object_id)
);

CREATE TABLE media_references (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  object_id  BIGINT UNSIGNED NOT NULL,
  biz_type   VARCHAR(16) NOT NULL, -- message|moment|favorite|avatar
  biz_id     VARCHAR(64) COLLATE utf8mb4_bin NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_mrefs_object (object_id),
  KEY idx_mrefs_biz (biz_type, biz_id)
);

CREATE TABLE media_user_references (
  id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  object_id        BIGINT UNSIGNED NOT NULL,
  user_id          BIGINT UNSIGNED NOT NULL,
  granted_by_ref_id BIGINT UNSIGNED NOT NULL,
  revoked_at       DATETIME(6) NULL,
  created_at       DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_murefs_object_user (object_id, user_id, revoked_at)
);

-- ---------------------------------------------------------------------------
-- Outbox / Inbox (contract §12.5; relay leases per design-review D4)
CREATE TABLE outbox_events (
  event_id         CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  type             VARCHAR(64) NOT NULL,
  aggregate_id     VARCHAR(64) COLLATE utf8mb4_bin NOT NULL,
  version          BIGINT NOT NULL DEFAULT 0,
  queue            VARCHAR(64) NOT NULL,
  status           VARCHAR(16) NOT NULL, -- pending|publishing|published|failed
  lease_owner      VARCHAR(64) NULL,
  lease_expires_at DATETIME(6) NULL,
  attempt          INT UNSIGNED NOT NULL DEFAULT 0,
  last_error       VARCHAR(255) NULL,
  created_at       DATETIME(6) NOT NULL,
  updated_at       DATETIME(6) NOT NULL,
  PRIMARY KEY (event_id),
  KEY idx_outbox_poll (status, created_at)
);

CREATE TABLE inbox_events (
  consumer_name     VARCHAR(64) NOT NULL,
  event_id          CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  status            VARCHAR(16) NOT NULL, -- received|processed|failed
  first_received_at DATETIME(6) NOT NULL,
  processed_at      DATETIME(6) NULL,
  last_error        VARCHAR(255) NULL,
  PRIMARY KEY (consumer_name, event_id)
);

-- Async retry / dead letters (contract §12.5.5)
CREATE TABLE async_retry_tasks (
  consumer_name  VARCHAR(64) NOT NULL,
  event_id       CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  attempt_count  INT UNSIGNED NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(6) NOT NULL,
  last_error     VARCHAR(255) NULL,
  status         VARCHAR(16) NOT NULL, -- pending|done|dead
  created_at     DATETIME(6) NOT NULL,
  updated_at     DATETIME(6) NOT NULL,
  PRIMARY KEY (consumer_name, event_id),
  KEY idx_retry_due (status, next_attempt_at)
);

CREATE TABLE async_dead_letters (
  event_id       CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  consumer_name  VARCHAR(64) NOT NULL,
  last_error     VARCHAR(255) NOT NULL,
  failed_at      DATETIME(6) NOT NULL,
  PRIMARY KEY (event_id, consumer_name)
);

-- ---------------------------------------------------------------------------
-- Audit (contract §16.2)
CREATE TABLE audit_logs (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  event_type VARCHAR(64) NOT NULL,
  actor_id   BIGINT UNSIGNED NULL,      -- null = anonymous/system
  actor_ip   VARCHAR(45) NOT NULL DEFAULT '',
  detail     JSON NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_audit_actor (actor_id, created_at),
  KEY idx_audit_type (event_type, created_at)
);

-- +goose Down
DROP TABLE upload_chunks;
DROP TABLE audit_logs;
DROP TABLE async_dead_letters;
DROP TABLE async_retry_tasks;
DROP TABLE inbox_events;
DROP TABLE outbox_events;
DROP TABLE media_user_references;
DROP TABLE media_references;
DROP TABLE media_variants;
DROP TABLE media_objects;
DROP TABLE upload_sessions;
DROP TABLE contact_tag_members;
DROP TABLE contact_tags;
DROP TABLE friend_requests;
DROP TABLE friend_settings;
DROP TABLE friendships;
DROP TABLE friendship_epochs;
DROP TABLE conversation_members;
DROP TABLE conversations;
DROP TABLE user_moment_settings;
DROP TABLE user_sessions;
DROP TABLE user_devices;
DROP TABLE user_profiles;
DROP TABLE users;
