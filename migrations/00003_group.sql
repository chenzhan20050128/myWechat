-- +goose Up
-- +goose StatementBegin
CREATE TABLE `groups` (
  id                    BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  name                  VARCHAR(64) NOT NULL,
  owner_id              BIGINT UNSIGNED NOT NULL,
  avatar_media_id       BIGINT UNSIGNED NULL,
  announcement          VARCHAR(2000) NOT NULL DEFAULT '',
  announcement_updated_by BIGINT UNSIGNED NULL,
  announcement_updated_at DATETIME(6) NULL,
  member_count          INT UNSIGNED NOT NULL DEFAULT 1,
  status                VARCHAR(16) NOT NULL DEFAULT 'active',
  conversation_id       BIGINT UNSIGNED NOT NULL,
  created_at            DATETIME(6) NOT NULL,
  dissolved_at          DATETIME(6) NULL,
  PRIMARY KEY (id),
  KEY idx_groups_owner (owner_id),
  KEY idx_groups_conv (conversation_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE group_members (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  group_id    BIGINT UNSIGNED NOT NULL,
  user_id     BIGINT UNSIGNED NOT NULL,
  role        VARCHAR(8) NOT NULL DEFAULT 'member',
  joined_at   DATETIME(6) NOT NULL,
  left_at     DATETIME(6) NULL,
  left_reason VARCHAR(16) NOT NULL DEFAULT '',
  active_flag TINYINT UNSIGNED GENERATED ALWAYS AS (IF(left_at IS NULL, 1, NULL)) STORED,
  PRIMARY KEY (id),
  UNIQUE KEY uk_gmembers_active (group_id, user_id, active_flag),
  KEY idx_gmembers_user (user_id, left_at),
  KEY idx_gmembers_group_left (group_id, left_at, joined_at)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE group_invite_codes (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  group_id    BIGINT UNSIGNED NOT NULL,
  code        VARCHAR(12) COLLATE utf8mb4_bin NOT NULL,
  created_by  BIGINT UNSIGNED NOT NULL,
  max_uses    INT UNSIGNED NOT NULL DEFAULT 50,
  use_count   INT UNSIGNED NOT NULL DEFAULT 0,
  expires_at  DATETIME(6) NOT NULL,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_ginvite_code (code),
  KEY idx_ginvite_group (group_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE group_invite_uses (
  code_id   BIGINT UNSIGNED NOT NULL,
  user_id   BIGINT UNSIGNED NOT NULL,
  joined_at DATETIME(6) NOT NULL,
  PRIMARY KEY (code_id, user_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE group_mutes (
  group_id   BIGINT UNSIGNED NOT NULL,
  user_id    BIGINT UNSIGNED NOT NULL,
  until_at   DATETIME(6) NULL,
  created_by BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (group_id, user_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE group_events (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  group_id   BIGINT UNSIGNED NOT NULL,
  event      VARCHAR(32) NOT NULL,
  actor_id   BIGINT UNSIGNED NOT NULL,
  target_id  BIGINT UNSIGNED NULL,
  detail     JSON NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_gevents_group (group_id, id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE group_todos (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  group_id    BIGINT UNSIGNED NOT NULL,
  title       VARCHAR(200) NOT NULL,
  description VARCHAR(2000) NOT NULL DEFAULT '',
  due_at      DATETIME(6) NULL,
  status      VARCHAR(16) NOT NULL DEFAULT 'active',
  created_by  BIGINT UNSIGNED NOT NULL,
  created_at  DATETIME(6) NOT NULL,
  updated_at  DATETIME(6) NOT NULL,
  completed_at DATETIME(6) NULL,
  cancelled_at DATETIME(6) NULL,
  deleted_at  DATETIME(6) NULL,
  PRIMARY KEY (id),
  KEY idx_gtodos_group (group_id, status, deleted_at)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE group_todo_assignees (
  todo_id     BIGINT UNSIGNED NOT NULL,
  user_id     BIGINT UNSIGNED NOT NULL,
  status      VARCHAR(16) NOT NULL DEFAULT 'assigned',
  completed_at DATETIME(6) NULL,
  PRIMARY KEY (todo_id, user_id),
  KEY idx_gtassign_user (user_id, status)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE group_todo_member_snapshots (
  todo_id   BIGINT UNSIGNED NOT NULL,
  user_id   BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (todo_id, user_id),
  KEY idx_gtsnap_user (user_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE group_todo_events (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  todo_id    BIGINT UNSIGNED NOT NULL,
  event      VARCHAR(24) NOT NULL,
  actor_id   BIGINT UNSIGNED NOT NULL,
  detail     JSON NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_gtevents_todo (todo_id, id)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS group_todo_events;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS group_todo_member_snapshots;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS group_todo_assignees;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS group_todos;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS group_events;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS group_mutes;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS group_invite_uses;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS group_invite_codes;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS group_members;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS `groups`;
-- +goose StatementEnd
