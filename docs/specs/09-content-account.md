# SPEC-09 content-account（公众号/服务号：图文、菜单、客服、通知）

> 依据：合同 §11、§13.7；复用 message 链路（合同 §11.4）。
> 读者：低等级 LLM 执行者。**全部技术判断已写死；未覆盖情况 → 停下记录 PROGRESS.md。**
> 前置：message 模块完成（official_service 会话走统一消息链路）；运营端鉴权（staff 身份）由本模块自管。

## 1. 范围

**做**：内容账号 CRUD（运营侧）、关注/取关、文章（草稿/发布/下架/阅读记录）、固定菜单（3×5、两类动作）、一对一客服会话（active/closed、用户 3 种消息类型、客服文字）、账号通知（3 条/天上限、幂等）。
**不做**：全网内容搜索、推荐、商业化统计、转接/协同/机器人/排队、脚本/小程序/支付菜单。

## 2. 数据模型（迁移 `migrations/00007_content_account.sql`）

```sql
CREATE TABLE official_accounts (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  name        VARCHAR(64) NOT NULL,          -- 精确账号名（查询键）
  avatar_media_id BIGINT UNSIGNED NULL,
  intro       VARCHAR(500) NOT NULL DEFAULT '',
  status      VARCHAR(16) NOT NULL DEFAULT 'draft',  -- draft|active|frozen|closed
  creator_id  BIGINT UNSIGNED NOT NULL,       -- 运营管理员 user_id（operator 域）
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_oaccounts_name (name),
  KEY idx_oaccounts_status (status)
);

CREATE TABLE official_account_staff (
  account_id BIGINT UNSIGNED NOT NULL,
  user_id    BIGINT UNSIGNED NOT NULL,       -- 客服人员（普通用户账号充当）
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (account_id, user_id)
);

CREATE TABLE official_followers (
  account_id  BIGINT UNSIGNED NOT NULL,
  user_id     BIGINT UNSIGNED NOT NULL,
  muted       TINYINT UNSIGNED NOT NULL DEFAULT 0,   -- 关闭账号消息提醒
  followed_at DATETIME(6) NOT NULL,
  unfollowed_at DATETIME(6) NULL,           -- 取关后重关注=新行（幂等由 active 唯一键保证）
  active_flag TINYINT UNSIGNED GENERATED ALWAYS AS (IF(unfollowed_at IS NULL, 1, NULL)) STORED,
  PRIMARY KEY (account_id, user_id, followed_at),
  UNIQUE KEY uk_ofollow_active (account_id, user_id, active_flag),
  KEY idx_ofollow_user (user_id)
);

CREATE TABLE official_articles (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id  BIGINT UNSIGNED NOT NULL,
  title       VARCHAR(128) NOT NULL,
  cover_media_id BIGINT UNSIGNED NULL,
  summary     VARCHAR(512) NOT NULL DEFAULT '',
  body        MEDIUMTEXT NOT NULL,           -- ≤20000 字符（应用层校验，列宽冗余）
  status      VARCHAR(16) NOT NULL DEFAULT 'draft',  -- draft|published|unpublished(下架)
  published_at DATETIME(6) NULL,
  unpublished_at DATETIME(6) NULL,
  created_at  DATETIME(6) NOT NULL,
  updated_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_oarticles_account (account_id, status, published_at)
);

CREATE TABLE official_article_reads (
  account_id BIGINT UNSIGNED NOT NULL,
  article_id BIGINT UNSIGNED NOT NULL,
  user_id    BIGINT UNSIGNED NOT NULL,
  first_read_at  DATETIME(6) NOT NULL,
  last_read_at   DATETIME(6) NOT NULL,
  PRIMARY KEY (account_id, article_id, user_id),
  KEY idx_oreads_user (user_id)
);

CREATE TABLE official_menus (
  account_id BIGINT UNSIGNED NOT NULL,
  level      TINYINT UNSIGNED NOT NULL,      -- 1|2
  parent_pos TINYINT UNSIGNED NULL,         -- 二级菜单指向一级 position
  position   TINYINT UNSIGNED NOT NULL,     -- 同级排序 1..N
  label      VARCHAR(32) NOT NULL,
  action     VARCHAR(16) NOT NULL,          -- open_article|start_service
  article_id BIGINT UNSIGNED NULL,          -- action=open_article 时必填
  PRIMARY KEY (account_id, level, position),
  KEY idx_omenu_parent (account_id, parent_pos, position)
);

CREATE TABLE official_notifications (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id  BIGINT UNSIGNED NOT NULL,
  user_id     BIGINT UNSIGNED NOT NULL,      -- 收件人
  kind        VARCHAR(16) NOT NULL,          -- article|welcome
  article_id  BIGINT UNSIGNED NULL,
  title       VARCHAR(128) NOT NULL DEFAULT '',
  created_at  DATETIME(6) NOT NULL,
  read_at     DATETIME(6) NULL,
  PRIMARY KEY (id),
  KEY idx_onotif_user (user_id, read_at, id),
  KEY idx_onotif_dedupe (account_id, user_id, article_id, created_at)
);
-- 3 条/天上限：SELECT COUNT WHERE account_id AND user_id AND kind='article' AND created_at>=今日0点UTC

CREATE TABLE service_sessions (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  official_account_id BIGINT UNSIGNED NOT NULL,
  user_id       BIGINT UNSIGNED NOT NULL,
  session_number INT UNSIGNED NOT NULL,      -- 同 (账号,用户) 递增，从 1 开始
  conversation_id BIGINT UNSIGNED NOT NULL,  -- type=official_service
  status        VARCHAR(16) NOT NULL DEFAULT 'active',  -- active|closed
  closed_by     BIGINT UNSIGNED NULL,        -- user|staff:user_id
  closed_at     DATETIME(6) NULL,
  created_at    DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_ssessions_key (official_account_id, user_id, session_number),
  KEY idx_ssessions_conv (conversation_id),
  KEY idx_ssessions_active (official_account_id, user_id, status)
);

CREATE TABLE service_session_staff (          -- 会话当前受理人（首版单客服，无转接）
  session_id BIGINT UNSIGNED NOT NULL,
  staff_id   BIGINT UNSIGNED NOT NULL,
  assigned_at DATETIME(6) NOT NULL,
  PRIMARY KEY (session_id)
);

CREATE TABLE service_session_events (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  session_id BIGINT UNSIGNED NOT NULL,
  event      VARCHAR(24) NOT NULL,   -- created|assigned|closed
  actor_id   BIGINT UNSIGNED NOT NULL,
  detail     JSON NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_ssevents_session (session_id, id)
);
```

