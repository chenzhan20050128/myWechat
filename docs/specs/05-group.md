# SPEC-05 group（群聊、群待办）

> 依据：合同 §6、§13.3、§14.3；ADR-001（跨模块仅走 service 接口）、ADR-006（事务纪律）。
> 读者：低等级 LLM 执行者。**全部技术判断已写死，不得自行变更；未覆盖情况 → 停下记录 PROGRESS.md。**
> **实现顺序注意：本模块必须先于 internal/message 的 service 完成编码**（message 依赖本模块的 CanSend/IsModerator），但迁移编号与编码顺序无关。

## 1. 范围

**做**：建群/资料/公告、成员管理（邀请/移除/退群/转让/解散）、角色（群主/管理员/成员）、入群二维码（24h/50 次）、禁言（10m/1h/1d/永久）、群事件系统消息、群待办（创建/指派/完成/逾期/取消/waived）。
**不做**：群相册、群公告以外的富文本、普通成员邀请、永久二维码（合同明确排除）。

## 2. 模块间契约（先定义，避免循环依赖）

Go 包不能循环 import。两个方向的依赖都用**消费方定义接口**（Go 惯用法）：

```go
// internal/group/service.go 中定义（message 模块实现注入）：
type SystemMessenger interface {
    // SendSystemTx 在事务内写一条 type=system 消息到群会话。
    // group 侧只提供 payload map；message 侧负责 seq 分配与落库（SPEC-04 R2 流程）。
    SendSystemTx(ctx, tx mysqlx.Tx, conversationID int64, event string, detail map[string]any) (messageID int64, err error)
}

// internal/message/service.go 中定义（group 模块实现注入）：
type GroupAuth interface {
    CanSend(ctx, groupID, userID int64) error          // 返回 *AppError 或 nil
    IsModerator(ctx, groupID, userID int64) (bool, error)
    IsMember(ctx, groupID, userID int64) (bool, error)
    MembershipWindows(ctx, groupID, userID int64) ([]Window, error) // R15 可读性
}
```
cmd/api 组装时互相注入。**禁止** group 包 import message 包（或反向）。

对 conversation 模块的扩展（阶段一 conversation 包新增一个函数，属本规格任务 1）：
```go
// conversation.CreateGroupTx(ctx, tx, creatorID) (convID int64, err error)
// 创建 type='group' 会话并加 creator 为成员（epoch 1）。直接 INSERT，无唯一键去重需求。
```
groups 表保存 conversation_id。

## 3. 数据模型（迁移 `migrations/00003_group.sql`，编号与实现顺序无关）

