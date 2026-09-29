-- +goose Up
-- +goose StatementBegin
CREATE TABLE backup_slots (
  account_id       BIGINT UNSIGNED NOT NULL,
  active_backup_id BIGINT UNSIGNED NULL,
  updated_at       DATETIME(6) NOT NULL,
  PRIMARY KEY (account_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE backups (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id   BIGINT UNSIGNED NOT NULL,
  status       VARCHAR(16) NOT NULL DEFAULT 'creating',
  scope        JSON NOT NULL,
  total_size   BIGINT UNSIGNED NOT NULL DEFAULT 0,
  part_count   INT UNSIGNED NOT NULL DEFAULT 0,
  object_key   VARCHAR(255) NOT NULL DEFAULT '',
  sha256       CHAR(64) NOT NULL DEFAULT '',
  created_at   DATETIME(6) NOT NULL,
  completed_at DATETIME(6) NULL,
  expires_at   DATETIME(6) NULL,
  deleted_at   DATETIME(6) NULL,
  fail_reason  VARCHAR(255) NULL,
  PRIMARY KEY (id),
  KEY idx_backups_account (account_id, status)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE backup_parts (
  backup_id BIGINT UNSIGNED NOT NULL,
  part_no   INT UNSIGNED NOT NULL,
  object_key VARCHAR(255) NOT NULL,
  size      BIGINT UNSIGNED NOT NULL,
  sha256    CHAR(64) NOT NULL,
  uploaded  TINYINT UNSIGNED NOT NULL DEFAULT 0,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (backup_id, part_no)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE backup_manifest_items (
  backup_id   BIGINT UNSIGNED NOT NULL,
  section     VARCHAR(32) NOT NULL,
  item_count  INT UNSIGNED NOT NULL,
  sha256      CHAR(64) NOT NULL,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (backup_id, section)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE backup_restore_jobs (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id  BIGINT UNSIGNED NOT NULL,
  backup_id   BIGINT UNSIGNED NOT NULL,
  status      VARCHAR(16) NOT NULL DEFAULT 'running',
  fail_reason VARCHAR(255) NULL,
  started_at  DATETIME(6) NOT NULL,
  finished_at DATETIME(6) NULL,
  PRIMARY KEY (id),
  KEY idx_restore_account (account_id, status)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE backup_restore_staging (
  job_id    BIGINT UNSIGNED NOT NULL,
  section   VARCHAR(32) NOT NULL,
  payload   JSON NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (job_id, section),
  KEY idx_staging_job (job_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE restored_profile_snapshots (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id  BIGINT UNSIGNED NOT NULL,
  restore_job_id BIGINT UNSIGNED NOT NULL,
  profile     JSON NOT NULL,
  size_bytes  BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_rprofile_account (account_id, id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE restored_message_snapshots (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id  BIGINT UNSIGNED NOT NULL,
  restore_job_id BIGINT UNSIGNED NOT NULL,
  conversation_id   BIGINT UNSIGNED NOT NULL,
  conversation_type VARCHAR(16) NOT NULL,
  counterparty JSON NOT NULL,
  message    JSON NOT NULL,
  orig_message_id BIGINT UNSIGNED NOT NULL,
  orig_created_at DATETIME(6) NOT NULL,
  size_bytes  BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_rmsg_account (account_id, restore_job_id, conversation_id, id),
  KEY idx_rmsg_orig (account_id, orig_message_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE transfer_handshakes (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id  BIGINT UNSIGNED NOT NULL,
  source_device VARCHAR(36) NOT NULL,
  target_device VARCHAR(36) NOT NULL,
  code        VARCHAR(16) NOT NULL,
  status      VARCHAR(16) NOT NULL DEFAULT 'pending',
  payload_object_key VARCHAR(255) NOT NULL DEFAULT '',
  sha256      CHAR(64) NOT NULL DEFAULT '',
  expires_at  DATETIME(6) NOT NULL,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_handshake_account (account_id, status)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS transfer_handshakes;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS restored_message_snapshots;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS restored_profile_snapshots;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS backup_restore_staging;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS backup_restore_jobs;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS backup_manifest_items;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS backup_parts;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS backups;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS backup_slots;
-- +goose StatementEnd
