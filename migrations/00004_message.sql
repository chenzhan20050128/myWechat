-- +goose Up
-- +goose StatementBegin
CREATE TABLE messages (
  id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  conversation_id  BIGINT UNSIGNED NOT NULL,
  conversation_seq BIGINT UNSIGNED NOT NULL,
  sender_id        BIGINT UNSIGNED NOT NULL,
  sender_type      VARCHAR(8) NOT NULL,
  client_msg_id    VARCHAR(64) COLLATE utf8mb4_bin NOT NULL DEFAULT '',
  type             VARCHAR(16) NOT NULL,
  payload          JSON NOT NULL,
  status           VARCHAR(16) NOT NULL,
  created_at       DATETIME(6) NOT NULL,
  expires_at       DATETIME(6) NOT NULL,
  recalled_at      DATETIME(6) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_messages_client (sender_id, client_msg_id),
  UNIQUE KEY uk_messages_conv_seq (conversation_id, conversation_seq),
  KEY idx_messages_expiry (status, expires_at),
  KEY idx_messages_conv_time (conversation_id, id),
  KEY idx_messages_created (created_at)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE message_assets (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  message_id     BIGINT UNSIGNED NOT NULL,
  media_object_id BIGINT UNSIGNED NOT NULL,
  kind           VARCHAR(16) NOT NULL,
  created_at     DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_massets_msg (message_id),
  KEY idx_massets_obj (media_object_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE message_references (
  message_id     BIGINT UNSIGNED NOT NULL,
  ref_message_id BIGINT UNSIGNED NOT NULL,
  ref_sender_id  BIGINT UNSIGNED NOT NULL,
  ref_type       VARCHAR(16) NOT NULL,
  ref_digest     VARCHAR(255) NOT NULL,
  created_at     DATETIME(6) NOT NULL,
  PRIMARY KEY (message_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE message_forwards (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  source_message_id BIGINT UNSIGNED NOT NULL,
  target_message_id BIGINT UNSIGNED NOT NULL,
  forwarded_by      BIGINT UNSIGNED NOT NULL,
  created_at        DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_mfwd_source (source_message_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE message_pins (
  conversation_id BIGINT UNSIGNED NOT NULL,
  message_id      BIGINT UNSIGNED NOT NULL,
  pinned_by       BIGINT UNSIGNED NOT NULL,
  created_at      DATETIME(6) NOT NULL,
  PRIMARY KEY (conversation_id, message_id),
  KEY idx_mpins_conv_created (conversation_id, created_at)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE conversation_settings (
  conversation_id   BIGINT UNSIGNED NOT NULL,
  user_id           BIGINT UNSIGNED NOT NULL,
  pinned            TINYINT UNSIGNED NOT NULL DEFAULT 0,
  muted             TINYINT UNSIGNED NOT NULL DEFAULT 0,
  background        VARCHAR(255) NOT NULL DEFAULT '',
  last_read_seq     BIGINT UNSIGNED NOT NULL DEFAULT 0,
  is_marked_unread  TINYINT UNSIGNED NOT NULL DEFAULT 0,
  unread_anchor_seq BIGINT UNSIGNED NOT NULL DEFAULT 0,
  updated_at        DATETIME(6) NOT NULL,
  PRIMARY KEY (conversation_id, user_id)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS conversation_settings;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS message_pins;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS message_forwards;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS message_references;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS message_assets;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS messages;
-- +goose StatementEnd