```sql
CREATE TABLE groups (
  id                    BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  name                  VARCHAR(64) NOT NULL,
  owner_id              BIGINT UNSIGNED NOT NULL,
  avatar_media_id       BIGINT UNSIGNED NULL,
  announcement          VARCHAR(2000) NOT NULL DEFAULT '',
  announcement_updated_by BIGINT UNSIGNED NULL,
  announcement_updated_at DATETIME(6) NULL,
  member_count          INT UNSIGNED NOT NULL DEFAULT 1,   -- 事务内维护，当前成员数
  status                VARCHAR(16) NOT NULL DEFAULT 'active',  -- active|dissolved
  conversation_id       BIGINT UNSIGNED NOT NULL,
  created_at            DATETIME(6) NOT NULL,
  dissolved_at          DATETIME(6) NULL,
  PRIMARY KEY (id),
  KEY idx_groups_owner (owner_id),
  KEY idx_groups_conv (conversation_id)
);

CREATE TABLE group_members (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  group_id    BIGINT UNSIGNED NOT NULL,
  user_id     BIGINT UNSIGNED NOT NULL,
  role        VARCHAR(8) NOT NULL DEFAULT 'member',  -- owner|admin|member
  joined_at   DATETIME(6) NOT NULL,
  left_at     DATETIME(6) NULL,
  left_reason VARCHAR(8) NOT NULL DEFAULT '',        -- quit|removed|dissolved
  active_flag TINYINT UNSIGNED GENERATED ALWAYS AS (IF(left_at IS NULL, 1, NULL)) STORED,
  PRIMARY KEY (id),
  UNIQUE KEY uk_gmembers_active (group_id, user_id, active_flag),
  KEY idx_gmembers_user (user_id, left_at),
  KEY idx_gmembers_group_left (group_id, left_at, joined_at)
);

CREATE TABLE group_invite_codes (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  group_id    BIGINT UNSIGNED NOT NULL,
  code        VARCHAR(12) COLLATE utf8mb4_bin NOT NULL,
  created_by  BIGINT UNSIGNED NOT NULL,
  max_uses    INT UNSIGNED NOT NULL DEFAULT 50,      -- 固定 50，不用参数覆盖
  use_count   INT UNSIGNED NOT NULL DEFAULT 0,
  expires_at  DATETIME(6) NOT NULL,                  -- created_at + 24h
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_ginvite_code (code),
  KEY idx_ginvite_group (group_id)
);

CREATE TABLE group_invite_uses (                    -- 二维码使用记录（24h 防重回判定依据）
  code_id   BIGINT UNSIGNED NOT NULL,
  user_id   BIGINT UNSIGNED NOT NULL,
  joined_at DATETIME(6) NOT NULL,                   -- 仅记录成功入群
  PRIMARY KEY (code_id, user_id)
);

CREATE TABLE group_mutes (
  group_id   BIGINT UNSIGNED NOT NULL,
  user_id    BIGINT UNSIGNED NOT NULL,
  until_at   DATETIME(6) NULL,                      -- NULL = 永久
  created_by BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (group_id, user_id)
);

CREATE TABLE group_events (                          -- 群操作事件流水（审计+排障，非业务读路径）
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  group_id   BIGINT UNSIGNED NOT NULL,
  event      VARCHAR(32) NOT NULL,   -- member.joined|member.quit|member.removed|role.changed|muted|unmuted|announcement.changed|owner.transferred|dissolved|invite_code.created
  actor_id   BIGINT UNSIGNED NOT NULL,
  target_id  BIGINT UNSIGNED NULL,
  detail     JSON NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_gevents_group (group_id, id)
);

CREATE TABLE group_todos (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  group_id    BIGINT UNSIGNED NOT NULL,
  title       VARCHAR(200) NOT NULL,
  description VARCHAR(2000) NOT NULL DEFAULT '',
  due_at      DATETIME(6) NULL,
  status      VARCHAR(16) NOT NULL DEFAULT 'active',  -- active|completed|overdue|cancelled
  created_by  BIGINT UNSIGNED NOT NULL,
  created_at  DATETIME(6) NOT NULL,
  updated_at  DATETIME(6) NOT NULL,
  completed_at DATETIME(6) NULL,
  cancelled_at DATETIME(6) NULL,
  deleted_at  DATETIME(6) NULL,                      -- 只从列表隐藏
  PRIMARY KEY (id),
  KEY idx_gtodos_group (group_id, status, deleted_at)
);

CREATE TABLE group_todo_assignees (
  todo_id     BIGINT UNSIGNED NOT NULL,
  user_id     BIGINT UNSIGNED NOT NULL,
  status      VARCHAR(16) NOT NULL DEFAULT 'assigned', -- assigned|completed|waived|cancelled
  completed_at DATETIME(6) NULL,
  PRIMARY KEY (todo_id, user_id),
  KEY idx_gtassign_user (user_id, status)
);

CREATE TABLE group_todo_member_snapshots (          -- 创建时成员快照（查看权判定）
  todo_id   BIGINT UNSIGNED NOT NULL,
  user_id   BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (todo_id, user_id),
  KEY idx_gtsnap_user (user_id)
);

CREATE TABLE group_todo_events (                    -- 待办操作流水（审计）
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  todo_id    BIGINT UNSIGNED NOT NULL,
  event      VARCHAR(24) NOT NULL,  -- created|modified|assigned|completed|waived|overdue|cancelled
  actor_id   BIGINT UNSIGNED NOT NULL,
  detail     JSON NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_gtevents_todo (todo_id, id)
);
```

