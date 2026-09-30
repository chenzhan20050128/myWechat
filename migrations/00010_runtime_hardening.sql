-- +goose Up
-- +goose StatementBegin
ALTER TABLE outbox_events
  ADD COLUMN lease_version INT UNSIGNED NOT NULL DEFAULT 0,
  ADD COLUMN published_at DATETIME(6) NULL;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE async_retry_tasks
  ADD COLUMN queue VARCHAR(64) NOT NULL DEFAULT '',
  ADD COLUMN event_type VARCHAR(64) NOT NULL DEFAULT '',
  ADD COLUMN aggregate_id VARCHAR(64) COLLATE utf8mb4_bin NOT NULL DEFAULT '',
  ADD COLUMN version BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN lease_until DATETIME(6) NULL,
  ADD COLUMN dispatched_at DATETIME(6) NULL,
  ADD KEY idx_retry_lease (status, lease_until);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE async_dead_letters
  ADD COLUMN envelope JSON NULL;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE upload_sessions
  ADD COLUMN assembly_owner VARCHAR(64) NULL,
  ADD COLUMN assembly_expires_at DATETIME(6) NULL,
  ADD KEY idx_uploads_assembly (status, assembly_expires_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE media_gc_tasks (
  object_id BIGINT UNSIGNED NOT NULL,
  status VARCHAR(16) NOT NULL,
  lease_until DATETIME(6) NULL,
  purge_after DATETIME(6) NOT NULL,
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  PRIMARY KEY (object_id),
  KEY idx_media_gc_status (status, updated_at)
);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE official_notifications
  ADD UNIQUE KEY uk_onotif_article (account_id, article_id, user_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE official_notifications
  DROP KEY uk_onotif_article;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE async_dead_letters
  DROP COLUMN envelope;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS media_gc_tasks;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE upload_sessions
  DROP KEY idx_uploads_assembly,
  DROP COLUMN assembly_expires_at,
  DROP COLUMN assembly_owner;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE async_retry_tasks
  DROP KEY idx_retry_lease,
  DROP COLUMN dispatched_at,
  DROP COLUMN lease_until,
  DROP COLUMN version,
  DROP COLUMN aggregate_id,
  DROP COLUMN event_type,
  DROP COLUMN queue;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE outbox_events
  DROP COLUMN published_at,
  DROP COLUMN lease_version;
-- +goose StatementEnd
