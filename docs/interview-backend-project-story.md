# 秋招后端项目简历与面试叙事

> 适用项目：`myWechat` 微信核心社交仿制系统
> 口径说明：以下内容以《跨部门技术 Leader 高并发高可用评审》中的 P0/P1 问题已修复后的系统终态为准，不把开发过程中的中间态写成线上事实。

## 1. 简历项目描述

### 项目名称

**微信核心社交系统后端**（Go / MySQL / Redis / RabbitMQ / S3 / WebSocket）

### 一句话定位

一个支持账号设备、好友关系、单聊群聊、媒体文件、朋友圈、收藏清理、备份恢复和内容账号的模块化单体后端，重点解决高并发消息一致性、权限快照、异步可靠投递、媒体生命周期和多实例高可用问题。

### 简历版本 A：后端开发偏工程实现

- 设计并实现微信核心社交后端，采用 Go 模块化单体 + API/Gateway/Worker 三进程架构，覆盖账号、好友、群聊、消息、朋友圈、媒体、收藏、备份和内容账号 9 个业务域。
- 设计 MySQL 事实源模型：以好友关系 `friendship_epoch` 支持关系周期权限快照，以会话行锁分配连续 `conversation_seq`，通过 `(sender_id, client_msg_id)` 唯一键实现消息发送幂等。
- 基于 MySQL Outbox + RabbitMQ + Inbox 实现至少一次异步投递与业务恰好一次效果，覆盖实时推送、媒体处理、通知、生命周期清理和定时任务，并支持失败重试、死信与运营重放。
- 设计媒体分片上传与对象引用模型，支持 200MB 文件断点续传、服务端完整性校验、业务引用计数和延迟物理回收，避免多用户共享媒体时误删。
- 完成高可用治理：Redis 会话缓存与限流、多实例 WebSocket 路由、Worker 租约并发抢占、健康检查、Prometheus 指标和故障演练，支撑 10,000 并发 WebSocket 与消息落库 P95 < 300ms 的目标。

### 简历版本 B：系统设计与复杂业务建模偏强

- 主导设计熟人社交系统核心数据模型，将关系、权限、消息、媒体、内容拆分为独立领域，通过服务接口交互，禁止跨模块直读表。
- 用发布时可见性快照 + 当前关系实时校验解决朋友圈权限回溯问题，保证删除好友、重新加好友、拉黑和“仅聊天”不会扩大历史可见范围。
- 用事务性 Outbox 解耦业务提交与异步副作用，保证 MySQL 业务事实与事件生成原子提交，RabbitMQ 故障时消息发送仍可成功落库，恢复后自动补投。
- 针对高并发消息链路做分层优化：会话内串行化保证有序，跨会话并发扩展吞吐；读路径批量化消除 N+1；热点 Worker 使用 `SKIP LOCKED` 租约多实例抢占。
- 建立故障语义：发送幂等、推送可补、媒体清理可恢复、备份失败不破坏旧备份、恢复不修改线上数据，保证组件故障下有明确降级路径。

### 建议简历用量

简历上放 4 到 5 条即可，优先选择版本 A 的第 1、2、3、5 条。如果目标岗位强调系统设计，可加入版本 B 的第 2、3 条。不要把所有模块都堆上简历，面试官更看重深度。

## 2. 面试两分钟项目介绍

可以直接按下面这段讲：

