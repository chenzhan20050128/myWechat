-- +goose Up
-- +goose StatementBegin
CREATE TABLE moments (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  author_id      BIGINT UNSIGNED NOT NULL,
  content        VARCHAR(2000) NOT NULL DEFAULT '',
  country        VARCHAR(64) NOT NULL DEFAULT '',
  province       VARCHAR(64) NOT NULL DEFAULT '',
  city           VARCHAR(64) NOT NULL DEFAULT '',
  place_name     VARCHAR(128) NOT NULL DEFAULT '',
  allow_comments TINYINT UNSIGNED NOT NULL DEFAULT 1,
  allow_likes    TINYINT UNSIGNED NOT NULL DEFAULT 1,
  status         VARCHAR(16) NOT NULL DEFAULT 'visible',
  deleted_at     DATETIME(6) NULL,
  created_at     DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_moments_author (author_id, status, id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE moment_assets (
  moment_id       BIGINT UNSIGNED NOT NULL,
  media_object_id BIGINT UNSIGNED NOT NULL,
  position        TINYINT UNSIGNED NOT NULL,
  created_at      DATETIME(6) NOT NULL,
  PRIMARY KEY (moment_id, position),
  KEY idx_massets_obj (media_object_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE moment_visibility_users (
  moment_id        BIGINT UNSIGNED NOT NULL,
  user_id          BIGINT UNSIGNED NOT NULL,
  friendship_epoch BIGINT UNSIGNED NOT NULL,
  allowed          TINYINT UNSIGNED NOT NULL,
  snapshot_at      DATETIME(6) NOT NULL,
  PRIMARY KEY (moment_id, user_id),
  KEY idx_mvis_user (user_id, allowed)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE moment_likes (
  moment_id  BIGINT UNSIGNED NOT NULL,
  user_id    BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (moment_id, user_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE moment_comments (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  moment_id   BIGINT UNSIGNED NOT NULL,
  user_id     BIGINT UNSIGNED NOT NULL,
  reply_to    BIGINT UNSIGNED NULL,
  content     VARCHAR(500) NOT NULL,
  status      VARCHAR(16) NOT NULL DEFAULT 'visible',
  deleted_at  DATETIME(6) NULL,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_mcomments_moment (moment_id, status, id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE moment_notifications (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  recipient_id BIGINT UNSIGNED NOT NULL,
  moment_id   BIGINT UNSIGNED NOT NULL,
  kind        VARCHAR(8) NOT NULL,
  actor_id    BIGINT UNSIGNED NOT NULL,
  created_at  DATETIME(6) NOT NULL,
  read_at     DATETIME(6) NULL,
  PRIMARY KEY (id),
  KEY idx_mnotif_recipient (recipient_id, read_at, id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE moment_schedules (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  author_id         BIGINT UNSIGNED NOT NULL,
  run_at            DATETIME(6) NOT NULL,
  timezone          VARCHAR(64) NOT NULL DEFAULT 'Asia/Shanghai',
  status            VARCHAR(16) NOT NULL DEFAULT 'scheduled',
  current_version   INT UNSIGNED NOT NULL DEFAULT 1,
  execution_version INT UNSIGNED NOT NULL DEFAULT 0,
  lease_owner       VARCHAR(64) NULL,
  lease_until       DATETIME(6) NULL,
  moment_id         BIGINT UNSIGNED NULL,
  fail_reason       VARCHAR(255) NULL,
  retry_count        INT UNSIGNED NOT NULL DEFAULT 0,
  created_at        DATETIME(6) NOT NULL,
  updated_at        DATETIME(6) NOT NULL,
  published_at      DATETIME(6) NULL,
  cancelled_at      DATETIME(6) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_mschedules_moment (moment_id),
  KEY idx_mschedules_due (status, run_at)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE moment_schedule_contents (
  schedule_id BIGINT UNSIGNED NOT NULL,
  version     INT UNSIGNED NOT NULL,
  content     VARCHAR(2000) NOT NULL DEFAULT '',
  country     VARCHAR(64) NOT NULL DEFAULT '',
  province    VARCHAR(64) NOT NULL DEFAULT '',
  city        VARCHAR(64) NOT NULL DEFAULT '',
  place_name  VARCHAR(128) NOT NULL DEFAULT '',
  PRIMARY KEY (schedule_id, version)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE moment_schedule_assets (
  schedule_id     BIGINT UNSIGNED NOT NULL,
  version         INT UNSIGNED NOT NULL,
  media_object_id BIGINT UNSIGNED NOT NULL,
  position        TINYINT UNSIGNED NOT NULL,
  PRIMARY KEY (schedule_id, version, position)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE moment_schedule_visibility_users (
  schedule_id       BIGINT UNSIGNED NOT NULL,
  version           INT UNSIGNED NOT NULL,
  user_id           BIGINT UNSIGNED NOT NULL,
  friendship_epoch  BIGINT UNSIGNED NOT NULL,
  allowed           TINYINT UNSIGNED NOT NULL,
  PRIMARY KEY (schedule_id, version, user_id)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS moment_schedule_visibility_users;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS moment_schedule_assets;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS moment_schedule_contents;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS moment_schedules;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS moment_notifications;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS moment_comments;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS moment_likes;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS moment_visibility_users;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS moment_assets;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS moments;
-- +goose StatementEnd