## 4. 领域规则

### 4.1 群基础（合同 §6.1）
- R1 建群：`POST /groups {name, avatar_media_id?}`。事务：conversation.CreateGroupTx → INSERT groups（member_count=1, owner）→ INSERT group_members(owner, role=owner) → 系统消息 `group.created` → audit。返回 `{group_id}`。
- R2 人数上限 500（含群主）：每次入群（邀请/扫码）事务内 `SELECT member_count FROM groups WHERE id=? FOR UPDATE` 后校验 `member_count + 新增数 <= 500`，超限 `QUOTA_EXCEEDED`。member_count 与 group_members 行在同一事务内增减，行锁串行化保证不超卖。
- R3 解散：仅群主。事务：`UPDATE groups SET status='dissolved', dissolved_at=?`；所有 active 成员 left_at=now, left_reason='dissolved'（一条 UPDATE）；系统消息 `group.dissolved`；audit。解散后：一切写操作（入群/发消息/待办变更）拒绝 `STATE_CONFLICT`；历史只读。
- R4 转让：仅群主，目标必须是当前 active 成员。事务：旧主 role=member，新主 role=owner，groups.owner_id 更新；系统消息 `owner.transferred`。转让后原群主为普通成员。

### 4.2 权限矩阵（合同 §6.2）
- R5 修改群名/头像/公告、邀请、移除普通成员、个人禁言、群消息置顶、待办管理：owner 或 admin。任命/移除管理员、转让、解散：仅 owner。
- R6 管理员上限 10：任命第 11 个 → `QUOTA_EXCEEDED`。任命目标必须是普通成员（不能任命管理员/群主）。
- R7 移除成员：owner/admin 只能移除 **role=member** 的成员；admin 不能移除 admin；owner 想移除 admin 必须先撤其管理员。不能移除自己。
- R8 权限判定统一入口：`requireModerator(ctx, groupID, userID)` 与 `requireOwner(...)`，群不存在 → `NOT_FOUND`，群 dissolved 且操作为写 → `STATE_CONFLICT`，非成员 → `FORBIDDEN`。

### 4.3 入群与二维码（合同 §6.3）
- R9 直接邀请：owner/admin，可批量（≤50/请求）。每个被邀者必须是邀请者的**有效好友**（`friendships` active，调 contact 模块接口 `contact.IsFriend(inviter, invitee)`）；目标已是 active 成员则跳过（幂等，不报错）；人数校验按 R2。事务内：INSERT group_members(epoch 新行) + member_count+n + **一条聚合系统消息** `member.joined`（detail 含 inviter 与 members 列表；**不逐人一条**——50 人批量若逐条会占 50 个 seq 与锁持时间）+ group_events（逐人一行，审计粒度）。
- R10 扫码入群：`POST /groups/join {code}`。校验顺序（全在单事务内）：
  1. code 存在、未过期（expires_at > now）、use_count < max_uses → 否则 `RESOURCE_UNAVAILABLE`；
  2. 群 active；用户非当前成员（是则幂等返回成功）；
  3. **同二维码 24h 防重回**：`SELECT 1 FROM group_members WHERE group_id=? AND user_id=? AND left_reason='removed' AND left_at > now - 24h` 命中 → `STATE_CONFLICT`；（防重回限定"同一二维码"——精确语义：只要该用户因被移除离开本群未满 24h，即拒绝扫码，无论用哪个码。**裁决：按"离开本群未满24h"实现**，更严格且实现无歧义，group_invite_uses 表仅作使用计数审计。）
  4. 人数校验 R2；INSERT member；use_count+1；INSERT group_invite_uses；系统消息 `member.joined`。