> 这个项目是我做的一个微信核心社交后端，范围不是做一个聊天 Demo，而是覆盖账号设备、好友关系、单聊群聊、媒体、朋友圈、收藏清理、备份恢复和内容账号这些闭环。架构上我没有拆微服务，而是用 Go 的模块化单体，分成 API、WebSocket Gateway 和 Worker 三个进程。MySQL 是唯一事实源，Redis 只放会话缓存、限流和路由这类可重建状态，RabbitMQ 负责异步分发，S3 存媒体对象。
>
> 我最花时间的是三类问题。第一是消息一致性和并发：每个会话用 `conversation_seq` 保证会话内有序，发送时锁会话行分配序号，权限、禁言和媒体归属在同一个事务里校验，消息、媒体引用和 Outbox 事件原子提交，客户端幂等 ID 防重复发送。第二是异步可靠性：业务事务里写 Outbox，Worker 通过租约领取事件，RabbitMQ 用 publisher confirm 和手动 ACK，消费端 Inbox 去重，失败进入 1/5/30 分钟重试，超过 5 次进死信，运营可以重放但复用原 event_id。这样 MQ 挂了不影响消息落库，恢复后自动补投。第三是权限和媒体生命周期：朋友圈用发布时的好友 epoch 快照，访问时再用当前关系收紧，避免删除好友再加回来后历史动态越权；媒体用对象和引用分离，清理用户引用不会误删别人的共享对象，无引用后进入延迟回收。
>
> 最后我把系统按多实例高可用来验收：Redis 做共享缓存和限流，WebSocket 通过路由层把推送送到持有连接的实例，Worker 用租约多实例并发，API 和 Worker 都有健康检查和 Prometheus 指标。目标是 10,000 并发 WebSocket、消息落库 P95 小于 300ms、历史查询 P95 小于 300ms。

## 3. 项目背景与需求拆解

### 3.1 业务范围

| 模块 | 核心需求 |
|---|---|
| 账号设备 | 注册登录、Argon2id 密码、Access/Refresh Token 轮换、多设备会话和立即吊销 |
| 好友关系 | 好友申请、同意、删除、拉黑、标签、方向性权限 |
| 会话消息 | 单聊、群聊、文件传输助手、多类型消息、幂等、撤回、转发、置顶、断线同步 |
| 群聊 | 500 人群、角色权限、禁言、公告、邀请码、群待办、历史权限 |
| 媒体 | 分片上传、断点续传、完整性校验、短期下载凭证、引用计数和回收 |
| 朋友圈 | 五种可见范围、发布时快照、实时权限收紧、点赞评论、通知、定时发布 |
| 收藏清理 | 收藏独立快照、存储空间预览与确认清理、跨用户引用保护 |
| 备份恢复 | 单槽位备份、30 天保留、异步导出、恢复生成只读档案不覆盖线上数据 |
| 内容账号 | 公众号-like 账号、文章、菜单、关注通知和一对一客服 |

### 3.2 非功能目标

- 10,000 个并发 WebSocket 连接。
- 普通消息落库确认 P95 < 300ms。
- 单聊和群聊历史查询 P95 < 300ms。
- 朋友圈首屏 P95 < 500ms。
- 群人数上限 500，消息历史每页 50，朋友圈每页 20。
- 所有请求设置超时，写接口限流。
- RabbitMQ 不可用时，消息发送仍能落库，Outbox 积压，恢复后继续投递。
- 异步事件至少一次投递，业务副作用通过幂等做到恰好一次效果。

## 4. 技术方案总览

### 4.1 架构图

```text
Client
  |
  | HTTP
  v
API Server
  |-- Auth / Contact / Group / Message / Moment / Media / Favorite / Backup / Content
  |
  |-- MySQL 8.x：业务事实源，事务、Outbox、Inbox、重试、死信
  |-- Redis：会话缓存、登录锁定、限流、WebSocket 路由
  |-- S3/MinIO：媒体对象
  |
  | WebSocket
  v
Gateway
  |-- 连接管理 / 心跳 / ACK / 慢消费者保护

Worker
  |-- Outbox Relay：租约领取 -> MQ publish -> confirm -> published
  |-- Consumers：推送、媒体处理、通知、内容 fanout
  |-- Scheduler：定时朋友圈、待办逾期、重试任务
  |-- Lifecycle：消息过期、上传过期、备份过期、媒体 GC

RabbitMQ
  |-- durable exchange/queue
  |-- publisher confirm + mandatory
  |-- manual ACK + DLQ
```

### 4.2 为什么是模块化单体

这个项目规模下，微服务会带来更多问题：

- 分布式事务、服务发现、部署链路和排障成本都会上升。
- 业务边界还不稳定，过早拆服务会导致接口反复变化。
- 核心难点是数据一致性和高并发，不是团队并行开发。

