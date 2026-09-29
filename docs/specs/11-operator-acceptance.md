# SPEC-11 operator（运营：审计查询、举报处置、报表）与验收测试

> 依据：合同 §2.1（运营管理员角色）、§16.2、§16.3、§18 全部小节。
> 读者：低等级 LLM 执行者。**全部技术判断已写死；未覆盖情况 → 停下记录 PROGRESS.md。**

## 1. 范围

**做**：运营身份与鉴权、审计日志查询接口、举报闭环（用户提交/运营处置/内容隐藏）、账号冻结/解冻、报表接口（消息量/用户增长/队列积压等运营读数）、死信查看与重放（SPEC-10 R18）。
**不做**：自动内容审核、推荐降权、BI 平台。

## 2. 数据模型（迁移 `migrations/00008_operator.sql`）

```sql
CREATE TABLE operator_admins (
  user_id    BIGINT UNSIGNED NOT NULL,      -- 运营管理员的普通账号（复用 users，双角色）
  added_by   BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (user_id)
);

CREATE TABLE reports (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  reporter_id BIGINT UNSIGNED NOT NULL,
  target_type VARCHAR(16) NOT NULL,   -- user|message|moment|group_todo|article|service_message
  target_id   BIGINT UNSIGNED NOT NULL,
  reason      VARCHAR(32) NOT NULL,   -- 枚举：spam|abuse|porn|fraud|other
  note        VARCHAR(1000) NOT NULL DEFAULT '',
  status      VARCHAR(16) NOT NULL DEFAULT 'pending',  -- pending|confirmed|rejected
  handled_by  BIGINT UNSIGNED NULL,
  handled_at  DATETIME(6) NULL,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_reports_dedupe (reporter_id, target_type, target_id),  -- 幂等合并
  KEY idx_reports_status (status, created_at)
);
-- 幂等合并语义：同一用户重复举报同一对象撞唯一键 → 返回已有 report_id，原行不更新

CREATE TABLE moderation_actions (       -- 处置流水（审计冗余存档 + 查询）
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  operator_id BIGINT UNSIGNED NOT NULL,
  action      VARCHAR(24) NOT NULL,     -- freeze_account|unfreeze_account|hide_content|unhide_content|confirm_report|reject_report
  target_type VARCHAR(16) NOT NULL,
  target_id   BIGINT UNSIGNED NOT NULL,
  note        VARCHAR(500) NOT NULL DEFAULT '',
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_modact_target (target_type, target_id)
);
```
内容隐藏：不建新表——各域用现有软删/状态列：
- user 冻结：`users.status='frozen'`（0001 已有该列；登录拒绝、令牌吊销）；
- message/moment/article 隐藏：既有 `status='deleted'/'unpublished'`；**解除处置=内容行不可"复活"**（裁决：合同"隐藏立即阻止读取，不删除数据库事实"——用软删状态；解除处置对用户内容支持恢复仅当各域有可恢复语义：article 可重新 publish；moment/message 的 deleted 不可逆 → **裁决：moment/message 的隐藏采用新状态值 `moderated`**（读取谓词与 deleted 同等拒绝，但可由 unhide 回 visible）。**无需 DDL 变更**：status 列是 VARCHAR(16)，不是 ENUM——新增取值只需各域读取谓词把 `moderated` 纳入拒绝集 + SPEC-06 §3 DDL 注释已标注。group_todo/service_message 隐藏 → 同理复用 `moderated` 或既有软删。

## 3. 领域规则

### 3.1 运营身份（合同 §2.1）
- R1 鉴权中间件 `RequireOperator`：user ∈ operator_admins 表 ∪ 配置 `WECHAT_OPERATOR_IDS`（启动注入，便于初始化）。运营接口前缀 `/api/v1/admin/*`。
- R2 admin 管理：仅现有 operator 可增删（首任由配置文件引导）。

### 3.2 审计查询（合同 §16.2）
- R3 查询接口 `GET /admin/audit-logs?actor_id=&event_type=&since=&until=&cursor=`（keyset id desc，50/页）。审计只读+仅运营；普通用户无任何审计接口（既有各域写入点已覆盖合同 §16.2 清单，**任务 3 需逐项核对既有 audit.LogTx 调用并补漏**：好友同意/删除/拉黑、群全部操作、待办全操作、动态/定时、撤回/收藏/上传/下载/清理、备份全操作、内容账号操作、举报处置——以 checklist 形式核对）。
- R4 审计不可修改：无 UPDATE/DELETE 代码路径（表也无此方法——store 只 insert+select）。

### 3.3 举报与处置（合同 §16.3）
- R5 用户提交：`POST /reports {target_type, target_id, reason, note}`。枚举校验；target 存在性轻校验（消息/动态等读不到也允许举报——**裁决：不校验可见性**，避免举报接口成为存在性探测器）。幂等合并（唯一键撞→返回原 id）。
- R6 运营列表：`GET /admin/reports?status=&cursor=`；详情：举报+被举报对象摘要（各域只读端口）。
- R7 处置动作（全部写 moderation_actions + audit）：
  - `POST /admin/reports/{id}/resolve {decision: confirmed|rejected}` → report.status；
  - `POST /admin/moderation/hide {target_type, target_id}` → 各域 HideContent 端口（写 `moderated`）；unhide 回原可见态（仅 moderated→visible）；
  - `POST /admin/moderation/freeze-user {user_id}` → users.status=frozen + 吊销全部会话（调 device.RevokeAll，sessionID=0 惯用法）+ audit；unfreeze → active（不自动恢复会话，需重新登录）。
