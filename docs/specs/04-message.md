# SPEC-04 message（消息、同步、WebSocket、180 天保留）

> 依据：合同 §5、§9、§14.1、§14.4、§15.2、§18.2；交叉评审裁决 4（阅读游标）；ADR-006。
> 读者：低等级 LLM 执行者。**本文件已包含全部技术判断，不得自行变更设计；遇到本文未覆盖的情况，停下并在 PROGRESS.md 记录问题，不要即兴决策。**

## 0. 前置依赖（必须已存在）

- 阶段一代码：`internal/platform/*`、`internal/conversation`、`internal/device`、`internal/auth`、`internal/user`。
- 迁移 0001 已含 `conversations`（`last_seq`）、`conversation_members`、`outbox_events`、`inbox_events`。
- 群成员资格判定由 `internal/group` 提供（SPEC-05）。**模块依赖方向：message → group（只调 service 接口）**。

## 1. 范围

**做**：消息发送（8 种类型）、会话序号、幂等、撤回、引用、转发、置顶、标记未读、阅读游标、未读数、历史拉取与断线同步、WebSocket 网关、在线推送与 delivered 确认、180 天过期、文件传输助手复用、会话列表、会话设置。
**不做**：已读回执给对方展示、输入中状态、在线状态、消息编辑、"删除所有人"、跨会话全局搜索（合同明确排除）。

## 2. 数据模型（迁移 0002，goose MySQL 方言）

```sql
CREATE TABLE messages (
  id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  conversation_id  BIGINT UNSIGNED NOT NULL,
  conversation_seq BIGINT UNSIGNED NOT NULL,
  sender_id        BIGINT UNSIGNED NOT NULL,  -- 系统消息=触发者或0
  sender_type      VARCHAR(8) NOT NULL,       -- user|staff|system
  client_msg_id    VARCHAR(64) COLLATE utf8mb4_bin NOT NULL DEFAULT '', -- 系统消息为 ''
  type             VARCHAR(16) NOT NULL,      -- text|emoji|image|video|voice|file|card|link|system
  payload          JSON NOT NULL,             -- 按 §3 payload schema
  status           VARCHAR(16) NOT NULL,      -- stored|delivered|recalled|expired
  created_at       DATETIME(6) NOT NULL,
  expires_at       DATETIME(6) NOT NULL,      -- created_at + 180d，插入时计算
  recalled_at      DATETIME(6) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_messages_client (sender_id, client_msg_id),
  UNIQUE KEY uk_messages_conv_seq (conversation_id, conversation_seq),
  KEY idx_messages_expiry (status, expires_at),
  KEY idx_messages_conv_time (conversation_id, id),
  KEY idx_messages_created (created_at)   -- 运营日报按时间窗统计（SPEC-11 R9）
) /* 列字符集 utf8mb4，payload 用默认 0900 排序规则即可 */;

CREATE TABLE message_assets (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  message_id     BIGINT UNSIGNED NOT NULL,
  media_object_id BIGINT UNSIGNED NOT NULL,
  kind           VARCHAR(16) NOT NULL,   -- image|video|voice|file|thumb
  created_at     DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_massets_msg (message_id),
  KEY idx_massets_obj (media_object_id)
);

CREATE TABLE message_references (        -- 引用回复
  message_id     BIGINT UNSIGNED NOT NULL,  -- 引用者（新消息）
  ref_message_id BIGINT UNSIGNED NOT NULL,  -- 被引用消息
  ref_sender_id  BIGINT UNSIGNED NOT NULL,
  ref_type       VARCHAR(16) NOT NULL,
  ref_digest     VARCHAR(255) NOT NULL,   -- 引用时摘要快照（≤200 runes）
  created_at     DATETIME(6) NOT NULL,
  PRIMARY KEY (message_id)
);

CREATE TABLE message_forwards (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  source_message_id BIGINT UNSIGNED NOT NULL,
  target_message_id BIGINT UNSIGNED NOT NULL,
  forwarded_by   BIGINT UNSIGNED NOT NULL,
  created_at     DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_mfwd_source (source_message_id)
);

CREATE TABLE message_pins (
  conversation_id BIGINT UNSIGNED NOT NULL,
  message_id      BIGINT UNSIGNED NOT NULL,
  pinned_by       BIGINT UNSIGNED NOT NULL,
  created_at      DATETIME(6) NOT NULL,
  PRIMARY KEY (conversation_id, message_id),
  KEY idx_mpins_conv_created (conversation_id, created_at)
);

-- 会话设置（每用户独立，合同 §5.2；阶段一迁移未建，此处补）
CREATE TABLE conversation_settings (
  conversation_id   BIGINT UNSIGNED NOT NULL,
  user_id           BIGINT UNSIGNED NOT NULL,
  pinned            TINYINT UNSIGNED NOT NULL DEFAULT 0,  -- 会话置顶
  muted             TINYINT UNSIGNED NOT NULL DEFAULT 0,  -- 免打扰
  background        VARCHAR(255) NOT NULL DEFAULT '',     -- 聊天背景（URL key）
  last_read_seq     BIGINT UNSIGNED NOT NULL DEFAULT 0,   -- 只前进（裁决4）
  is_marked_unread  TINYINT UNSIGNED NOT NULL DEFAULT 0,
  unread_anchor_seq BIGINT UNSIGNED NOT NULL DEFAULT 0,   -- 仅界面定位
  updated_at        DATETIME(6) NOT NULL,
  PRIMARY KEY (conversation_id, user_id)
);

```