因此采用一个 Go module，内部按领域拆包，运行时拆成 API、Gateway、Worker 三个进程。模块之间只能调用对方 `service.go` 的导出接口，不能直接读写对方表。这样保留单体的开发效率，也保留未来拆分的边界。

### 4.3 各组件职责

| 组件 | 职责 | 不承担的职责 |
|---|---|---|
| MySQL | 业务事实、事务、幂等、Outbox、Inbox、租约、审计 | 不做长期缓存，不做全文搜索平台 |
| Redis | 会话缓存、限流、登录锁定、WS 路由 | 不做唯一事实源 |
| RabbitMQ | 事件分发、削峰、异步解耦 | 不做事实源，不承诺恰好一次 |
| S3/MinIO | 媒体对象字节 | 不判断业务权限 |
| Worker | 异步副作用、批处理、重试、死信 | 不绕过领域服务直接修改核心业务事实 |

## 5. 核心数据关系

### 5.1 账号与会话

```text
users
  1 -> 1 user_profiles
  1 -> N user_devices
  1 -> N user_sessions
```

设计点：

- 密码只存 Argon2id 哈希。
- Access Token 和 Refresh Token 只存 SHA-256 哈希。
- Refresh Token 轮换，旧 token 重放视为令牌族泄露，吊销整个设备会话。
- 退出设备时以数据库 `revoked_at` 为事实源，Redis 缓存删除只是加速失效。

### 5.2 好友关系与权限

```text
users A/B
  pair -> friendship_epochs(current_epoch)
  pair -> friendships(user_low, user_high, friendship_epoch, status)
  A -> B friend_settings(owner_id, friend_id, message_perm, moment_perm)
  A -> N contact_tags -> contact_tag_members
```

关键设计是 `friendship_epoch`：

1. 每次重新建立好友关系，epoch 递增，不复用。
2. 朋友圈发布时保存每个可见用户的 `(user_id, friendship_epoch)`。
3. 访问历史动态时必须同时满足：发布时在允许集合、当时 epoch 匹配、当前仍是好友且当前关系未被拉黑或隐藏。

这样解决了“删除好友后又加回来，是否能看到旧朋友圈”的边界：不能。因为旧动态绑定旧 epoch，新关系是新 epoch。

### 5.3 会话与消息

```text
conversations
  1 -> N conversation_members
  1 -> N messages
  message 1 -> N message_assets
  message 1 -> 1 message_references
  message 1 -> N message_forwards
  conversation + user -> conversation_settings
```

关键约束：

- `messages(sender_id, client_msg_id)` 唯一，发送幂等。
- `messages(conversation_id, conversation_seq)` 唯一，会话内连续有序。
- `conversation_settings.last_read_seq` 只能前进。
- `is_marked_unread` 与阅读游标分离。
- 群成员历史按 `joined_at <= message.created_at < left_at` 判定。

### 5.4 媒体对象与引用

```text
media_objects
  1 -> N upload_sessions / upload_chunks（上传过程）
  1 -> N media_references（业务引用）
  1 -> N media_user_references（用户访问授权）
  message_assets / moment_assets / favorite_media_objects
```

核心思想是对象和引用分离：

- 上传阶段只证明字节完整，不等于业务可用。
- 消息、朋友圈、收藏各自写入自己的业务引用。
- 用户清理只撤销自己的 `media_user_references`，不影响其他用户。
- 只有所有业务引用消失后，对象才进入延迟回收队列。

### 5.5 朋友圈可见性

```text
moments
  1 -> N moment_assets
  1 -> N moment_visibility_users(moment_id, user_id, friendship_epoch, allowed)
  1 -> N moment_likes
  1 -> N moment_comments
  1 -> N moment_notifications
```

可见性判定顺序：

1. 动态未删除。
2. 访问者是作者，或当前是好友。
3. 当前 epoch 与发布时快照一致。
4. 作者没有对访问者设置 hidden / blocked / no_moments。
5. 访问者命中允许集合且不在排除集合。

这不是简单 ACL，而是“发布时快照 + 当前关系实时收紧”的模型。