- R11 二维码创建：owner/admin。`POST /groups/{id}/invite-codes`：code = 10 字符 Crockford Base32 随机（platform/ids 新增 `NewInviteCode()`，字符集 `0123456789ABCDEFGHJKMNPQRSTVWXYZ`，crypto/rand，冲突重试 3 次）；expires_at = now+24h；max_uses 固定 50。返回 `{code, expires_at}`。不提供永久码。
- R12 成员记录：joined_at/left_at 永久保留（多行多区间）。退群 `POST /groups/{id}/quit`：群主不能退群（`STATE_CONFLICT`，只能转让或解散）；left_reason='quit'。移除：left_reason='removed'。
- R13 系统消息可见性：由消息表的自然成员区间过滤实现（SPEC-04 R15），本模块不做额外可见性逻辑。事件发生时在场的成员 = 事件系统消息的可见者，与区间语义一致。

### 4.4 禁言（合同 §6.4）
- R14 设置：owner/admin，对单个成员（不能禁言 owner；不能禁言 admin？**裁决：admin 可以被 owner 禁言，admin 不能禁言 admin**）。duration 枚举：`10m|1h|1d|forever` → until_at = now + 对应时长 / NULL。UPSERT `group_mutes`（覆盖旧禁言）。
- R15 效果：被禁言者不能发消息、不能建群待办；可收可读。owner/admin **不受**普通禁言影响（CanSend 对 owner/admin 直接放行）。
- R16 到期：`CanSend` 判定 `until_at IS NULL OR until_at > now`（DB 时间字段是事实源，惰性判定即正确）。Worker（SPEC-10）每日清理 `until_at <= now` 的行（物理清理，非语义必需）。
- R17 解禁：`DELETE /groups/{id}/mutes/{user_id}` owner/admin。
- R18 禁言/解禁生成系统消息 `member.muted`/`member.unmuted`。

### 4.5 群待办（合同 §6.5）
- R19 创建：owner/admin。校验：title 1..200 字符；description ≤2000；assignee_ids 1..20 且全部是**当前 active 成员**（去重）；due_at > now（可选）。事务：INSERT group_todos → INSERT assignees(assigned) → INSERT member_snapshots（当前全部 active 成员，一条 `INSERT ... SELECT`）→ group_todo_events(created) → 系统消息 `todo.created`。
- R20 修改：owner/admin。可改 title/description/due_at/追加指派（新指派也必须是当前成员且总数 ≤20；追加的成员**必须已在 member_snapshots 中**——若不在，自动补入快照）。已 completed/cancelled 的待办不可改（`STATE_CONFLICT`）。
- R21 取消：owner/admin。`status='cancelled'`，未终态 assignees → 'cancelled'。取消后不能再完成。删除（列表隐藏）：`deleted_at=now`，仅 owner/admin，审计保留（group_todo_events 不删）。
- R22 完成：被指派者本人，`POST /groups/{id}/todos/{tid}/complete`。assignee 状态 assigned → completed（幂等：已 completed 返回成功）。todo 已 cancelled → `STATE_CONFLICT`。群 dissolved → `STATE_CONFLICT`。全部 assignees 为 completed 时 todo.status='completed', completed_at=now；**裁决：若全部 assignees 都变为 waived（无人可完成），todo 自动置 'cancelled'**。
- R23 waived：成员退群/被移除时，**同一事务内**：`UPDATE group_todo_assignees a JOIN group_todos t ON ... SET a.status='waived' WHERE t.group_id=? AND a.user_id=? AND a.status='assigned'`；随后对每个受影响 todo 重算整体状态（R22 规则）。waived 不可逆（重新入群不恢复）。
- R24 逾期：Worker 扫描 `status='active' AND due_at IS NOT NULL AND due_at <= now AND EXISTS(assigned assignee)` → status='overdue' + 系统消息。逾期仍可完成（R22 无 overdue 拦截）。
- R25 查看：owner/admin 可见群内全部未删除待办；普通成员 = 当前 active 成员 **且** 在 member_snapshots 中（新成员看不到加入前创建的待办；退群后不可见）。列表过滤 + 详情校验同一谓词。
- R26 幂等：重复创建请求带 `client_request_id`（UUID，`(group_id, client_request_id)` 由调用方查 group_todo_events.detail 判重，**简化裁决：不引入新唯一键，重复创建生成两条待办是可接受的**——创建幂等不在合同强制清单内；完成/取消幂等必须实现（R22/R21 条件更新天然幂等））。

