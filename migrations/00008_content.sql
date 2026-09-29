-- +goose Up
-- +goose StatementBegin
CREATE TABLE official_accounts (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  name        VARCHAR(64) NOT NULL,
  avatar_media_id BIGINT UNSIGNED NULL,
  intro       VARCHAR(500) NOT NULL DEFAULT '',
  status      VARCHAR(16) NOT NULL DEFAULT 'draft',
  creator_id  BIGINT UNSIGNED NOT NULL,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_oaccounts_name (name),
  KEY idx_oaccounts_status (status)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE official_account_staff (
  account_id BIGINT UNSIGNED NOT NULL,
  user_id    BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (account_id, user_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE official_followers (
  account_id  BIGINT UNSIGNED NOT NULL,
  user_id     BIGINT UNSIGNED NOT NULL,
  muted       TINYINT UNSIGNED NOT NULL DEFAULT 0,
  followed_at DATETIME(6) NOT NULL,
  unfollowed_at DATETIME(6) NULL,
  active_flag TINYINT UNSIGNED GENERATED ALWAYS AS (IF(unfollowed_at IS NULL, 1, NULL)) STORED,
  PRIMARY KEY (account_id, user_id, followed_at),
  UNIQUE KEY uk_ofollow_active (account_id, user_id, active_flag),
  KEY idx_ofollow_user (user_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE official_articles (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id  BIGINT UNSIGNED NOT NULL,
  title       VARCHAR(128) NOT NULL,
  cover_media_id BIGINT UNSIGNED NULL,
  summary     VARCHAR(512) NOT NULL DEFAULT '',
  body        MEDIUMTEXT NOT NULL,
  status      VARCHAR(16) NOT NULL DEFAULT 'draft',
  published_at DATETIME(6) NULL,
  unpublished_at DATETIME(6) NULL,
  created_at  DATETIME(6) NOT NULL,
  updated_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_oarticles_account (account_id, status, published_at)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE official_article_reads (
  account_id BIGINT UNSIGNED NOT NULL,
  article_id BIGINT UNSIGNED NOT NULL,
  user_id    BIGINT UNSIGNED NOT NULL,
  first_read_at  DATETIME(6) NOT NULL,
  last_read_at   DATETIME(6) NOT NULL,
  PRIMARY KEY (account_id, article_id, user_id),
  KEY idx_oreads_user (user_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE official_menus (
  account_id BIGINT UNSIGNED NOT NULL,
  level      TINYINT UNSIGNED NOT NULL,
  parent_pos TINYINT UNSIGNED NULL,
  position   TINYINT UNSIGNED NOT NULL,
  label      VARCHAR(32) NOT NULL,
  action     VARCHAR(16) NOT NULL,
  article_id BIGINT UNSIGNED NULL,
  PRIMARY KEY (account_id, level, position),
  KEY idx_omenu_parent (account_id, parent_pos, position)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE official_notifications (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id  BIGINT UNSIGNED NOT NULL,
  user_id     BIGINT UNSIGNED NOT NULL,
  kind        VARCHAR(16) NOT NULL,
  article_id  BIGINT UNSIGNED NULL,
  title       VARCHAR(128) NOT NULL DEFAULT '',
  created_at  DATETIME(6) NOT NULL,
  read_at     DATETIME(6) NULL,
  PRIMARY KEY (id),
  KEY idx_onotif_user (user_id, read_at, id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE service_sessions (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  official_account_id BIGINT UNSIGNED NOT NULL,
  user_id       BIGINT UNSIGNED NOT NULL,
  session_number INT UNSIGNED NOT NULL,
  conversation_id BIGINT UNSIGNED NOT NULL,
  status        VARCHAR(16) NOT NULL DEFAULT 'active',
  closed_by     BIGINT UNSIGNED NULL,
  closed_at     DATETIME(6) NULL,
  created_at    DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_ssessions_key (official_account_id, user_id, session_number),
  KEY idx_ssessions_conv (conversation_id),
  KEY idx_ssessions_active (official_account_id, user_id, status)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE service_session_staff (
  session_id BIGINT UNSIGNED NOT NULL,
  staff_id   BIGINT UNSIGNED NOT NULL,
  assigned_at DATETIME(6) NOT NULL,
  PRIMARY KEY (session_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE service_session_events (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  session_id BIGINT UNSIGNED NOT NULL,
  event      VARCHAR(24) NOT NULL,
  actor_id   BIGINT UNSIGNED NOT NULL,
  detail     JSON NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_ssevents_session (session_id, id)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS service_session_events;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS service_session_staff;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS service_sessions;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS official_notifications;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS official_menus;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS official_article_reads;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS official_articles;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS official_followers;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS official_account_staff;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS official_accounts;
-- +goose StatementEnd
