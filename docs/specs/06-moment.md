# SPEC-06 moment（朋友圈、互动、定时发布）

> 依据：合同 §7、§13.4、§14.2；交叉评审裁决 3（epoch 快照）；ADR-004。
> 读者：低等级 LLM 执行者。**全部技术判断已写死；未覆盖情况 → 停下记录 PROGRESS.md。**

## 1. 范围

**做**：动态发布（文字/≤9图/城市位置/可见范围/评论点赞开关）、时间线与相册、点赞、一级评论与回复、删除、互动通知（聚合+已读）、朋友圈免打扰（user_moment_settings，阶段一已建表）、定时发布（≤30 天、抢占事务、失败重试）。
**不做**：视频动态、动态转发、原地编辑、二级以上回复、经纬度。

## 2. 跨模块契约（消费方定义接口）

```go
// moment 需要的 contact 端口（contact.Service 实现，SPEC-02 已有 IsFriend；需补 epoch 版本）：
type FriendSnapshotProvider interface {
    // ActiveFriendsWithEpoch 返回 author 当前全部 active 好友的 (user_id, friendship_epoch)。
    ActiveFriendsWithEpoch(ctx, userID int64) ([]FriendEpoch, error)
    // IsFriendCurrentEpoch 判断 viewer 是否 author 的 active 好友且 epoch 一致。
    IsFriendCurrentEpoch(ctx, authorID, viewerID int64, epoch int64) (bool, error)
    // MomentPerm 裁决 4/5：返回 author 对 viewer 的 hidden/black/moment_perm。
    MomentPerm(ctx, authorID, viewerID int64) (hidden, blocked, noMoments bool, err error)
    // ExpandTag 展开标签成员为 (user_id, epoch) 快照。
    ExpandTag(ctx, ownerID, tagID int64) ([]FriendEpoch, error)
    // IsMuted：viewer 对 author 的朋友圈免打扰（user_moment_settings.mute_author）。
    IsMuted(ctx, viewerID, authorID int64) (bool, error)
}
```
> SPEC-02（contact）如缺上述函数，属本规格任务 1：在 contact 包补齐（不改表）。

## 3. 数据模型（迁移 `migrations/00004_moment.sql`）

