-- +goose Up
-- +goose StatementBegin
CREATE TABLE operator_admins (
  user_id    BIGINT UNSIGNED NOT NULL,
  added_by   BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (user_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE reports (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  reporter_id BIGINT UNSIGNED NOT NULL,
  target_type VARCHAR(16) NOT NULL,
  target_id   BIGINT UNSIGNED NOT NULL,
  reason      VARCHAR(32) NOT NULL,
  note        VARCHAR(1000) NOT NULL DEFAULT '',
  status      VARCHAR(16) NOT NULL DEFAULT 'pending',
  handled_by  BIGINT UNSIGNED NULL,
  handled_at  DATETIME(6) NULL,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_reports_dedupe (reporter_id, target_type, target_id),
  KEY idx_reports_status (status, created_at)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE moderation_actions (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  operator_id BIGINT UNSIGNED NOT NULL,
  action      VARCHAR(24) NOT NULL,
  target_type VARCHAR(16) NOT NULL,
  target_id   BIGINT UNSIGNED NOT NULL,
  note        VARCHAR(500) NOT NULL DEFAULT '',
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_modact_target (target_type, target_id)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS moderation_actions;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS reports;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS operator_admins;
-- +goose StatementEnd
