# 架构决策文档（ADR 合集）

> 本文件记录所有跨模块的架构级决策及其理由。修改架构前先在本文更新决策。
> 依据：《微信核心社交仿制项目最终需求分析.md》§12-§16，《产品交叉评审.md》六裁决。

## ADR-001 模块化单体，三进程拓扑

- **决策**：一个 Go module，三个可执行进程：`cmd/api`（HTTP）、`cmd/gateway`（WebSocket，阶段二）、`cmd/worker`（异步任务）。
- **理由**：合同 §12.1 明确禁止微服务；三进程是合同既定拓扑。
- **模块边界**：`internal/<domain>/` 每模块四文件起步——`service.go`（导出的业务接口与实现）、`store.go`（不导出的 MySQL 访问）、`handler.go`（HTTP 适配）、`dto.go`（API 契约）。**跨模块只允许调用对方 `service.go` 导出的接口；禁止跨模块读表/写表/拼对方 SQL。** 核心写操作必须经所属领域模块执行（合同 §12.3）。

## ADR-002 端口-适配器的基础设施抽象（platform 层）

- **决策**：`internal/platform/` 提供与领域无关的能力，全部以接口定义 + 多驱动实现：
  | 接口 | 生产驱动 | 开发降级驱动 |
  |---|---|---|
  | `cache.Cache`（会话缓存/限流/锁计数） | Redis | memory（进程内，接口语义一致） |
  | `storage.ObjectStore`（对象存储） | S3 兼容（MinIO/云 S3） | local（本地磁盘，同一 Key 语义） |
  | `mq.Publisher`（事件投递） | RabbitMQ | none（Worker 直读 MySQL Outbox） |
- **无驱动组件**：`sigtoken.Codec` 是**唯一**的 HMAC-SHA256 签名令牌实现，个人二维码令牌与本地驱动下载 URL 共用它；新增任何"签名/校验不透明载荷"的需求都走这里，禁止再写第二份 HMAC（`WECHAT_SIGNING_SECRET` 是唯一密钥源）。
- **理由**：合同 §12.5.2 明确 MySQL Outbox 是事实层、RabbitMQ 只是分发层；降级路径是合同内架构，不是临时绕路。领域代码只依赖接口，未来接入真实中间件零领域改动。
- **红线**：WebSocket、Redis、RabbitMQ 都不是消息事实源；事实只认 MySQL 事务（合同 §12.5.1）。

## ADR-003 认证与设备会话模型

- **令牌**：Access Token = 32 字节随机数（base64url），30 分钟；Refresh Token = 48 字节随机数，30 天。数据库只存 SHA-256 哈希（合同 §3.2/§3.3）。
- **校验链**：中间件取 Bearer → SHA-256 → cache 查 `sess:at:<hash>` → miss 回源 MySQL → 校验 `revoked_at IS NULL` 且未过期 → 写回 cache（TTL=剩余有效期）。吊销/改密/刷新时主动删 cache key，保证"立即失效"（合同 §3.3）。
- **轮换**：refresh 成功即换发新 refresh+access，旧 refresh 哈希作废；旧 access 同步从 cache 删除。旧 refresh 重放视为攻击信号 → 吊销整个设备会话。
- **登录锁定**：同一账号连续失败 10 次冻结 10 分钟。计数器放 cache（key=`lock:login:<account_key>`），冻结标记同库。统一返回 `INVALID_CREDENTIALS`（不区分账号不存在/密码错误）。
- **密码**：Argon2id（time=3, memory=64MB, threads=2, keylen=32），格式 `$argon2id$v=19$m=65536,t=3,p=2$salt$hash`。

## ADR-004 好友关系 epoch 与方向性权限

- **决策**（交叉评审裁决 3，不可违背）：
  - `friendships` 以 `(user_low, user_high, friendship_epoch)` 为唯一键；`user_low<user_high` 规范化存储，避免 A-B/B-A 双行。
  - 每次重新建立关系 epoch 取双方历史最大 epoch+1，**永久不复用**；`friendship_epochs(user_low,user_high)` 表保存当前 epoch 指针（合同 §12.4"当前 epoch 指针"）。
  - 方向性设置在 `friend_settings`：每行 = (owner_id, friend_id)，含 remark、message_perm(normal/no_message/blocked)、moment_perm(visible/hidden)、notify 开关。双方各一行，互不覆盖。
- **判定顺序**（合同 §2.2）：令牌→资源归属→关系/成员资格→资源状态/快照/保留期→发凭证。无权限与不存在统一 `RESOURCE_UNAVAILABLE`。

## ADR-005 标识与时间

- **内部主键**：`BIGINT UNSIGNED AUTO_INCREMENT`。API 的 JSON DTO 中所有 ID 字段以字符串输出（`json:",string"`），避免 JS 2^53 精度问题。
- **事件 ID/幂等键**：UUIDv4 字符串（outbox event_id、audit id、上传会话 id 等跨系统键）。
- **时间**：MySQL 连接固定 `time_zone='+00:00'`、`parseTime=true`；列一律 `DATETIME(6)`；Go 内一律 `time.Time` UTC；API 序列化 RFC3339。严禁服务器本地时区入库。

## ADR-006 事务与并发纪律