```sql
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
  status         VARCHAR(16) NOT NULL DEFAULT 'visible',  -- visible|deleted|moderated（moderated=运营隐藏，读取与 deleted 同拒，见 SPEC-11 R7）
  deleted_at     DATETIME(6) NULL,
  created_at     DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_moments_author (author_id, status, id)
);

CREATE TABLE moment_assets (
  moment_id      BIGINT UNSIGNED NOT NULL,
  media_object_id BIGINT UNSIGNED NOT NULL,
  position       TINYINT UNSIGNED NOT NULL,      -- 0..8 排序
  created_at     DATETIME(6) NOT NULL,
  PRIMARY KEY (moment_id, position),
  KEY idx_massets_obj (media_object_id)
);

CREATE TABLE moment_visibility_users (      -- 发布时快照（合同 §13.4 明确列）
  moment_id          BIGINT UNSIGNED NOT NULL,
  user_id            BIGINT UNSIGNED NOT NULL,
  friendship_epoch   BIGINT UNSIGNED NOT NULL,
  allowed            TINYINT UNSIGNED NOT NULL,  -- 1=允许集合 0=排除集合
  snapshot_at        DATETIME(6) NOT NULL,
  PRIMARY KEY (moment_id, user_id),
  KEY idx_mvis_user (user_id, allowed)
);

CREATE TABLE moment_likes (
  moment_id  BIGINT UNSIGNED NOT NULL,
  user_id    BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (moment_id, user_id)
);

CREATE TABLE moment_comments (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  moment_id   BIGINT UNSIGNED NOT NULL,
  user_id     BIGINT UNSIGNED NOT NULL,
  reply_to    BIGINT UNSIGNED NULL,          -- 仅一级回复：指向另一条 comment
  content     VARCHAR(500) NOT NULL,
  status      VARCHAR(16) NOT NULL DEFAULT 'visible',  -- visible|deleted
  deleted_at  DATETIME(6) NULL,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_mcomments_moment (moment_id, status, id)
);

CREATE TABLE moment_notifications (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  recipient_id BIGINT UNSIGNED NOT NULL,     -- 动态作者（或被回复者？裁决见 R16）
  moment_id   BIGINT UNSIGNED NOT NULL,
  kind        VARCHAR(8) NOT NULL,           -- like|comment|reply
  actor_id    BIGINT UNSIGNED NOT NULL,
  created_at  DATETIME(6) NOT NULL,
  read_at     DATETIME(6) NULL,
  PRIMARY KEY (id),
  KEY idx_mnotif_recipient (recipient_id, read_at, id)
);

CREATE TABLE moment_schedules (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  author_id     BIGINT UNSIGNED NOT NULL,
  run_at        DATETIME(6) NOT NULL,
  timezone      VARCHAR(64) NOT NULL DEFAULT 'Asia/Shanghai',
  status        VARCHAR(16) NOT NULL DEFAULT 'scheduled',  -- scheduled|published|cancelled|failed
  current_version INT UNSIGNED NOT NULL DEFAULT 1,          -- 每次修改+1
  execution_version INT UNSIGNED NOT NULL DEFAULT 0,        -- 抢占时递增（14.2）
  lease_owner   VARCHAR(64) NULL,
  lease_until   DATETIME(6) NULL,
  moment_id     BIGINT UNSIGNED NULL,        -- 发布成功后回填；UNIQUE 见下
  fail_reason   VARCHAR(255) NULL,
  retry_count   INT UNSIGNED NOT NULL DEFAULT 0,
  created_at    DATETIME(6) NOT NULL,
  updated_at    DATETIME(6) NOT NULL,
  published_at  DATETIME(6) NULL,
  cancelled_at  DATETIME(6) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_mschedules_moment (moment_id),
  KEY idx_mschedules_due (status, run_at)
);

CREATE TABLE moment_schedule_contents (      -- 当前版本内容（修改=覆盖+version+1）
  schedule_id BIGINT UNSIGNED NOT NULL,
  version     INT UNSIGNED NOT NULL,
  content     VARCHAR(2000) NOT NULL DEFAULT '',
  country     VARCHAR(64) NOT NULL DEFAULT '',
  province    VARCHAR(64) NOT NULL DEFAULT '',
  city        VARCHAR(64) NOT NULL DEFAULT '',
  place_name  VARCHAR(128) NOT NULL DEFAULT '',
  PRIMARY KEY (schedule_id, version)
);

CREATE TABLE moment_schedule_assets (
  schedule_id BIGINT UNSIGNED NOT NULL,
  version     INT UNSIGNED NOT NULL,
  media_object_id BIGINT UNSIGNED NOT NULL,
  position    TINYINT UNSIGNED NOT NULL,
  PRIMARY KEY (schedule_id, version, position)
);

CREATE TABLE moment_schedule_visibility_users (
  schedule_id BIGINT UNSIGNED NOT NULL,
  version     INT UNSIGNED NOT NULL,
  user_id     BIGINT UNSIGNED NOT NULL,
  friendship_epoch BIGINT UNSIGNED NOT NULL,
  allowed     TINYINT UNSIGNED NOT NULL,
  PRIMARY KEY (schedule_id, version, user_id)
);
```
（`user_moment_settings` 阶段一已建，含 hide_author/mute_author/blacklist——以 0001 迁移实际列为准，缺失列在本迁移补齐。）

## 4. 领域规则