### 5.6 异步任务

```text
业务事务
  -> outbox_events
  -> RabbitMQ
  -> inbox_events(consumer_name, event_id)
  -> async_retry_tasks
  -> async_dead_letters
```

关键约束：

- 业务数据和 Outbox 事件同一 MySQL 事务提交。
- 每个消费者独立 Inbox，唯一键 `(consumer_name, event_id)`。
- 重试使用固定阶梯：1 分钟、5 分钟、30 分钟，最多 5 次。
- 死信重放复用原 event_id，仍由 Inbox 幂等。

## 6. 高并发设计故事

### 6.1 消息发送：如何在保证有序的同时提高吞吐

面试可以这样讲：

> 我没有追求全局消息有序，因为不同会话之间没有必要串行。系统只保证单会话内 `conversation_seq` 连续递增。发送时先做参数和快速权限预检查，进入事务后锁定 `conversations` 行，重新校验关系、群成员、禁言和媒体归属，然后分配 `last_seq + 1`，同事务写入消息、媒体引用和 Outbox。这样同一个会话天然串行，不同会话锁不同行，可以并发扩展。

要点：

- 有序范围：会话内，不跨会话。
- 并发单位：conversation row。
- 幂等：`(sender_id, client_msg_id)`。
- 权限：事务内复核，避免 TOCTOU。
- 热点群优化方向：消息事实仍由 MySQL 保证，热点群可以做写合并或队列化，但不牺牲事实一致性。

### 6.2 读路径：从 N+1 到批量查询

初始实现容易犯的错误是：

```text
查 50 条消息
for each message:
    查会话类型
    查群成员
    查引用
    查资产
```

一次请求可能变成 200 次 DB 往返。最终优化为：

1. 每页查询一次会话类型和群信息。
2. 成员资格和好友权限批量判断。
3. 引用和资产用 `message_id IN (...)` 一次加载。
4. 会话列表 keyset 分页，最后消息和未读数批量聚合。

面试亮点是能明确说出：并发系统优化首先要减少请求放大系数，其次才是加缓存。

### 6.3 Worker 并发：租约与任务隔离

批任务不能一个 goroutine 串行执行所有事情，否则消息过期积压会饿死定时朋友圈。

最终设计：

- 每类任务独立 goroutine。
- 每个 tick 限量处理，例如消息过期每轮最多 5000 条。
- 每个任务独立超时和 recover。
- 多实例通过 MySQL 租约和 `FOR UPDATE SKIP LOCKED` 抢占。
- Worker ID 必须实例唯一。
- 提交结果必须校验 lease owner 和 execution version，防止旧 Worker 覆盖新执行。

### 6.4 群聊并发

群人数上限和邀请码使用次数都依赖条件更新：

```sql
UPDATE group_invite_codes
SET use_count = use_count + 1
WHERE id = ? AND use_count < max_uses;
```

群成员数在 `groups` 行上维护，写路径先锁 group 行，避免并发邀请超过 500 人。成员表用 active flag 唯一键保证重复入群幂等。

### 6.5 媒体上传并发

大文件完成上传不能直接全量组装，否则两个并发 complete 会重复消耗 200MB I/O。

终态流程：

1. 短事务将 upload session 从 `open` 抢占为 `assembling`，写租约。
2. 租约内读取分片，流式计算 SHA-256、大小和 MIME。
3. 校验通过后，同事务写入 `media_objects`、会话完成状态和 `media.process` Outbox。
4. 失败释放租约，过期后其他 Worker 可重试。
5. 过期清理只能修改 `open` 状态，不能覆盖 `completed`。

## 7. 高可用设计故事

### 7.1 业务事务与异步副作用解耦

没有 Outbox 时，常见问题是：

```text
begin tx
insert message
publish MQ
commit tx
```

如果 publish 后 commit 失败，消费者会收到一个不存在的事实。反过来，commit 后 publish 失败，事件又会丢。

最终方案：

```text
begin tx
insert message
insert outbox_event
commit

Worker:
lease outbox
publish MQ with confirm
mark published
```