- **隔离级别**：`READ COMMITTED`（合同 §12.4），会话级设置在 DSN。
- **事务边界**：Service 层用 `mysqlx.WithinTx(ctx, func(tx) error)`；Store 方法全部接受 `tx` 参数。业务数据 + outbox_events 必须同事务提交。
- **锁顺序**（防死锁，全局统一）：users(按 id 升序) → friendships/friend_settings(user_low,user_high 升序) → conversations → conversation_members → 业务表。**群内写路径**（group 模块）：先锁 `` `groups` `` 行 `FOR UPDATE`，再写 group_members/group_events/系统消息（其下再走 conversation → messages），最后 outbox。死锁/锁超时以同幂等键最多重试 3 次（合同 §12.4）。
- **会话序号**：`conversation_seq` 通过 `SELECT ... FOR UPDATE` 锁会话行递增 `last_seq` 分配（合同 §12.4），禁止 AUTO_INCREMENT。
- **SKIP LOCKED** 仅用于 Worker 任务竞争，不用于用户读路径（合同 §14.5）。

## ADR-007 统一 API 契约

- 响应包：`{"code":"OK|ERROR_CODE","message":"...","data":{...},"request_id":"uuid"}`。HTTP 状态码与 code 映射集中在 `platform/errors`。
- 错误码基线（合同 §15.1）：`UNAUTHENTICATED / RESOURCE_UNAVAILABLE / FORBIDDEN / INVALID_ARGUMENT / STATE_CONFLICT / QUOTA_EXCEEDED / SYNC_CURSOR_EXPIRED / INTERNAL_ERROR` + 领域码（`INVALID_CREDENTIALS / ALREADY_FRIEND / CONTENT_UNAVAILABLE ...`）。
- 列表一律游标分页：`?cursor=<opaque>&limit=N`，响应含 `next_cursor`、`has_more`。
- 写接口支持幂等键（header `Idempotency-Key` 或业务幂等键如 `client_msg_id`）。
- `request_id` 中间件生成并注入日志上下文。

## ADR-008 审计

- `audit` 模块提供 `audit.Logger.Log(ctx, entry)`，其余模块以 fire-and-forget 语义调用（同事务写 `audit_logs` 表；审计丢失可接受的事件走 outbox，阶段二起）。
- 必记事件清单见合同 §16.2。阶段一覆盖：登录成功/失败、密码修改、设备退出、好友同意/删除/拉黑、媒体上传/下载。

## ADR-009 配置与运行模式

- 12-factor：全部走环境变量 `WECHAT_*`，`platform/config` 集中解析+校验，dev 默认值可裸跑（除 MySQL DSN）。
- 驱动开关：`CACHE_DRIVER=redis|memory`、`STORAGE_DRIVER=s3|local`、`MQ_DRIVER=rabbitmq|none`。
- 迁移：`goose` + `migrations/*.sql`（MySQL 方言，`-- +goose Up/Down`）。DDL 与数据回填分离（合同 §12.2）。`cmd/migrate` 内嵌 migrations（embed.FS）。

## ADR-010 阶段一范围冻结

阶段一只交付：注册/登录/令牌/设备/资料、好友申请/关系 epoch/方向权限/标签/搜索、上传/对象/下载凭证、审计、Outbox 表结构。conversation 仅实现"建 direct/transfer 会话 + 成员判定"供 contact 与注册流程调用；消息本体属阶段二。**阶段一代码中禁止出现消息发送、朋友圈、收藏的半成品实现。**

## ADR-011 MySQL 8.0 本地兼容

合同指定 8.4；本机只有 8.0.44。所用特性（CTE、窗口函数、`SKIP LOCKED`、生成列、`utf8mb4_0900_bin` 除外）8.0 均支持。大小写敏感需求用 `utf8mb4_bin` 排序规则（8.0/8.4 皆有）。docker-compose 固定 `mysql:8.4`，生产对齐合同。

## ADR-012 平台复用组件与分片协议演进

- 平台组件清单及"领域模块禁止重新发明"红线见 `docs/design-review.md` D11（argon/clock/validate/pagination/ratelimit/cache/storage/mq/mysqlx/httpx/errors/ids/outbox）。
- 媒体分片协议与 S3 MultipartUpload 同构（D3）：编号分片 ↔ part number，为未来客户端直传对象存储保留零迁移演进路径。
- Outbox：`platform/outbox.Emit(tx, event)` 是唯一事件写入器；Relay 领取逻辑（SKIP LOCKED + 租约）实现一次，`MQ_DRIVER=rabbitmq|none` 两模式共享（D4）。

## ADR-013 模块边界编译期强制

- 每领域模块 = 单一 Go 包；store 类型不导出，跨模块物理拿不到他方表访问类型。
- `scripts/check_boundaries.sh` 断言依赖图：platform 不依赖 domain；domain 间只 import 根包（service 接口）。
- Principal/Authenticator 接口定义在 `platform/httpx`，由 auth 模块实现（依赖倒置，platform 不 import domain，见 design-review D8）。

## 设计推敲记录

所有高风险决策（令牌模型、refresh 重放、分片协议、Outbox 租约、epoch 指针表、排序规则、keyset 分页、模块边界、限流、Argon2 参数）的逐项推演见 `docs/design-review.md`（D1–D12）。修改这些决策必须先更新该文档。
