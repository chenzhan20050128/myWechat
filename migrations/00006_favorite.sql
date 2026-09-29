-- +goose Up
-- +goose StatementBegin
CREATE TABLE favorites (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  owner_id    BIGINT UNSIGNED NOT NULL,
  kind        VARCHAR(16) NOT NULL,
  content     JSON NOT NULL,
  source_message_id BIGINT UNSIGNED NULL,
  source_sender_id  BIGINT UNSIGNED NULL,
  source_sent_at    DATETIME(6) NULL,
  source_type  VARCHAR(16) NULL,
  total_size  BIGINT UNSIGNED NOT NULL DEFAULT 0,
  status      VARCHAR(16) NOT NULL DEFAULT 'active',
  active_flag TINYINT UNSIGNED GENERATED ALWAYS AS (IF(status='active', 1, NULL)) STORED,
  deleted_at  DATETIME(6) NULL,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_fav_dedupe (owner_id, source_message_id, active_flag),
  KEY idx_fav_owner (owner_id, status, id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE favorite_tags (
  owner_id  BIGINT UNSIGNED NOT NULL,
  tag       VARCHAR(32) NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (owner_id, tag)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE favorite_tag_items (
  favorite_id BIGINT UNSIGNED NOT NULL,
  tag         VARCHAR(32) NOT NULL,
  PRIMARY KEY (favorite_id, tag),
  KEY idx_ftag_tag (tag, favorite_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE favorite_assets (
  favorite_id BIGINT UNSIGNED NOT NULL,
  position    SMALLINT UNSIGNED NOT NULL,
  item_snapshot JSON NOT NULL,
  PRIMARY KEY (favorite_id, position)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE favorite_media_objects (
  favorite_id     BIGINT UNSIGNED NOT NULL,
  position        SMALLINT UNSIGNED NOT NULL,
  media_object_id BIGINT UNSIGNED NOT NULL,
  created_at      DATETIME(6) NOT NULL,
  PRIMARY KEY (favorite_id, position),
  KEY idx_favmedia_obj (media_object_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE storage_cleanup_jobs (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id     BIGINT UNSIGNED NOT NULL,
  scope       VARCHAR(16) NOT NULL,
  filter      JSON NOT NULL,
  preview     JSON NOT NULL,
  status      VARCHAR(16) NOT NULL DEFAULT 'previewed',
  confirmed_at DATETIME(6) NULL,
  finished_at DATETIME(6) NULL,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_cleanup_user (user_id, status)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE storage_cleanup_items (
  item_id     BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  cleanup_id  BIGINT UNSIGNED NOT NULL,
  media_user_reference_id BIGINT UNSIGNED NOT NULL,
  media_object_id BIGINT UNSIGNED NOT NULL,
  state       VARCHAR(16) NOT NULL,
  error       VARCHAR(255) NULL,
  updated_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (item_id),
  KEY idx_cleanup_items_job (cleanup_id, state)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE media_gc_queue (
  media_object_id BIGINT UNSIGNED NOT NULL,
  enqueued_at     DATETIME(6) NOT NULL,
  purge_after     DATETIME(6) NOT NULL,
  state           VARCHAR(16) NOT NULL DEFAULT 'queued',
  purged_at       DATETIME(6) NULL,
  PRIMARY KEY (media_object_id),
  KEY idx_gc_due (state, purge_after)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS media_gc_queue;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS storage_cleanup_items;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS storage_cleanup_jobs;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS favorite_media_objects;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS favorite_assets;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS favorite_tag_items;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS favorite_tags;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS favorites;
-- +goose StatementEnd