跨 MySQL 和 RabbitMQ 无法原子，所以系统明确采用：

- 至少一次投递。
- 消费端 Inbox 去重。
- 业务副作用幂等。

这是面试中的关键点：不要假装跨资源能做事务，要承认重复窗口，再用幂等消除重复影响。

### 7.2 RabbitMQ 故障降级

场景：MQ 挂了，用户还能发消息吗？

答案：可以。

1. API 只依赖 MySQL 事务提交。
2. Outbox 事件留在 `pending`。
3. Relay 指数退避重连。
4. MQ 恢复后按 Outbox 继续投递。
5. 实时推送延迟，但消息事实不丢。

### 7.3 消费失败与死信

消费失败不能无限 requeue：

| 错误类型 | 处理 |
|---|---|
| 参数错误、资源已删除 | 永久失败，写失败原因，ACK，进入失败记录 |
| 网络、存储临时失败 | 写入 `async_retry_tasks`，1/5/30 分钟重试 |
| 超过 5 次 | 标记 dead，写 `async_dead_letters`，告警 |
| 人工修复后重放 | 复用原 event_id，Inbox 保证不重复副作用 |

### 7.4 多实例 API

多实例下必须解决：

| 问题 | 方案 |
|---|---|
| token 缓存不一致 | MySQL revoked 状态是事实源，缓存短 TTL，吊销删缓存 |
| 限流计数分裂 | Redis 固定窗口全局计数 |
| 登录锁定分裂 | Redis 计数 + 冻结标记 |
| WS 连接分布在不同实例 | Redis Pub/Sub 或 Rabbit fanout 路由到持有连接的实例 |
| 本地磁盘不可共享 | 生产使用 S3/MinIO，local 只用于开发 |

### 7.5 健康检查与可观测性

API：

- `/healthz`：进程存活。
- `/readyz`：MySQL ping、Redis ping、必要依赖检查；优雅关停时主动 503。

核心指标：

- HTTP QPS、延迟、错误率、in-flight。
- DB pool in-use、wait count、wait duration。
- Outbox pending、oldest age、publish failure。
- Queue depth、consumer latency、retry pending、dead letters。
- WS connections、slow consumer disconnects、ACK latency。
- Worker task duration 和 rows processed。

告警：

- Outbox 未发布事件超过 1 分钟。
- 队列消息年龄超过 5 分钟。
- 死信持续增长。
- 消息发送 P95 超过 300ms 持续 5 分钟。
- DB 池等待 P95 超过 100ms。

## 8. 关键故障场景推演

面试官很喜欢问“如果某个组件挂了怎么办”。以下是可以直接讲的场景。

### 场景 1：API 提交事务后、Outbox Relay 发布前崩溃

结果：事件仍在 `pending` 或租约超时状态。

恢复：其他 Relay 通过租约超时重新领取并发布。

风险：可能重复发布，由 Inbox 和业务幂等兜底。

### 场景 2：RabbitMQ 发布成功，但 Outbox 未标记 published

结果：事件会被重新发布。

恢复：消费者看到同一 event_id，Inbox 去重。

结论：系统不追求传输层恰好一次，而是通过消费端幂等达到业务恰好一次。

### 场景 3：消费者处理成功后、ACK 前崩溃

结果：RabbitMQ 重投。

恢复：Inbox 已记录处理状态或业务唯一键兜底，重复消息不会产生重复副作用。

### 场景 4：MySQL 短暂不可用

结果：API 写请求失败，不返回假成功。

处理：请求超时和错误率告警；readyz 变 503，LB 摘流；客户端按幂等键重试。

### 场景 5：Redis 不可用

结果：缓存和限流能力下降。

处理：会话鉴权回源 MySQL；限流按配置选择 fail-open 或 fail-closed，并输出明确告警。事实状态不丢失。

### 场景 6：S3 不可用

结果：新上传和下载受影响，但已提交的 MySQL 事实不变。

处理：上传失败可续传；媒体处理进入重试；下载返回明确错误，不删除引用。

### 场景 7：WebSocket 推送失败