## 3. 领域规则

### 3.1 内容账号（合同 §11.1）
- R1 创建/编辑：仅运营管理员（operator 域端口 `operator.IsOperator(userID)`；阶段六前用配置白名单 `WECHAT_OPERATOR_IDS` 实现该端口，**裁决**）。draft→active 由运营发布；frozen/closed 后普通用户接口全部 `NOT_FOUND`（不可见=不存在，防探测）。
- R2 用户查询：`GET /official-accounts?name=精确名`，仅 status=active 出现。无模糊搜索。

### 3.2 关注与图文（合同 §11.2）
- R3 关注：`POST /official-accounts/{id}/follow`。事务：INSERT official_followers（active_flag 唯一键兜底幂等；重关注走新行）→ **固定欢迎消息**：找/建 official_service 会话？**裁决：欢迎消息不进客服会话**（用户未发起客服），落 official_notifications(kind=welcome, title=固定文案"感谢关注…") → outbox 推送。取关：UPDATE unfollowed_at（幂等）。
- R4 文章（运营侧）：POST 创建 draft；PUT 编辑（draft 可全改，published 只能改?**裁决：published 不可编辑只能下架重发**）；POST publish（draft→published, published_at）；POST unpublish（published→unpublished）。
- R5 文章（用户侧）：列表 `GET /official-accounts/{id}/articles?cursor=`（published，published_at desc，20/页）；正文 `GET /articles/{id}`：status=published 且账号 active 否则 `CONTENT_UNAVAILABLE`；**下架文章从列表/正文/菜单/通知/客服消息全部不可读**（通知与客服消息里只保留标题摘要，正文链接失效返回 CONTENT_UNAVAILABLE——即读取入口统一走本判定）。阅读记录 UPSERT（first 不变，last 刷新）。
- R6 阅读记录仅首次+最近时间，无统计接口。

### 3.3 菜单（合同 §11.3）
- R7 结构：≤3 一级；每一级 ≤5 二级；一级有子菜单时自身不可带动作（**裁决：带子菜单的一级 action 置空**）。动作仅 open_article（指向 published 文章，保存时校验）或 start_service。
- R8 菜单读取（用户）：账号 active 才返回；点击 open_article 时若文章已下架 → `CONTENT_UNAVAILABLE`（读取时实时判，不缓存）。
- R9 菜单保存（运营）：整体覆盖式 PUT（事务删旧插新，全量校验后写入）。