**不建的表**：`message_receipts`（已读回执不做；delivered 确认不需要独立表——见 R22，条件 UPDATE 天然幂等）、消息分区（合同 §12.4 明确首期不分区）。
**群会话说明**：`conversation_members` 对 type=group 的会话只在建群时写入创建者一行；群会话的成员资格真源是 `group_members`（SPEC-05），可读性按成员区间判定（R15）。执行者不要试图为群会话维护 conversation_members。

## 3. payload schema（JSON 字段固定，执行者不得增删）

| type | payload 字段 | 校验规则 |
|---|---|---|
| text | `{"content": string}` | 1..10000 runes；NoControl |
| emoji | `{"content": string, "image_object_id": int64?}` | content ≤ 64 runes；image_object_id 为图片表情 |
| image | `{"media_object_id": "...", "width": int, "height": int}` | 对象 owner=sender、status=ready、mime∈image/*、≤20MB |
| video | `{"media_object_id": "...", "duration_ms": int}` | 对象 mime=video/mp4、≤200MB；duration ≤ 600000 |
| voice | `{"media_object_id": "...", "duration_ms": int}` | mime∈audio/*、≤10MB；duration ≤ 60000 |
| file | `{"media_object_id": "...", "file_name": "...", "size": int}` | ≤200MB；file_name ≤ 255 字节 |
| card | `{"user_id": "...", "nickname": "...", "account_name": "...", "avatar_media_id": "..."}` | 名片主人必须是**发送者当前好友**（消息事务内校验）；字段为发送时快照 |
| link | `{"url": "...", "title": "...", "summary": "...", "thumb_media_id": "..."?}` | url ≤ 2048 且 http(s)；title ≤ 128；summary ≤ 512；不抓取正文 |
| system | `{"event": "...", ...}` | 仅服务端生成；客户端不可发送 type=system |

（JSON 内 ID 一律字符串。）

## 4. 领域规则（编号即测试用例编号）

### 4.1 发送（合同 §5.5）
- R1：请求必带 `client_msg_id`（客户端 UUID）；`(sender_id, client_msg_id)` 唯一。重复提交：查出已有消息，**原样返回第一次结果**（含原 conversation_seq），HTTP 200，不产生新消息。
- R2：发送流程（**两段式，最小化会话行锁持有时间——高并发关键路径**）：
  1. **锁外预检**（普通快照读，不持锁）：成员/关系/禁言/客服状态/媒体归属与限制校验（R3-R7 全部规则）+ 幂等预查（`SELECT id ... WHERE sender_id=? AND client_msg_id=?` 命中直接返回）；
  2. 事务内：`SELECT last_seq, type FROM conversations WHERE id=? FOR UPDATE`（**锁住后只做**：`seq = last_seq + 1`；UPDATE last_seq；INSERT messages（expires_at = now + 180d）；INSERT message_assets；INSERT message_references；`outbox.Emit(tx, ...)`）→ 提交后返回 `stored`。
  - 锁序：预检读不锁；事务内先 conversations 后 messages（与群操作锁序一致，见 SPEC-05 §6）。
  - 锁等待控制：`mysqlx` 连接会话级 `SET innodb_lock_wait_timeout = 5`（秒）；锁等待超时返回 `UNAVAILABLE`（服务端繁忙语义），由 mysqlx.WithinTx 统一识别。**群会话 500 人并发发送在会话行上串行是合同"连续递增 seq"的固有代价**，锁内只剩 3 个 INSERT，单消息锁持有点位 <2ms。
- R3：单聊（type=direct）权限：双方当前 friendship 必须 active；`to 对 from` 的 message_perm=blocked 或 from 对 to blocked → 拒绝；`to 对 from`=no_message → 拒绝（from 仍可发给 to 的场景 = from 是设置方：即设置方主动发不受自己 no_message 限制——判定调用 `contact.CanSendMessage(from, to)`）。
- R4：群聊（type=group）：调用 `group.CanSend(userID)`（SPEC-05 R14）；群解散 → 拒绝。
- R5：transfer：仅会话归属者本人（conversations.transfer_owner_id = sender）。
- R6：official_service：仅 `service_session.status=active` 时 user 可发；客服只能发 text（校验在 service_session 模块的 CanSend 封装，message 只调接口）。
- R7：空文字（content 为空或全空白）、未知 type、payload 违反 §3 校验、媒体对象不 ready/非本人 → `INVALID_ARGUMENT`，**不写任何行**。
- R8：返回体：`{message_id, conversation_id, conversation_seq, status:"stored", created_at}`。

### 4.2 撤回/引用/转发/置顶（合同 §5.6）
- R9 撤回：`UPDATE messages SET status='recalled', recalled_at=? WHERE id=? AND sender_id=? AND status IN ('stored','delivered') AND created_at > now()-2min`；rows=0 → `STATE_CONFLICT`（超时或非本人或已撤回）。同一事务写 outbox `message.recalled`。撤回后读取返回占位（§4.5 R22）。
- R10 引用：发送时带 `ref_message_id`。校验：引用者必须能读被引用消息（同会话、成员窗口内、未过期）；快照 digest=按类型取前 200 runes。**不复制 payload 全量**。
- R11 转发：入口=对"当前用户可读且未撤回未过期"的消息调用发送接口（新会话、新 client_msg_id、type 不变、payload 深拷贝），另写 message_forwards 记录溯源。card 转发目标会话若非发送者好友关系 → 拒绝（R2 卡片校验自然覆盖）。批量转发：每目标会话一条消息，接口循环调用（服务端单循环，非并发）。
- R12 置顶：单聊任一方；群聊仅群主/管理员（`group.IsModerator`）。`INSERT IGNORE INTO message_pins`；会话已置顶 ≥ 20 条 → `QUOTA_EXCEEDED`。取消置顶 DELETE。置顶列表按 created_at desc。
- R13 标记未读：`UPDATE conversation_settings SET is_marked_unread=1, unread_anchor_seq=<当前 last_seq>`；**绝不修改 last_read_seq**（裁决 4）。进入会话确认阅读：`last_read_seq = max(last_read_seq, 确认序号)`，`is_marked_unread=0`。**conversation_settings 行是懒创建的：本节所有写操作一律 UPSERT（INSERT ... ON DUPLICATE KEY UPDATE），不存在"先 SELECT 判存在再插"的路径**（并发安全）。

### 4.3 历史与同步（合同 §5.8）
- R14 拉取：`GET /conversations/{id}/messages?after_seq=&limit=50`（**limit 1..50，默认 50——合同 §17 每页 50 条**）。返回 seq > after_seq 且用户可读的消息，按 seq asc。after_seq 缺省=从最新往回取（返回最后 50 条，倒序参数 `before_seq` 支持；两参互斥）。
- R15 可读性过滤（每条消息必须满足）：
  - direct/transfer/official_service：用户是当前成员（conversation_members，left_at IS NULL）；official_service 关闭后仍可读（历史只读）。
  - group：消息 `created_at` ∈ [该用户该群的**某个**成员区间 (joined_at, left_at)]（group_members 多行多区间）。
  - `status != 'expired'`。
- R16 `SYNC_CURSOR_EXPIRED`：**可用窗口 = 该会话内 expires_at > now 的消息**（过期消息只剩占位行，正文不可读）。当 `after_seq < MIN(可用窗口内用户可读消息的 seq) - 1` 时返回该错误（指示客户端丢弃旧游标、以当前最早可用 seq 重新同步）。实现：一条 `SELECT MIN(conversation_seq) FROM messages WHERE conversation_id=? AND expires_at>? ` 再套 R15 过滤取 MIN 比较。
- R17 同步接口幂等：重复调用同一 after_seq 返回相同结果集（无副作用）。
- R18 未读数：`COUNT(messages WHERE conversation_id=? AND seq > last_read_seq AND 满足 R15 可读 AND status IN ('stored','delivered') AND sender_id != 用户)`（recalled/expired 不计入未读）。不用序号差。
- R19 会话列表：用户全部会话按 `最后一条可读消息时间 desc`；每项含：conversation_id、type、对端摘要（direct=对方昵称/备注；group=群名）、最后消息摘要、未读数、置顶/免打扰标记。摘要规则：recalled→"[消息已撤回]"；expired→"[消息已过期]"；媒体类→"[图片]/[视频]/[语音]/[文件]"。

### 4.4 WebSocket 网关（合同 §15.2）
- R20 建连：`GET /ws`（协议升级前先带 Bearer？不行——浏览器 WS 不能自定义 header。**用 `GET /ws?access_token=...` 查询参数鉴权**，建连后立即校验，失败关闭码 4001。**安全注意：access_token 会出现在 URL 里——访问日志对 `/ws` 路径必须不记录 query string（logger/httpx 中间件加白名单规则），且 access token 生命周期仅 30 分钟**）。设备维度连接：同一 device_id 多连接时后连踢前连（关 4002）。
- R21 推送帧（服务端→客户端，JSON）：
  ```json
  {"event_id":"uuid","type":"message.stored","conversation_id":"123","message_id":"456","conversation_seq":789,"sender_id":"1","preview":"文本前50字"}
  ```
  其他 type：`message.recalled`、`conversation.updated`（群事件）、`notification.new`（朋友圈/待办/文章通知聚合，阶段三起）。
- R22 客户端确认帧：`{"type":"ack","event_id":"uuid","message_id":"456"}`。网关收到后（进程内）调 `message.MarkDelivered(message_id)`：`UPDATE messages SET status='delivered' WHERE id=? AND status='stored'`——条件更新天然幂等，**无需 message_receipts 表**（合同只要求"至少一个目标设备确认"这一事实；谁确认的属于审计性信息，不建表存储）。**网关不写任何业务表**——所有写操作通过进程内 service 调用（合同 §15.2"Gateway 不直接修改业务表"：指不经业务逻辑直写表）。
- R23 在线路由：进程内 `map[userID]→set[*conn]`（多设备）。RabbitMQ 模式下由 `wechat.message.push` 消费者调网关的 `Deliver(userID, frame)`；none 模式下内嵌 Relay 直投。网关收到目标不在线 → 忽略（客户端靠 HTTP 同步补齐）。**多实例限制（V1 已知边界）**：`wechat.message.push` 是单消费者队列，多 Gateway 实例部署时只有持锁消费者所在实例能投递本地连接——**V1 约束：Gateway 单实例部署**（合同 §17 目标 10k 连接，单实例可承载；横向扩展留作后续版本，届时增加 per-instance 匿名广播队列，不属本期）。执行者不要自行实现广播拓扑。
- R24 断线恢复：客户端重连后逐会话调 R14 同步；服务端不补推离线事件。
- R25 心跳：ping/pong 30s，60s 无 pong 断开。单连接推送队列上限 1024 帧，满则断开（慢消费者保护）。**单 IP 并发 WS 连接上限 100**（防滥用，超出拒绝 4003）；Hub 的 `map[userID]` 读写用 sync.RWMutex，推送路径零 DB 查询。

### 4.5 180 天保留（合同 §5.7、§14.4）
- R26 Worker 批处理（每次 ≤ 1000 条，循环直到空）：`SELECT id FROM messages WHERE status IN ('stored','delivered') AND expires_at <= now LIMIT 1000 FOR UPDATE SKIP LOCKED`；逐条：`UPDATE messages SET status='expired'`；删除该消息的 message_assets 行（业务媒体引用）；对每个 affected media_object 调 `media.OnReferencesRemoved(object_ids)`（SPEC-03 R13，无引用则入 GC 队列）。**保留消息行本身**（序号占位+幂等信息）。**不为过期发 outbox 事件**（一条消息一个事件会在保留期边界产生事件风暴；客户端通过正常同步/未读数变化感知过期，见 R16/R18 的过滤语义）。
- R27 过期消息任何在线入口不可读正文（读取过滤 status）；`message_references.ref_digest` 快照保留可展示（引用显示"原消息已过期"由客户端根据被引用消息状态渲染——接口在组装引用时附带 `ref_status` 字段）。
- R28 保留期不因任何操作延长；expires_at 建行时定死。

## 5. API 契约（全部 `/api/v1` 前缀，鉴权除注明外必需）

| # | 方法 路径 | 请求 | 响应 data | 错误 |
|---|---|---|---|---|
| A1 | POST /conversations/{id}/messages | `{client_msg_id, type, payload, ref_message_id?}` | R8 | INVALID_ARGUMENT, FORBIDDEN(禁言), STATE_CONFLICT |
| A2 | GET /conversations/{id}/messages?after_seq=&limit= / ?before_seq=&limit= | — | `{messages:[...], has_more, next_cursor}` | RESOURCE_UNAVAILABLE, SYNC_CURSOR_EXPIRED |
| A3 | GET /conversations | `?cursor=&limit=` | R19 列表 | — |
| A4 | GET /conversations/{id} | — | 会话详情+我的设置 | — |
| A5 | POST /conversations/{id}/read | `{seq}` | `{last_read_seq}` | INVALID_ARGUMENT |
| A6 | POST /conversations/{id}/mark-unread | — | — | — |
| A7 | PATCH /conversations/{id}/settings | `{pinned?, muted?, background?}` | — | INVALID_ARGUMENT |
| A8 | POST /messages/{id}/recall | — | — | STATE_CONFLICT |
| A9 | POST /messages/{id}/forward | `{target_conversation_ids:[...], client_msg_ids:[...]}`（一一对应） | `{results:[{conversation_id, message_id, conversation_seq}]}` | INVALID_ARGUMENT |
| A10 | POST /conversations/{id}/pins | `{message_id}` | — | QUOTA_EXCEEDED, FORBIDDEN |
| A11 | DELETE /conversations/{id}/pins/{message_id} | — | — | — |
| A12 | GET /conversations/{id}/pins | — | 置顶列表 | — |
| A13 | GET /ws?access_token= | 升级 | — | 4001/4002 |

消息读取视图（A2 元素）：
```json
{
  "message_id":"456","conversation_id":"123","conversation_seq":789,
  "sender_id":"1","sender_type":"user","type":"text",
  "payload":{"content":"hello"},"status":"stored","created_at":"...",
  "ref":{"message_id":"100","sender_id":"2","type":"text","digest":"...","status":"recalled"},
  "assets":[{"media_object_id":"9","kind":"image"}]
}
```
recalled 消息：`"status":"recalled","payload":null`（正文不出网）。

## 6. 关键实现算法

### 6.1 未读数（避免全表 COUNT 的大会话）
上限 500 人群、180 天窗口，直接 COUNT 走 `(conversation_id, conversation_seq)` 索引足够（P95 < 300ms 目标内）。**不做二级计数表**（YAGNI，记入 design-review 备查）。

### 6.2 会话列表"最后可读消息"查询与未读数（高并发热点接口的预算）
A3 会话列表是全系统最热读接口。**明确的查询预算**：每页 20 个会话，每会话 2 条索引查询（最后消息：`uk_messages_conv_seq` 反向取 1；未读数：`seq > last_read_seq` 的 range COUNT）+ 会话成员/设置 1 次批查（`WHERE user_id=me AND conversation_id IN (...)`）= **每页约 45 条索引查询，全部命中索引，无全表扫描**。P95 目标 300ms 内（合同 §17）。**不建未读计数表**（一致性成本 > 收益；如未来压测超标，优化路径=会话维度计数器表，记入 design-review 备查，执行者不要预先实现）。

### 6.3 WS 网关进程结构（cmd/gateway）
```
internal/ws/           ← 新包（domain: ws，加入边界脚本 DOMAINS）
  hub.go     Hub{ conns map[int64]map[string]*Conn } // user→device→conn
  conn.go    Conn{ send chan Frame(1024), ws conn }
  auth.go    建连鉴权（复用 auth.Service.Authenticate）
cmd/gateway/main.go    只启动 Hub + 消费 wechat.message.push（none: 直投函数）
```
Hub 依赖注入接口 `type Deliverer interface{ Deliver(ctx, userID int64, f Frame) }`。**API 进程与 Gateway 进程不共享内存**——MQ_DRIVER=none 时两进程合并部署由 cmd/api 内嵌 Hub（`--embed-gateway` 启动参数，默认 true 于 dev）。

## 7. 幂等与并发

- I1：发送幂等=唯一索引兜底；并发同 client_msg_id：后提交者撞 `uk_messages_client` → 回滚 → 改为 SELECT 已有行返回（服务层处理 1062）。
- I2：seq 分配：会话行锁串行化；死锁按 mysqlx.WithinTx 重试（锁顺序：先 conversations 后 messages）。
- I3：MarkDelivered：条件 UPDATE 幂等（无独立表）。
- I4：recall/pin/read 全部条件更新，天然幂等。

## 8. 测试用例清单（执行者必须全部落成 Go 测试）

| ID | 用例 | 断言 |
|---|---|---|
| T1 | 重复 client_msg_id | 只有一行消息，两次响应相同 |
| T2 | 空文本/超长文本/未知类型 | INVALID_ARGUMENT，无行 |
| T3 | 非成员发送 | RESOURCE_UNAVAILABLE |
| T4 | no_message/blocked 各方向 | 按 R3 精确拒绝/放行 |
| T5 | 群禁言者发送 | FORBIDDEN |
| T6 | seq 连续性：并发 50 发送 | seq 1..50 无空洞无重复 |
| T7 | 撤回 119s/121s | 成功/STATE_CONFLICT |
| T8 | 撤回后读取 | payload=null 占位 |
| T9 | 转发已撤回消息 | 拒绝 |
| T10 | 置顶第 21 条 | QUOTA_EXCEEDED |
| T11 | 标记未读后 last_read_seq 不变 | 裁决 4 |
| T12 | read 回退序号 | 被钳制只前进 |
| T13 | 群成员窗口：入群前/退群后消息不可读 | R15 |
| T14 | after_seq 过旧 | SYNC_CURSOR_EXPIRED |
| T15 | 180 天过期 worker | status=expired、assets 删除、消息行保留 |
| T16 | 过期消息不出现在 A2/A3 摘要 | R19 摘要=[消息已过期] |
| T17 | WS：鉴权失败 4001、重复设备 4002 | R20 |
| T18 | WS ack 后 status=delivered；重复 ack 幂等 | R22 |
| T19 | transfer 会话非本人发送 | RESOURCE_UNAVAILABLE |
| T20 | 客服会话关闭后发送 | STATE_CONFLICT |

## 9. 任务分解（执行顺序，每步 build+vet 通过后才进下一步）

1. 迁移 0002（§2 全部表）。
2. `internal/message/store.go`：所有 SQL（无业务逻辑）。
3. `internal/message/service.go`：R1-R13、R15-R19（WS 相关除外）+ `MarkDelivered`。
4. `internal/message/dto.go` + `handler.go`：A1-A12。
5. 单测 T1-T16（fake 依赖注入：contact/group/media 用接口 stub）。
6. `internal/ws/`：Hub/Conn/鉴权，T17-T18。
7. `cmd/api` 增加 /ws 路由（embed-gateway）。
8. `cmd/worker`：过期批处理任务（R26）接入 runtime（SPEC-10）。
9. 边界脚本 DOMAINS 加入 message、ws。

## 10. 验收标准（合同 §18.2 映射）

- 单聊/群聊/transfer/客服会话全部类型可收发；重复 client_msg_id 只一条；提交后才返回成功；2 分钟撤回边界正确；180 天后任何在线入口不可读；断线按序补齐；游标只前进；标记未读不回退；群成员只读加入期间消息；群解散后只读。