结果：客户端实时性下降。

处理：消息已落库，客户端重连后按 `conversation_seq` 增量同步；ACK 只推进 delivered 状态，不改变消息事实。

## 9. 面试深挖问题与回答要点

### Q1：为什么不用 Kafka？

当前需求是事件分发、重试、死信和消费者幂等，RabbitMQ 已足够。项目不需要事件回放、长期流存储和多消费者重复读历史日志。更重要的是，即使换 Kafka，也必须保留 MySQL Outbox 和 Inbox 幂等，因为跨资源一致性不能靠消息系统解决。

### Q2：为什么不用分布式锁分配消息序号？

消息序号只需要会话内有序，MySQL 会话行锁就是天然的分布式锁。引入 Redis 锁会增加锁过期、续约、脑裂和失败恢复问题，还可能造成 MySQL 事实与缓存不一致。会话行锁简单、可验证、能和事务一起提交。

### Q3：会话行锁会不会成为瓶颈？

会。但它只影响单个热点会话，不影响其他会话并发。优化路径是把 payload 解码、权限预检查和大 I/O 移出锁外，锁内只做最终校验和写入。极端热点群可以再做写入合并，但事实和序号仍必须由 MySQL 保证。

### Q4：如何保证消息不丢？

消息成功返回前，消息、媒体引用和 Outbox 已在 MySQL 同一事务提交。推送失败不等于消息失败，客户端断线后按 seq 同步。MQ 丢消息由 Outbox 重投补齐，消费失败由 retry 和 dead letter 兜底。

### Q5：如何保证消息不重？

发送端用 `(sender_id, client_msg_id)` 唯一键。异步链路承认至少一次投递，消费端用 `(consumer_name, event_id)` Inbox 去重，业务表再用唯一键兜底。也就是说，传输层允许重复，业务效果不允许重复。

### Q6：朋友圈权限为什么不做通用 ACL 或规则引擎？

需求只有五种固定可见范围，直接保存发布时用户集合和 epoch 快照更简单、可审计、可索引。通用规则引擎会扩大范围，还会让权限调试和数据回滚更复杂。

### Q7：媒体删除时如何避免影响其他用户？

对象和引用分离。一个用户清理自己的引用，只 revoke 自己的 `media_user_references`。物理删除前检查所有业务引用，只有无引用才进入延迟回收。收藏还会生成独立快照，不受原消息过期影响。

### Q8：备份为什么不能在 HTTP 请求里同步做？

备份最大 10GB，同步 HTTP 必然超时且无法恢复。终态是异步任务、分片导出、part 表断点续跑、进度查询和完成后原子替换备份槽位。失败时旧备份保持不变。

### Q9：如何验证高并发？

不只是压测 QPS，还要验证正确性：

- 并发发送同一 `client_msg_id`，只产生一条消息。
- 50 并发发送同一会话，`conversation_seq` 无空洞无重复。
- 双 Worker 抢占同一任务，只有一个副作用。
- 断开 MQ 后 API 仍可落库，恢复后积压清零。
- 双实例 API 吊销 token 后另一个实例立即拒绝。
- 10,000 WS 连接下内存和 goroutine 有界。

### Q10：这个项目最难的点是什么？

推荐回答：

> 最难的点不是写出接口，而是在一个复杂社交系统里划清事实边界。比如消息成功到底是 MySQL 提交成功，还是 WebSocket 推送成功；朋友圈权限是看当前好友，还是看发布时快照；媒体删除是删对象还是删用户引用。我的做法是先定义事实源和失败语义，再设计锁、幂等和异步链路。这样每个组件故障时，系统行为都是可推导的。

## 10. 项目亮点总结

可以把以下五点作为面试主轴：

1. **数据建模能力**：好友 epoch、会话 seq、媒体对象引用分离、朋友圈可见性快照，都是复杂业务关系而不是简单 CRUD。
2. **事务一致性**：核心业务、媒体引用、Outbox 事件同事务提交，明确跨 MySQL/RabbitMQ 不做伪原子。
3. **高并发设计**：会话内串行、跨会话并发；幂等键；批量读；Worker 租约；条件更新防超卖。
4. **高可用设计**：MQ 降级、重试、死信、重放、健康检查、指标告警、多实例路由。
5. **工程质量**：模块边界脚本、迁移、单元测试、MySQL 集成测试、故障注入和性能验收。