- R8 隐藏立即生效：读取谓词同步（任务：message/moment/article 读取处 `status != visible` 已天然拒绝 `moderated`——核对即可， moderated 与 deleted 同在拒绝集）。**不删除数据库事实**。

### 3.4 报表（运营读数）
- R9 `GET /admin/stats/overview?date=`：DAU（audit 近 24h 登录成功去重）、新注册数、消息量（messages 当日 COUNT）、发送失败率（audit 拒绝/总量近似）。
- R10 `GET /admin/stats/queues`：outbox pending 数与最老年龄、retry pending、dead letters 计数（直查表，与 SPEC-10 指标同源）。
- R11 `GET /admin/stats/storage`：media_objects 总字节、GC 队列深度、备份总数。
- R12 全部报表只读、实时查询（无预聚合表，数据量级 V1 可接受；SQL 必须 LIMIT/时间窗约束）。

### 3.5 死信（承接 SPEC-10）
- R13 `GET /admin/dead-letters?cursor=`、`POST /admin/dead-letters/{id}/replay`（重放前须能查看失败原因——列表含 last_error）。

## 4. 跨模块端口（operator 定义，各域实现）
```go
type ContentModeration interface {
    HideMessage(ctx, messageID, operatorID int64) error      // status→moderated
    UnhideMessage(ctx, messageID int64) error
    HideMoment(ctx, momentID, operatorID int64) error
    UnhideMoment(ctx, momentID int64) error
    // article 用既有 publish/unpublish；user 冻结调 user+device 既有服务
}
```

## 5. API 契约（`/api/v1`）

| # | 方法 路径 | 调用者 |
|---|---|---|
| G1 | POST /reports | 用户 |
| G2 | GET /admin/audit-logs?... | 运营 |
| G3 | GET/POST /admin/reports[...] | 运营 |
| G4 | POST /admin/reports/{id}/resolve | 运营 |
| G5 | POST /admin/moderation/hide / unhide | 运营 |
| G6 | POST /admin/moderation/freeze-user / unfreeze-user | 运营 |
| G7 | GET /admin/stats/overview / queues / storage | 运营 |
| G8 | GET /admin/dead-letters / POST .../replay | 运营 |
| G9 | POST/DELETE /admin/operators | 运营 |

## 6. 测试用例

| ID | 用例 | 断言 |
|---|---|---|
| T1 | 非运营访问 /admin | FORBIDDEN |
| T2 | 重复举报 | 返回原 report_id |
| T3 | 隐藏动态后用户读取 | 拒绝；DB 行保留 |
| T4 | unhide | 恢复 visible 可读 |
| T5 | 冻结用户 | 登录拒绝+全部会话失效 |
| T6 | 审计查询过滤 | actor/type/时间过滤正确 |
| T7 | §16.2 审计清单核对 | checklist 逐项有写入点（静态核对测试/文档） |
| T8 | 报表数值 | stub 数据下精确 |
| T9 | 死信重放 | 原 event_id 重投，Inbox 幂等 |

## 7. 任务分解

1. 迁移 00008_operator.sql（含 messages/moments status 枚举扩展注释与 users 核对）。
2. `internal/operator/`：RequireOperator 中间件（httpx 扩展）→ service（R1-R13）→ handler（G1-G9）。
3. **审计 checklist 核对任务**：逐项列合同 §16.2 → grep 各域 audit 调用 → 补漏（产出到 PROGRESS.md）。
4. message/moment 的 HideContent 端口实现（小改：status 谓词核对）。
5. 单测 T1-T9。
6. 边界脚本 DOMAINS 加入 operator。

## 8. 验收测试计划（合同 §18 全量 → 可执行清单）

**本节是"阶段七"的执行脚本**。两类：
1. **自动化验收**（Go 测试，`-tags=acceptance`）：§18.1-18.7 各小节逐条映射到对应 SPEC 的测试用例编号（T 表），已有单测覆盖的直接引用；缺的补集成测试（需要 MySQL，integration tag）。产出 `docs/acceptance-matrix.md`：合同条目 ↔ 测试用例 ID ↔ 状态。
2. **性能基线**（§17）：`loadtest/` 目录（Go 编写，复用 API client）：10k WS 连接（连接保持+心跳）、消息发送 P95<300ms（1000 并发用户×100 msg）、历史查询 P95、朋友圈首屏 P95<500ms。结果写 `docs/loadtest-report.md`。**需要真实 MySQL+Redis+RabbitMQ 环境**（deploy/docker-compose.yml 一键起）。
3. **运维验收**（文档化检查单，不代码化）：MySQL 每日备份+binlog 归档、`innodb_flush_log_at_trx_commit=1`/`sync_binlog=1`、对象存储版本化、月度恢复演练——写入 `docs/ops-checklist.md`。

## 9. 验收
举报闭环幂等；处置立即生效且不删事实；审计仅运营可查；冻结吊销全端；报表可用；§18 矩阵全绿后项目验收通过。