### 3.4 客服（合同 §11.4）
- R10 发起：`POST /official-accounts/{id}/service-sessions`。前置：已关注（active follower）、账号 active。事务：session_number = 当前该 (账号,用户) 最大 +1（行锁：`SELECT ... FOR UPDATE` on official_accounts 或 MAX+1 唯一键兜底——**用唯一键 uk_ssessions_key + 撞键重读递增，同 conversation direct_key 惯用法**）→ INSERT session(active) + conversation.CreateOfficialServiceTx（conversation 包新增：type=official_service，无 direct_key，直接 INSERT）→ service_session_events(created) → 系统消息 `session.created`。
- R11 每用户每账号仅一个 active：发起前查 active 存在则幂等返回现有 session。
- R12 用户可发 text/image/file（消息链路 R6 校验调本模块 `CanSendServiceMessage`：session active）；客服（staff）仅 text；staff 必须 official_account_staff 成员且为该会话受理人（首版无转接：任一该账号 staff 首次回复即 INSERT service_session_staff 认领）。
- R13 结束：用户或该账号 staff `POST /service-sessions/{id}/close`。active→closed；旧会话只读（CanSend 拒绝 STATE_CONFLICT）；重咨询走 R10 新 session_number。客服消息 180 天保留（message 统一规则）。
- R14 客服列表（staff 侧）：`GET /staff/service-sessions?status=active&cursor=`（本账号全部，按 created_at desc）+ 详情（消息读取复用 message 历史接口，staff 通过 service_session_staff+account_staff 判权——message 读取谓词扩展：official_service 会话 staff 可读其账号会话，**在 message 模块留 StaffReader 端口，本模块实现**）。

### 3.5 通知（合同 §11.5）
- R15 文章发布 → 对全部 active 未 muted followers 生成 article 通知：**逐条 INSERT official_notifications + outbox 推送**。量大：**裁决：follower >1000 时分批（每批 500）由 Worker 异步（outbox 事件 official.article.notify → SPEC-10 消费展开）**；小量同步。
- R16 3 条/天/账号/用户：INSERT 前查当日（UTC 0 点起）该账号发给该用户的 article 通知数，≥3 则跳过（文章仍可主页查看）。判定在生成侧（同步或 Worker 内），单条 SELECT+INSERT，允许极小概率竞态超 1 条（**接受**，业务非资金敏感）。
- R17 幂等：同一文章不重复通知——uk 式查重 `SELECT 1 FROM official_notifications WHERE account_id AND article_id AND kind='article' AND created_at>=发布当日`？**裁决：通知表加生成列去重无必要；文章发布是一次性动作（draft→published 条件更新），重复发布请求幂等返回，通知生成挂在发布事务后的一次性 outbox 事件上，outbox 至少一次投递 + 消费端按 article_id 幂等（查已存在该 article 通知计数>0 即跳过整批）**。
- R18 muted=关闭提醒：不生成通知行？**裁决：muted 用户不生成通知（收件箱无行），文章列表仍可见**；取关删除未读通知？**裁决：保留历史通知行，仅停止新增**。
- R19 通知读取：`GET /notifications?cursor=`（用户全部账号通知+朋友圈通知聚合？**朋友圈通知在 moment 域自身接口**，本接口仅 official_notifications）+ 标记已读。

## 4. API 契约