## 11. 面试中的表达建议

1. 先讲业务闭环，再讲技术，不要一上来堆组件。
2. 每个技术点都绑定一个具体失败场景或并发场景。
3. 主动讲取舍，例如“为什么不用 Kafka”“为什么不用 Redis 锁”“为什么不用微服务”。
4. 承认系统假设：RabbitMQ 是至少一次，Redis 不是事实源，MySQL 是唯一事实。
5. 用数字表达目标：10,000 WS、P95 300ms、500 人群、200MB 文件、180 天消息保留。
6. 不要说“完美解决所有问题”，要讲“在什么故障窗口内保证什么语义”。

## 12. 完整项目故事（STAR 版）

如果面试官让你完整讲一次项目，可以按这个结构展开，控制在五到八分钟。

### S：背景和目标

我要做一个微信核心社交后端，不是单机聊天 Demo，而是一个有明确性能和可靠性目标的系统。业务上要覆盖账号设备、好友关系、单聊群聊、媒体、朋友圈、收藏清理、备份恢复和内容账号；技术上要支持 10,000 并发 WebSocket、消息落库 P95 小于 300ms、历史查询 P95 小于 300ms，并在 MySQL、Redis、RabbitMQ、S3 或 Worker 故障时保持明确语义。

### T：主要技术任务

我把问题拆成四层：

1. **领域建模**：识别用户、设备、会话、消息、好友关系、权限、媒体对象、业务引用、异步事件这些核心实体。
2. **一致性**：决定哪些数据在同一个 MySQL 事务提交，哪些副作用通过 Outbox 异步化。
3. **并发**：确定锁粒度和幂等键，避免全局锁，同时保证会话内有序。
4. **可用性**：为每个组件设计故障行为、重试策略、降级路径和监控指标。

### A：核心设计和实现

#### 第一层：先确定事实源

我首先确定 MySQL 是唯一事实源。Redis 只保存会话缓存、限流和路由；RabbitMQ 只负责分发；S3 只存字节。这样可以避免多份数据互相冲突。

#### 第二层：建模复杂关系

好友关系不是简单布尔值，而是带 `friendship_epoch` 的关系周期。朋友圈发布时保存用户和 epoch 快照，访问时同时检查发布时权限和当前关系。这样删除好友再重新添加后，旧朋友圈不会重新可见。

消息用会话和 `conversation_seq` 建模，会话内连续有序；用 `(sender_id, client_msg_id)` 唯一键保证发送幂等；群消息按成员加入和退出时间判断历史可见性。

媒体采用对象和引用分离：`media_objects` 表示字节对象，`media_references` 表示消息、朋友圈、收藏等业务引用，`media_user_references` 表示用户访问授权。这样用户清理自己的引用不会误删共享对象。

#### 第三层：设计消息发送链路

发送消息时，先做参数校验和快速预检查；进入事务后锁定会话行，并重新校验好友权限、群成员、禁言、媒体归属，然后分配 `last_seq + 1`，同事务写入消息、媒体引用和 Outbox 事件。这样会话内有序，跨会话可并发，权限变化不会产生 TOCTOU。

#### 第四层：设计异步链路

业务事务提交后，Worker 用租约领取 Outbox 事件，发布到 RabbitMQ，收到 publisher confirm 后标记 published。消费者用 Inbox 唯一键去重，临时失败按 1/5/30 分钟重试，最多 5 次，超限进死信。跨 MySQL 和 MQ 不做伪原子，承认至少一次投递，用幂等保证业务效果只有一次。

#### 第五层：高可用治理

API 使用 Redis 共享缓存和限流，readyz 检查 MySQL 和 Redis；Worker 使用租约和多实例抢占；Gateway 负责连接、心跳、ACK 和慢消费者保护；核心链路暴露 Outbox 积压、队列延迟、DB 池等待、消息 P95 和 WS 连接指标。