## 5. API 契约（`/api/v1`，全部需鉴权）

| # | 方法 路径 | 请求体 | 成功响应 data | 主要错误 |
|---|---|---|---|---|
| B1 | POST /groups | `{name, avatar_media_id?}` | `{group_id}` | INVALID_ARGUMENT |
| B2 | PATCH /groups/{id} | `{name?}` | — | FORBIDDEN |
| B3 | PUT /groups/{id}/avatar | `{media_object_id}` | — | INVALID_ARGUMENT |
| B4 | PUT /groups/{id}/announcement | `{content}` | — | FORBIDDEN, INVALID_ARGUMENT |
| B5 | GET /groups/{id} | — | 群详情（含我的角色） | NOT_FOUND |
| B6 | GET /groups/{id}/members?cursor= | — | 成员列表（user_id/昵称/角色/禁言状态） | FORBIDDEN(非成员) |
| B7 | POST /groups/{id}/members | `{user_ids:[...]}` | `{added:[...], skipped:[...]}` | QUOTA_EXCEEDED, FORBIDDEN |
| B8 | POST /groups/join | `{code}` | `{group_id}` | RESOURCE_UNAVAILABLE, STATE_CONFLICT, QUOTA_EXCEEDED |
| B9 | DELETE /groups/{id}/members/{uid} | — | — | FORBIDDEN, STATE_CONFLICT |
| B10 | POST /groups/{id}/quit | — | — | STATE_CONFLICT(群主) |
| B11 | POST /groups/{id}/transfer | `{user_id}` | — | STATE_CONFLICT |
| B12 | POST /groups/{id}/dissolve | — | — | FORBIDDEN |
| B13 | POST /groups/{id}/admins | `{user_id}` | — | QUOTA_EXCEEDED, STATE_CONFLICT |
| B14 | DELETE /groups/{id}/admins/{uid} | — | — | STATE_CONFLICT |
| B15 | POST /groups/{id}/mutes | `{user_id, duration}` | — | INVALID_ARGUMENT, FORBIDDEN |
| B16 | DELETE /groups/{id}/mutes/{uid} | — | — | — |
| B17 | POST /groups/{id}/invite-codes | — | `{code, expires_at}` | FORBIDDEN |
| B18 | POST /groups/{id}/todos | `{title, description?, assignee_ids, due_at?, client_request_id?}` | `{todo_id}` | INVALID_ARGUMENT, QUOTA_EXCEEDED |
| B19 | PATCH /groups/{id}/todos/{tid} | `{title?, description?, due_at?, append_assignee_ids?}` | — | STATE_CONFLICT |
| B20 | POST /groups/{id}/todos/{tid}/complete | — | — | STATE_CONFLICT |
| B21 | DELETE /groups/{id}/todos/{tid} | — | — | FORBIDDEN |
| B22 | GET /groups/{id}/todos?status=&cursor= | — | 待办列表（含我的指派状态） | FORBIDDEN |
| B23 | GET /groups/{id}/todos/{tid} | — | 待办详情+指派明细 | NOT_FOUND, FORBIDDEN |

`GET /users/me/groups?cursor=`：我的群列表（B 包外补充，走 group 模块）。

## 6. 事务与锁序（全部写操作统一）

```
BEGIN
  1. SELECT * FROM groups WHERE id=? FOR UPDATE          -- 锁群，串行化人数/角色变更
  2.（如涉及成员变更）校验 group_members 区间
  3. 变更行 + member_count 维护
  4. SystemMessenger.SendSystemTx（其内部再锁 conversations 行 —— 锁序：groups → conversations → messages，全局一致）
  5. group_events / group_todo_events / audit.LogTx
COMMIT
```
死锁重试用 `mysqlx.WithinTx`（既有机制，重试 3 次）。**锁序文档同步更新到 ARCHITECTURE.md ADR-006。**