| # | 方法 路径 | 调用者 | 说明 |
|---|---|---|---|
| F1 | POST /official-accounts | 运营 | 建 draft |
| F2 | PATCH /official-accounts/{id} | 运营 | 编辑基础字段/状态 |
| F3 | GET /official-accounts?name= | 用户 | 精确查询（active） |
| F4 | GET /official-accounts/{id} | 用户 | 资料+菜单+是否关注 |
| F5 | POST/DELETE /official-accounts/{id}/follow | 用户 | 关注/取关（幂等） |
| F6 | POST /official-accounts/{id}/articles | 运营 | 建 draft |
| F7 | PUT /official-accounts/{id}/articles/{aid} | 运营 | 编辑 draft |
| F8 | POST /official-accounts/{id}/articles/{aid}/publish | 运营 | 发布+通知 |
| F9 | POST /official-accounts/{id}/articles/{aid}/unpublish | 运营 | 下架 |
| F10 | GET /official-accounts/{id}/articles?cursor= | 用户 | 列表 |
| F11 | GET /articles/{id} | 用户 | 正文+阅读记录 |
| F12 | PUT /official-accounts/{id}/menu | 运营 | 覆盖式保存 |
| F13 | POST /official-accounts/{id}/service-sessions | 用户 | 发起（幂等） |
| F14 | POST /service-sessions/{id}/close | 用户/staff | 结束 |
| F15 | GET /staff/service-sessions?status=&cursor= | staff | 会话列表 |
| F16 | GET /staff/service-sessions/{id}/messages?cursor= | staff | 历史读取 |
| F17 | POST /staff/service-sessions/{id}/messages | staff | 回复（仅 text） |
| F18 | GET /notifications?cursor= | 用户 | 通知列表 |
| F19 | POST /notifications/read | 用户 | `{ids?|all}` |
| F20 | PUT /official-accounts/{id}/mute | 用户 | 关闭账号提醒 |

用户在客服会话发消息走通用 `POST /conversations/{id}/messages`（message 模块，R6 权限回调本模块）。

## 5. 跨模块端口

```go
// message 侧需要（本模块实现）：
type ServiceSessionAuth interface {
    CanSendServiceMessage(ctx, conversationID, senderID int64, senderType string) error
    StaffCanRead(ctx, conversationID, staffID int64) (bool, error)
}
// conversation 包扩展（任务 1）：CreateOfficialServiceTx(ctx, tx, accountID, userID) (convID, error)
// operator 端口：IsOperator(ctx, userID) bool —— 阶段六前读配置 WECHAT_OPERATOR_IDS（逗号分隔）
```

## 6. 幂等与并发

- I1：关注/取关幂等（active_flag 唯一键 / 条件 UPDATE）。
- I2：session_number 唯一键 + 撞键重试递增。
- I3：文章发布条件 UPDATE（draft→published），重复发布幂等。
- I4：通知生成挂一次性 outbox 事件，消费端按 article 幂等（整批查重）。
- I5：菜单覆盖式 PUT 全量事务。

## 7. 测试用例

| ID | 用例 | 断言 |
|---|---|---|
| T1 | 冻结/注销账号查询 | NOT_FOUND（不可见） |
| T2 | 关注→欢迎通知；重复关注 | 一条通知；幂等 |
| T3 | 未关注发起客服 | FORBIDDEN |
| T4 | 每账号每用户单 active 会话 | 幂等返回 |
| T5 | 关闭后发消息 | STATE_CONFLICT；重新咨询 session_number+1 |
| T6 | 客服发图片 | INVALID_ARGUMENT（仅 text） |
| T7 | 非本账号 staff 读取/回复 | FORBIDDEN |
| T8 | 菜单 4 一级/二级 6 个 | INVALID_ARGUMENT |
| T9 | 菜单指向下架文章 | CONTENT_UNAVAILABLE |
| T10 | 菜单指向 draft 文章（保存时） | INVALID_ARGUMENT |
| T11 | 文章下架后正文/列表/通知链接 | 全部 CONTENT_UNAVAILABLE |
| T12 | 阅读记录首次/最近 | UPSERT 正确 |
| T13 | 4 条文章通知/天 | 第 4 条不生成 |
| T14 | muted 用户 | 无通知行，文章可读 |
| T15 | 重复发布同文章 | 不重复通知 |
| T16 | published 编辑 | 拒绝 |

## 8. 任务分解

1. conversation.CreateOfficialServiceTx + 单测。
2. operator.IsOperator 配置端口（platform/config 加 WECHAT_OPERATOR_IDS）。
3. 迁移 00007_content_account.sql。
4. `internal/content/`（包名 content）：store→service（R1-R9 账号/关注/文章/菜单）→handler（F1-F12）+ 单测 T1-T2、T8-T12、T16。
5. 客服子域：service_session（R10-R14）+ F13-F17 + 单测 T3-T7。
6. 通知子域（R15-R19）+ F18-F20 + 单测 T13-T15。
7. Worker 接入点：`FanoutArticleNotifications`（SPEC-10 消费 official.article.notify）。
8. 边界脚本 DOMAINS 加入 content。

## 9. 验收

内容账号与普通用户体系隔离；关注幂等+欢迎通知；文章四状态流转+下架全链路不可读；菜单 3×5+两类动作；客服单 active 会话+类型限制+staff 权限；通知 3 条/天上限+幂等+muted 语义。