### 4.1 发布（合同 §7.1/§7.2）
- R1 内容校验：content ≤2000 字符；图片 0..9 张（media_object owner=作者、ready、mime image/*、单张 ≤20MB——单张大小上传时已限，此处只查 ready+归属）；空文字且无图且无位置 → INVALID_ARGUMENT。
- R2 可见范围 `visibility` 枚举：`self|all_friends|selected|tag|exclude`（不给谁看）。发布事务内展开（调 §2 端口）：
  - self：只写作者自身 (epoch=0)；
  - all_friends：全部好友 (uid, epoch) allowed=1；
  - selected：`selected_user_ids` ⊆ 当前好友，逐个 (uid, epoch) allowed=1；含非好友 → INVALID_ARGUMENT；
  - tag：ExpandTag(tag_id) 展开 → 同上；
  - exclude：全部好友 allowed=1 + `excluded_user_ids`（必须⊆好友）写 allowed=0（**同 user 出现两行时以 (moment_id,user_id) 主键冲突——裁决：exclude 模式下被排除者直接不写 allowed=1 行，只写 allowed=0 行**；判定=命中 allowed=1 且不在排除集）。
- R3 发布事务：INSERT moments → moment_assets → moment_visibility_users → outbox `moment.published`（推好友时间线，SPEC-10 消费）→ commit 后返回。
- R4 修改=删除+重发（合同），不提供 PATCH。

### 4.2 访问判定（合同 §7.3，单一函数 `canView(viewer, moment)`，所有读路径复用）
```
1. moment.status = visible
2. viewer == author                                  → 放行
3. ActiveFriendWithEpoch(author, viewer) == false    → 拒绝（实时）
4. snapshot epoch 不一致                              → 拒绝（快照）
5. author 对 viewer hidden / blocked / no_moments     → 拒绝（实时）
6. visibility_users 无 (viewer, allowed=1) 记录       → 拒绝（快照）
   （exclude 模式：viewer 在 allowed=0 集中 → 拒绝）
7. 媒体另有有效引用的过期豁免不适用于动态（动态本身不过期，媒体 GC 由引用决定）
```
**收紧不扩大**（合同）：以上 3/5 为实时判定（删好友、拉黑、仅聊天立即失效；重新加好友 epoch 不同也不恢复）。所有列表/详情/相册/评论/点赞/通知/原图接口必须走同一函数——**禁止任何接口绕过**。

### 4.3 时间线、相册（合同 §7.4）
- R5 我的时间线 `GET /moments/feed?cursor=`：
  - 候选集 SQL（一条查询）：`SELECT m.* FROM moments m WHERE m.author_id=me AND m.status='visible' UNION SELECT m.* FROM moments m JOIN moment_visibility_users v ON v.moment_id=m.id AND v.user_id=me AND v.allowed=1 WHERE m.status='visible'`，按 (created_at desc, id desc) keyset 分页 20 条。
  - **批量权限过滤（禁止逐条 N+1）**：取一页 20 条后，收集去重的作者集合，**一次性批量**查询：好友关系+epoch（`WHERE (a,b) IN (...)` 或作者 IN + viewer=me）、快照命中（`WHERE moment_id IN (...) AND user_id=me`）、hidden/blocked/moment_perm（friend_settings 一次查询）；页内同作者共享结果。批量过滤后放行的条目才返回。**逐条 canView 仅用于单条读取接口（详情/相册单条/原图/互动前置）**。
- R6 个人主页 `GET /users/{id}/moments`：仅本人（全部）或 canView 过滤后的。相册 `GET /users/{id}/moments/album`：仅有图的动态，同权限过滤。
- R7 时间线条目：正文、城市、缩略图（thumb variant 的下载 URL，走 media 签名 URL）、互动摘要（点赞数/评论数/我是否点赞）。
- R8 原图：`GET /moments/{id}/assets/{position}/original` —— 先 canView 再向 media 请求签名 URL（302 或 JSON 返回 url，**裁决：JSON 返回 `{url, expires_in}`**）。

### 4.4 互动（合同 §7.4）
- R9 点赞：`POST/DELETE /moments/{id}/likes`。前置 canView + allow_likes（关闭后禁止新增，历史保留——合同）。唯一键兜底幂等；删除幂等。不能给自己点赞？**裁决：允许**（微信真实行为允许）。
- R10 评论：`POST /moments/{id}/comments {content, reply_to?}`。content 1..500；reply_to 必须是同动态的 visible 评论（指向已删除评论 → STATE_CONFLICT？**裁决：拒绝 INVALID_ARGUMENT，客户端对已删父评论显示占位**——实际微信允许回复已删除评论占位；为降低歧义，拒绝并让客户端处理）。前置 canView + allow_comments。作者可删任意评论；用户可删自己评论；删除=软删，子回复保留显示"原评论已删除"。
- R11 删除动态：作者本人。软删（status=deleted）；互动保留但不可达；媒体引用随 moment_assets 行保留（GC 依引用计数，SPEC-03）。
- R12 通知：like/comment/reply 产生 moment_notifications（recipient=动态作者；reply 的 recipient=被回复者，同时评论通知仍发作者——两行）。按动态聚合展示（读接口 GROUP BY moment_id 取最新）；单条/全部标记已读 `POST /moments/notifications/read {ids?|all}`。**自己对自己的操作不发通知**。朋友圈免打扰（IsMuted）只影响 WS 推送，不影响通知行与已读。

### 4.5 定时发布（合同 §7.5/§14.2）
- R13 创建 `POST /moment-schedules`：`{run_at, content, assets(media_object_ids≤9), location?, visibility{...}, timezone?}`。校验：run_at > now 且 ≤ now+30d；时区仅作客户端语义（**裁决：run_at 一律转 UTC 存储；timezone 字段仅记录，服务端不解析时区库**——API 要求客户端传 ISO8601 含偏移或 UTC）；媒体 ready；可见范围同 R2 展开为 version=1 快照。
- R14 修改 `PATCH /moment-schedules/{id}`：仅 status=scheduled；内容/位置/可见范围整体覆盖到新 version 行（current_version+1），run_at 可改（仍限 30d 内）；旧 version 行保留。取消 `DELETE`：status=cancelled。
- R15 到期执行（Worker，精确按合同 §14.2 七步）：
  ```
  1. SELECT 候选：status='scheduled' AND run_at<=now
  2. 事务A（抢占）：SELECT ... FOR UPDATE SKIP LOCKED →
     UPDATE moment_schedules SET execution_version=execution_version+1,
       lease_owner=?, lease_until=now+2m WHERE id=? AND status='scheduled'
  3. 读 current_version 内容/资产/可见快照
  4. 实时校验：媒体仍 ready；发布时好友/epoch/权限照 R2 语义重算？——
     裁决（依合同"发布时仍执行实时关系校验，不能因快照扩大权限"）：
     保持创建时快照，但发布时将快照与实时关系求交：
     对每个快照 (uid, epoch) 再验 IsFriendCurrentEpoch + MomentPerm（§2 端口），
     不满足者剔除。作者被删好友等导致集合空 → 仅剩作者自身可见（不失败）。
  5. 事务B：INSERT moments + assets + 交集后的 visibility + outbox moment.published +
     UPDATE moment_schedules SET status='published', moment_id=?, published_at=?
  6. uk_mschedules_moment + status 条件保证重复执行只产生一条动态
  7. 失败：fail_reason + retry_count+1 + status='failed'；用户主动重试
     POST /moment-schedules/{id}/retry → status='scheduled'（run_at=now）
  ```
- R16 发布前任务不出现在任何时间线/相册/通知；仅 `GET /moment-schedules`（我的任务列表，含 status/fail_reason）可见。
- R17 租约回收（Worker 常规任务）：`status='scheduled' AND lease_until<now AND lease_owner IS NOT NULL` → 清空租约回到可抢占池。

## 5. API 契约（`/api/v1`，需鉴权）

| # | 方法 路径 | 请求 | 响应 data |
|---|---|---|---|
| C1 | POST /moments | `{content?, assets?, location?, visibility{type, selected_user_ids?|tag_id?|excluded_user_ids?}, allow_comments?, allow_likes?}` | `{moment_id, created_at}` |
| C2 | DELETE /moments/{id} | — | — |
| C3 | GET /moments/feed?cursor= | — | 20 条+next_cursor |
| C4 | GET /moments/{id} | — | 详情+互动摘要 |
| C5 | GET /users/{id}/moments?cursor= | — | 主页动态 |
| C6 | GET /users/{id}/moments/album?cursor= | — | 相册 |
| C7 | GET /moments/{id}/assets/{pos}/original | — | `{url, expires_in}` |
| C8 | POST /moments/{id}/likes | — | — |
| C9 | DELETE /moments/{id}/likes | — | — |
| C10 | GET /moments/{id}/comments?cursor= | — | 评论树（一级+回复平铺带 reply_to） |
| C11 | POST /moments/{id}/comments | `{content, reply_to?}` | `{comment_id}` |
| C12 | DELETE /moments/{id}/comments/{cid} | — | — |
| C13 | GET /moments/notifications?cursor= | — | 聚合通知列表 |
| C14 | POST /moments/notifications/read | `{ids?:[], all?:bool}` | — |
| C15 | POST /moment-schedules | 见 R13 | `{schedule_id}` |
| C16 | PATCH /moment-schedules/{id} | 同 C1 内容字段+run_at? | — |
| C17 | DELETE /moment-schedules/{id} | — | — |
| C18 | GET /moment-schedules?cursor= | — | 我的任务列表 |
| C19 | POST /moment-schedules/{id}/retry | — | — |

错误映射：无权限一律 `FORBIDDEN`（canView 拒绝）、动态不存在或不可见统一 `NOT_FOUND`（**裁决：不可见与不存在同响应，防探测**）；关闭互动后新增 → `STATE_CONFLICT`。

## 6. 幂等与并发

- I1：点赞唯一键幂等；评论无幂等键（业务允许重复内容）。
- I2：定时抢占=execution_version 条件更新+SKIP LOCKED，多 Worker 安全（测试并发抢占仅一个成功）。
- I3：发布成功回填 moment_id 唯一键，重复执行撞键 → 事务回滚 → 重读状态为 published 即视为成功。
- I4：canView 双层（快照+实时）在每次读取执行，无缓存一致性问题。

## 7. 测试用例

| ID | 用例 | 断言 |
|---|---|---|
| T1 | 空文字无图无位置 | INVALID_ARGUMENT |
| T2 | 10 张图 | INVALID_ARGUMENT |
| T3 | 五种可见范围快照展开 | visibility_users 行精确匹配 |
| T4 | 删好友后访问其历史动态 | 拒绝（实时收紧） |
| T5 | 重新加好友（epoch+1）后访问旧动态 | 拒绝（快照 epoch 不匹配） |
| T6 | 标签发布后改标签成员 | 历史动态可见性不变 |
| T7 | hidden/black/no_moments 各自立即拒绝 | R4 步骤5 |
| T8 | 时间线分页游标稳定性 | 同 cursor 重复请求同结果 |
| T9 | 相册/原图接口走 canView | 无权限拿不到 URL |
| T10 | 关闭评论后评论 | STATE_CONFLICT；历史保留 |
| T11 | 重复点赞/取消 | 幂等 |
| T12 | 删父评论后子回复 | 占位显示 |
| T13 | 定时：抢占并发 5 worker | 仅 1 个执行；单条动态 |
| T14 | 定时：run_at 超 30 天/过去 | INVALID_ARGUMENT |
| T15 | 定时：发布时好友已删 | 交集剔除，其余可见 |
| T16 | 定时：失败→retry | failed→scheduled→published |
| T17 | 发布前任务不在 feed/album | R16 |
| T18 | 修改任务生成 version+1，旧版本保留 | R14 |
| T19 | 通知聚合+标记已读（单条/全部） | R12 |
| T20 | 免打扰：WS 不推，通知行存在 | R12 |

## 8. 任务分解

1. contact 包补齐 §2 端口函数（ActiveFriendsWithEpoch / IsFriendCurrentEpoch / MomentPerm / ExpandTag / IsMuted）+ 单测。
2. 迁移 00004_moment.sql。
3. `internal/moment/store.go` → `service.go`（canView 单一函数 + R1-R12）→ `dto.go` → `handler.go`（C1-C14）。
4. 单测 T1-T12（contact 端口用 stub）。
5. 定时子域：schedule service（R13-R17）+ handler（C15-C19）+ 单测 T13-T18。
6. Worker 接入点：`PublishDueSchedules(ctx)` 与 `RecoverScheduleLeases(ctx)` 函数（SPEC-10 调度）。
7. 边界脚本 DOMAINS 加入 moment。

## 9. 验收

可见性六条件全链路（feed/详情/相册/原图/评论/点赞/通知不可绕过）；epoch 快照与实时收紧并存正确；定时发布抢占/幂等/失败重试正确；分页稳定；互动开关语义正确。