## 7. 幂等与并发

- I1：活跃成员唯一键（active_flag 生成列）兜底并发入群；INSERT IGNORE + 影响行数判断。
- I2：member_count 与成员行同事务，groups 行锁保证 ≤500 不超卖（测试 T5 并发验证）。
- I3：管理员 ≤10 同理（groups 行锁内 COUNT）。
- I4：邀请码 use_count 原子 `UPDATE ... SET use_count=use_count+1 WHERE id=? AND use_count<max_uses`，rows=0 → RESOURCE_UNAVAILABLE。
- I5：完成/取消/waived 均为条件 UPDATE，天然幂等。
- I6：转让群主=两行 UPDATE + owner_id 更新，同事务原子。

## 8. 测试用例（全部落成 Go 单测，接口 stub 注入）

| ID | 用例 | 断言 |
|---|---|---|
| T1 | 建群 | groups+members+conversation+系统消息四表齐写 |
| T2 | 501 人入群 | QUOTA_EXCEEDED，member_count=500 |
| T3 | 并发 20 个入群到 490 人群 | 最终 ≤500，无超卖 |
| T4 | 非好友邀请 | 拒绝该被邀者，其余成功 |
| T5 | 扫码：过期码/超 50 次/有效 | 精确三种结果 |
| T6 | 被移除 23h/25h 后扫码 | 拒绝 / 允许 |
| T7 | admin 任命第 11 个 | QUOTA_EXCEEDED |
| T8 | admin 移除 admin / owner 移除 admin | 均拒绝；先撤管理员后可移 |
| T9 | 群主退群 | STATE_CONFLICT |
| T10 | 禁言 owner 不生效；禁言 admin 后该 admin 发消息 | 放行 / FORBIDDEN |
| T11 | 禁言到期（FakeClock 前拨） | CanSend 放行 |
| T12 | 解散后入群/发消息/改待办 | 全部 STATE_CONFLICT |
| T13 | 解散后读历史 | 消息模块 R15 放行（区间命中） |
| T14 | 待办：全部完成 | status=completed |
| T15 | 待办：到期未完成 | Worker 置 overdue；仍可完成 |
| T16 | 被指派人退群 | assignee=waived；全 waived → todo cancelled |
| T17 | 重新入群后 waived 不恢复、旧待办不可见 | R23/R25 |
| T18 | 新成员看不到加入前待办 | R25 |
| T19 | 取消后完成 | STATE_CONFLICT |
| T20 | 重复完成请求 | 幂等成功 |
| T21 | 普通成员建待办 | FORBIDDEN |
| T22 | 转让后旧主权限 | 变普通成员 |

## 9. 任务分解（顺序执行，每步 build+vet+边界检查通过）

1. `platform/ids` 新增 `NewInviteCode()`。
2. `conversation.CreateGroupTx`（+ 单测）。
3. 迁移 00003_group.sql + cmd/migrate 验证 goose up/down 干净往返。
4. `internal/group/store.go` → `service.go`（R1-R17 权限/成员/禁言部分）+ `dto.go` + `handler.go`（B1-B17）。
5. group 单测 T1-T13。
6. 待办子域：service 扩展（R19-R26）+ handler（B18-B23）+ 单测 T14-T22。
7. SystemMessenger 接口就位（message 侧实现属 SPEC-04 任务；在 message 完成前，单测用 stub）。
8. Worker 任务接入点预留（逾期扫描/禁言清理函数 `ScanOverdueTodos`/`CleanupExpiredMutes`，由 SPEC-10 调度）。
9. 边界脚本 DOMAINS 加入 group。

## 10. 验收（合同 §18 对应）

群 500 上限精确；二维码 24h/50 次/防重回精确；权限矩阵全项正确；解散后只读；禁言四档与豁免正确；待办六规则（快照可见/waived/逾期可完成/全完成/取消/幂等）全部可测。