### R：结果和验证

最终验证不只看功能，而是看并发和故障语义：

- 同一 `client_msg_id` 并发发送，只产生一条消息。
- 同一会话 50 并发发送，序号无空洞、无重复。
- MQ 断开时消息仍可落库，恢复后 Outbox 补投。
- 消费者重复收到同一 event_id，业务副作用只有一次。
- 双实例 API 吊销 token 后，另一个实例立即拒绝。
- 10,000 WebSocket 连接下内存和 goroutine 数量有界。

这个故事的重点是：我不是先选组件再拼功能，而是先定义事实源、事务边界和失败语义，再选择锁、幂等和异步机制。

## 13. 面试证据索引

面试前可以快速复习这些位置，保证每个说法都有代码或文档支撑。

| 主题 | 证据位置 |
|---|---|
| 架构决策 | `docs/ARCHITECTURE.md` |
| 性能与可靠性目标 | `微信核心社交仿制项目最终需求分析.md` §17 |
| Outbox/Inbox 规格 | `docs/specs/10-runtime.md` |
| 消息发送幂等与序号 | `internal/message/service.go`、`internal/message/store.go`、`migrations/00004_message.sql` |
| 好友 epoch | `internal/contact/service.go`、`internal/contact/store.go`、`migrations/00001_phase1_foundation.sql` |
| 朋友圈可见性快照 | `internal/moment/service.go`、`internal/moment/store.go`、`migrations/00005_moment.sql` |
| 群并发和角色权限 | `internal/group/service.go`、`internal/group/store.go`、`migrations/00003_group.sql` |
| 媒体对象与引用 | `internal/media/service.go`、`internal/media/store.go`、`migrations/00001_phase1_foundation.sql` |
| Redis 会话与限流 | `internal/platform/cache/cache.go`、`internal/platform/ratelimit/ratelimit.go`、`internal/device/service.go` |
| 可信代理与客户端 IP | `internal/platform/httpx/response.go` |
| 健康检查 | `cmd/api/main.go` |
| 模块边界治理 | `scripts/check-boundaries.sh` |
| 高并发评审与整改 | `reviews/跨部门技术Leader高并发高可用评审.md` |

## 14. 当前代码基线说明

截至 2026-09-30，代码中已经落地的修复包括：

- Redis 驱动和启动 ping，未支持驱动会启动失败。
- `/readyz` 检查 MySQL 和 Redis，优雅关停时返回不可用。
- 消息发送在事务内重新校验好友权限、群权限和媒体归属。
- 消息媒体写入 durable 的 `media_references`。
- 群系统消息、群事件和成员计数错误会导致事务回滚。
- 定时朋友圈提交时校验 execution version、lease owner 和 lease until。
- 客户端 IP 解析引入可信代理配置，避免伪造 X-Forwarded-For 绕过限流。
- 边界检查脚本在 Go 不存在时会正确失败。
- 运行时加固已开始落地：`LocalRouter`、事务化 Inbox、Outbox lease version 和 retry/dead-letter 信封迁移已出现，但尚未接入 `cmd/worker` 的完整启动链路。

当前仍未完全落地的部分：

- Worker 尚未接入 Outbox Relay 和 Inbox 消费者。
- RabbitMQ 和 S3 生产适配器尚未实现。
- WebSocket Gateway 的完整鉴权、ACK、跨实例路由尚未实现。
- Metrics 与告警体系尚未完整落地。
- 高并发压测和故障演练还未完成。

因此，本文的“简历版本”是目标终态口径；如果面试官要求现场看代码，应主动说明项目仍在稳定性验收阶段，并列出已修复和待修复项。不要把目标架构说成已经全部上线，否则深挖时容易失去可信度。

2026-09-30 验证结果：

- `go test ./...` 通过。
- `go vet ./...` 通过。
- `scripts/check-boundaries.sh` 在当前 WSL 环境因缺少 Go 正确失败，边界检查需要在有 Go 的 shell 或 CI 中执行。
