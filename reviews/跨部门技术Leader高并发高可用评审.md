# 跨部门技术 Leader 高并发高可用评审

评审日期：2026-09-29
评审角色：另一部门技术 Leader，只看技术方案与实现，不改产品范围。
评审基线：`微信核心社交仿制项目最终需求分析.md`、`docs/ARCHITECTURE.md`、`docs/specs/10-runtime.md`、`docs/PROGRESS.md` 与当前 `main` 代码。

## 1. 总体结论

**当前实现不能按“高并发高可用系统”验收，只能按单机开发原型验收。**

核心原因不是没有采用微服务或 Kafka，而是四个基础承诺没有闭环：

1. 可靠异步链路没有接入运行时：业务代码写入 Outbox，但 Worker 没有 Relay 和消费者，事件会永久停留 `pending`。
2. 生产 HA 驱动缺失且配置有误导性：配置接受 `redis`、`s3`、`rabbitmq`，实际 Redis 静默降级为进程内存，S3 和 RabbitMQ 未实现。
3. 消息发送鉴权存在 TOCTOU 竞态：好友、群禁言、媒体归属校验都在事务和会话锁外执行，锁内只复核会话类型。
4. 读路径存在大量 N+1 查询：消息历史、会话列表、朋友圈 Feed 在高并发下难以达到合同 P95 目标。

分级标准：

| 级别 | 定义 |
|---|---|
| P0 | 破坏正确性、可靠性或导致核心功能不可验收 |
| P1 | 在高并发、多实例、故障恢复下造成明显性能、可用性或数据质量问题 |
| P2 | 工程质量与验收风险，会放大排障和演进成本 |

## 2. 问题清单

| 编号 | 级别 | 问题 | 主要位置 |
|---|---|---|---|
| F-01 | P0 | Outbox 与消费者未接入 Worker | `cmd/worker/main.go:77` |
| F-02 | P0 | Redis 配置静默降级为内存，多实例会话吊销和限流失效 | `internal/platform/cache/cache.go:28` |
| F-03 | P0 | Inbox 与业务副作用不是同一事务，重试链路有缺陷 | `internal/runtime/inbox.go:57` |
| F-04 | P0 | 消息发送鉴权和媒体校验在事务外 | `internal/message/service.go:122` |
| F-05 | P1 | 消息历史、会话列表、朋友圈读路径 N+1 | `internal/message/service.go:410` |
| F-06 | P1 | Worker 单 goroutine 串行执行全部任务 | `cmd/worker/main.go:77` |
| F-07 | P1 | 媒体上传、过期清理和 GC 有并发与泄漏问题 | `internal/media/service.go:150` |
| F-08 | P1 | 群模块多处吞掉数据库错误 | `internal/group/service.go:324` |
| F-09 | P1 | 定时朋友圈租约版本未参与提交条件 | `internal/moment/store.go:491` |
| F-10 | P1 | WebSocket 只有进程内 Hub | `internal/ws/hub.go:2` |
| F-11 | P1 | 可观测性和健康检查不足 | `cmd/api/main.go:330` |
| F-12 | P1 | 公网限流信任 `X-Forwarded-For` | `internal/platform/httpx/response.go:78` |
| F-13 | P2 | 边界检查脚本存在假阳性 | `scripts/check-boundaries.sh:15` |

## 3. P0 问题

### F-01 Outbox 与消费者未接入 Worker

**证据**

- `cmd/worker/main.go:77-123` 只启动一个每分钟 ticker，直接执行消息过期、群待办、禁言、申请过期、上传过期、定时朋友圈和备份过期，没有创建 `runtime.NewRelay`，也没有 `InboxConsumer`。
- `cmd/api/main.go:104` 构造 `moment.Service` 时 Outbox 参数是 `nil`；`cmd/api/main.go:107` 构造 `content.Service` 时也是 `nil`。
- `internal/message/service.go:189`、`:320`、`:356` 写入 `message.stored`、`message.recalled` 等事件，但没有进程消费。
- `internal/platform/mq/mq.go:37-40` 的 `NewNop` 是真正丢弃事件，而不是本地路由。
- `docs/PROGRESS.md:51` 声称 runtime 已完成，与 Worker 实际接线不一致。

**影响**

- 消息接口可以返回 `stored`，但实时推送不会发生。
- `media.process` 不会消费，媒体处理状态不会推进。
- 朋友圈通知、文章 fanout、内容账号通知无法到达。
- `outbox_events` 单调增长；`operator.QueueStats` 只能看到积压，没有恢复能力。
- “MQ 不可用时 API 仍可落库，恢复后继续投递”的降级语义没有实现，因为正常情况也没有投递。

**改进路径**

1. 先把 `none` 驱动做成真正的本地路由：Worker 领取 Outbox 后按 `event_type` 调用同一张 handler 表，并走 Inbox 去重与重试，不允许 `NewNop` 伪装成功。
2. `cmd/worker` 按 `docs/specs/10-runtime.md` 接线 `wechat.message.push`、`wechat.media.process`、`wechat.notification`、`wechat.content.service` 等消费者。
3. RabbitMQ 驱动实现 publisher confirm、mandatory return、手动 ACK、重连和 DLQ；MySQL 仍是事实源。
4. `PROGRESS.md` 应按能力项拆分完成度：Relay、Inbox、Scheduler、Rabbit 适配器、metrics、gateway 分别标注。

**验收**

- 发送消息后，Outbox 从 `pending` 到 `published`，handler 执行一次，Hub 收到推送。
- 杀掉 Worker 后消息仍可落库；重启后积压被消费且副作用只有一次。
- RabbitMQ 断连时 API 成功率不下降，恢复后积压清零。

### F-02 Redis 配置静默降级为内存

**证据**

- `internal/platform/config/config.go:161` 接受 `WECHAT_CACHE_DRIVER=redis`。
- `internal/platform/cache/cache.go:28-35` 中除 `memory` 之外都返回 `NewMemory()`。注释说“fail loudly”，实际没有错误返回。
- `cmd/api/main.go:60` 调用 `cache.New` 后没有驱动探测。
- `deploy/docker-compose.yml:3` 默认只有 MySQL 和 API，没有 Redis、RabbitMQ、MinIO、Worker、Gateway。

**影响**

多 API 实例下：

- 实例 1 吊销 token 后只删除实例 1 的缓存；实例 2 仍可能命中旧 token，破坏“立即失效”。
- 登录失败锁定和限流计数按实例分裂，攻击者轮询实例可获得近似 N 倍额度。
- 本地对象存储无法跨实例读取，多实例部署不可用。

**改进路径**

1. 未实现驱动必须在启动时失败；`cache.New` 应返回 `(Cache, error)`，配置层只允许已实现驱动。
2. 实现 Redis 驱动并启动 ping。
3. 会话吊销以 DB 事实源为准，缓存使用短 TTL 或版本号；删缓存只是加速失效。
4. 限流和登录锁定使用共享 Redis；Redis 故障时明确 fail-open/fail-closed 并告警。
5. 部署拓扑补齐 Worker、Gateway、Redis、共享对象存储，并配置健康检查。

**验收**

- 双实例测试：实例 1 吊销 token 后，实例 2 立即拒绝。
- 双实例限流：总请求数超过全局阈值后，任意实例拒绝。
- Redis 断连行为符合设计并有告警。

### F-03 Inbox 与业务副作用不是同一事务

**证据**

- `internal/runtime/inbox.go:57-69` 先独立写入 Inbox，再调用 handler，成功后另一个事务 `markProcessed`。
- `internal/runtime/inbox.go:72-103` 中 `markReceived`、handler、`markProcessed` 分属不同事务。
- `internal/runtime/inbox.go:153-170` 的 `DueRetryTasks` 对 retry 行执行 `SELECT ... FOR UPDATE SKIP LOCKED`，但查询不在显式事务中；返回的 Event 只有 `EventID`，缺少 type、aggregate、version、attempt。
- `internal/runtime/relay.go:98-116` 发布失败直接置 `failed`，没有 attempt、退避或重新入队；`RunOnce` 忽略 `PublishOne` 错误。
- `internal/content/service.go:676-710` 的文章通知 fanout 是先 COUNT 再逐条插入；`migrations/00008_content.sql:85-95` 的通知表没有 `(account_id, article_id, user_id)` 唯一键。

**影响**

- handler 成功后、`markProcessed` 前崩溃，重投后会再次执行副作用。
- 通知类副作用会产生重复，文章 fanout 还可能突破每日通知限制。
- `failed` 是无重试终态，网络抖动会导致事件永久丢失。
- retry 任务无法被安全使用，无法还原完整事件。

需要说明：`docs/specs/10-runtime.md:68-71` 明确选择 handler 自带事务、Inbox 单独标记，并依赖业务幂等。这不是实现走样，而是规格放松了合同要求。问题在于当前并非所有 handler 都幂等，因此该裁决不能成立。

**改进路径**

1. 优先方案：`Dispatch` 开启 MySQL 事务，在同一事务内插入或读取 Inbox 行并执行 handler 的数据库副作用，成功后一起标记 `processed`。
2. 如果保留两阶段方案，每个 handler 必须有数据库唯一键。文章通知增加 `(account_id, article_id, user_id)` 唯一键；点赞通知由点赞首次生效决定。
3. `async_retry_tasks` 保存完整事件信封和 queue；到期任务在事务中领取并更新状态，重投仍使用原 event_id。
4. Outbox 发布失败区分可重试与永久错误；`markProcessed`、`markFailed` 检查 `RowsAffected`。

**验收**

- 在 handler 后、markProcessed 前注入崩溃，重投后副作用只有一次。
- 并发投递同一 event_id 100 次，业务效果只有一次。
- 网络失败进入 retry，超限进入 dead letter，人工重放仍幂等。

### F-04 消息发送鉴权和媒体校验在事务外

**证据**

- `internal/message/service.go:122` 在事务外调用 `precheckSend`。
- `internal/message/service.go:204-298` 使用 `s.db` 做好友权限、群禁言、成员资格、媒体归属和名片校验。
- `internal/message/service.go:143-158` 进入事务后只锁定会话行并复核会话类型。
- 合同 `微信核心社交仿制项目最终需求分析.md:277` 要求“服务端事务内校验会话成员、关系权限、群禁言、客服会话状态、媒体归属和消息限制”。

**影响**

并发时序示例：用户 A 发送消息的预检查读到仍是好友、未被禁言；同时对方拉黑、删除好友，或管理员将 A 移出群、设置禁言；A 的事务随后锁会话、写消息并提交。由于锁内没有重新鉴权，不该产生的消息仍会成为系统事实。媒体对象状态同理。

**改进路径**

1. 为 `contact`、`group`、`media`、`content` 提供 `Tx` 版本校验接口，消息发送事务内调用。
2. 按既有锁序锁定好友 epoch/设置、群行和成员行、会话行，避免死锁。
3. 媒体归属和状态在同一事务读取，消息资产与 `media_references` 同事务写入。
4. 事务外预检查只能作为快速失败优化，不能作为最终授权依据。

**验收**

- 发送与拉黑、删除好友、移出群、禁言、媒体清理并发执行，最终只允许一种一致结果。
- 事务提交前权限变化必须导致消息事务回滚。

## 4. P1 问题

### F-05 核心读路径 N+1

**证据**

- `internal/message/service.go:410` 的 `History` 拉取消息后逐条调用 `canRead`。
- `internal/message/service.go:471` 每条消息查询一次会话类型；群消息还会查询群和成员历史。
- `internal/message/service.go:492` 每条消息单独加载引用和资产。
- `internal/message/service.go:693` 会话列表没有分页，并对每个会话循环查询类型、标题、最后消息和未读数。
- `internal/moment/service.go:278-293` Feed 只取 `limit*4`，再逐条做可见性判断；`internal/moment/service.go:238-260` 每次判断包含设置、快照、好友 epoch 多次查询。
- 合同 `微信核心社交仿制项目最终需求分析.md:1226-1228` 明确 P95 目标。

**影响**

一页 50 条消息可能触发 150 到 250 次数据库往返。高并发下连接池、CPU 和网络往返会被耗尽。朋友圈 Feed 在权限命中率低时会漏页：如果 80 个候选都不可见，即使后面还有可见动态，也不会继续扫描。

**改进路径**

1. 会话类型和群信息每页查询一次，不能放在每条消息循环里。
2. 成员资格、好友权限、群成员时间区间下沉到 SQL JOIN，或先做批量权限判定。
3. 引用和资产用 `message_id IN (...)` 批量加载后在内存组装。
4. 会话列表改为 keyset 分页，并用批量 SQL 汇总最后消息和未读数。
5. Feed 循环拉取候选直到满足页大小或到达边界，同时设置最大扫描数；权限数据批量获取。

**验收**

- 单次历史请求 SQL 次数为常数或低于明确上限。
- 混合权限数据下翻页不漏动态。
- 压测达到 P95 目标并输出 DB 池等待、SQL 次数和延迟分布。

### F-06 Worker 单 goroutine 串行执行全部任务

**证据**

- `cmd/worker/main.go:77-123` 只有一个 goroutine 和一个 ticker。
- `internal/message/service.go:811-831` 的 `SweepExpired` 在一次调用中循环处理所有到期消息。
- `internal/moment/service.go:620-648` 的 `PublishDueSchedules` 同样循环处理所有到期任务。
- `cmd/worker/main.go:110` 租约 owner 固定为 `worker-1`。
- 任务没有独立超时，只有进程级 context 取消。

**影响**

消息过期积压 100 万条时，`SweepExpired` 可能长期占用 Worker，群待办、上传过期、备份过期、定时朋友圈全部延迟。一次慢 SQL 或慢存储删除会阻塞整个 Worker。多实例部署也缺乏统一 worker ID 和租约语义。

**改进路径**

1. 每类任务独立 goroutine，使用有界并发。
2. 每个 tick 只处理固定批次，例如消息过期每轮最多 5000 条。
3. 每个任务设置独立超时和 panic recover。
4. Worker ID 使用实例唯一值；租约提交校验 owner 和 version。
5. 增加任务耗时、处理行数、失败率和最老积压年龄指标。

**验收**

- 消息过期任务挂起 5 分钟时，定时朋友圈和上传过期仍按周期执行。
- 双 Worker 压测不重复副作用。

### F-07 媒体上传、过期和 GC 生命周期有缺陷

**证据**

- `internal/media/service.go:150-222` 的 `CompleteUpload` 先读 open 状态，再在事务外执行 `verifyAssembly`；两个并发 complete 会各自组装一份完整对象。
- `internal/media/service.go:227-269` 会流式复制并计算整个文件；单文件上限 200MB。
- `internal/media/store.go:177-189` 的 `closeSession` 无条件更新状态，不要求当前状态是 `open`。
- `internal/media/service.go:282-306` 过期清理先查询 stale 再调用 `closeSession`，可能把刚完成的上传改成 expired。
- `internal/media/service.go:400-404` 的 `OnReferencesRemoved` 是 no-op。
- `internal/message/service.go:849-869` 先删除 `message_assets` 并提交，再调用媒体 GC；失败或崩溃后不再能拿到 object id。
- 消息发送只写 `message_assets`，没有写 `media_references`，见 `internal/message/service.go:179-190`。

**影响**

并发 complete 重复消耗 200MB 级 I/O、CPU 和临时存储；complete 与 expire 竞争会覆盖状态；消息过期后媒体引用清理不可恢复，物理对象无法安全 GC，形成存储泄漏；进程在 DB 提交后、删除 chunk 前崩溃会留下孤儿 chunk。

**改进路径**

1. `CompleteUpload` 先用短事务把会话从 `open` 抢占为 `assembling` 并写租约，租约内组装，完成事务转 `completed`；过期清理不得修改非 open 状态。
2. 组装失败或退出时释放租约；租约过期后其他 Worker 可重试。
3. 消息过期事务写 durable 的 `media_gc` 任务，后续 Worker 按引用计数决定是否物理删除。
4. 消息和朋友圈创建时同事务写 `media_references`。
5. 本地存储写对象改为临时文件加原子 rename，并考虑 fsync；对象删除走延迟回收。

**验收**

- 50 个并发 complete 只组装一次，其余返回同一 object id。
- complete 与 expire 并发后状态只能为 completed 或 expired 二选一。
- 消息过期后 GC 任务可恢复，只有无引用对象被物理删除。

### F-08 群模块吞掉数据库错误

**证据**

- `internal/group/service.go:324`、`:367` 忽略 `bumpMemberCount` 错误。
- `internal/group/service.go:325`、`:287`、`:368` 等位置忽略群事件写入错误。
- `internal/group/service.go:326`、`:369`、`:76`、`:178`、`:228` 等位置忽略系统消息写入错误。
- `internal/message/service.go:604-656` Pin 先 count 再 insert，没有锁定 conversation。
- `internal/message/store.go:316-335` count 与 insert 是两个语句。

**影响**

数据库错误时事务仍可能提交，产生成员数不一致、缺系统消息和审计事件的半成功状态。两个管理员在 19 个置顶时并发置顶第 20、21 条，count 都读到 19，最终可能超过 20 条上限。

**改进路径**

1. 所有同事务必需副作用必须返回错误并让事务回滚，`_ =` 只允许用于明确可丢弃的日志类操作。
2. Pin 前锁定 conversation 行，或维护每会话 pin 计数表并用条件更新。
3. 增加失败注入测试：系统消息或计数失败时业务状态必须回滚。

**验收**

- 注入 `bumpMemberCount` 失败后，成员退出事务整体失败。
- 并发 100 次 Pin，置顶数不超过 20。

### F-09 定时朋友圈可能重复发布

**证据**

- `internal/moment/store.go:413-448` claim 会递增 `execution_version` 并写 lease owner。
- `internal/moment/store.go:491-497` `markSchedulePublished` 只按 `id` 和 `status='scheduled'` 更新，不校验 `execution_version`、lease owner 或 lease until。
- `internal/moment/service.go:649-678` 先插入 moment，再调用 `markSchedulePublished`；函数不检查 `RowsAffected`。

**影响**

Worker A 领取后慢于租约 TTL，Worker B 重新领取，两者都会插入 moment。A 先提交会把 schedule 置为 published；B 随后插入第二个 moment，更新 schedule 时影响 0 行但错误被忽略，事务仍提交。结果是同一 schedule 产生两条动态。

**改进路径**

1. 同一事务内先执行条件更新：`WHERE id=? AND status='scheduled' AND execution_version=? AND lease_owner=?`。
2. 检查 `RowsAffected`；不是当前租约就回滚，不插入 moment。
3. 可在 moments 表增加唯一 `schedule_id` 作为兜底，但租约冲突仍应显式返回。

**验收**

- 模拟租约超时和双 Worker 竞争，只生成一个 moment，输者回滚或返回当前已发布结果。

### F-10 WebSocket 未形成可用网关

**证据**

- `internal/ws/hub.go:2` 明确标注单进程、多实例是 V1 约束。
- `internal/ws/hub.go:74-83` 只有进程内 map 遍历和慢消费者关闭。
- `cmd/api/main.go` 没有挂载 WebSocket 路由；项目没有 `cmd/gateway`。
- 合同 `微信核心社交仿制项目最终需求分析.md:1225` 要求 10,000 并发 WebSocket。

**影响**

无法完成连接认证、心跳、ACK、断线同步等核心链路。多 API 实例下，事件发布到实例 1 时，连接在实例 2 的设备收不到。

**改进路径**

1. 先实现单实例 Gateway：鉴权、心跳、发送队列、ACK、断线提示。
2. 多实例时通过 Redis Pub/Sub 或 Rabbit fanout 将 `message.push` 路由到持有连接的实例；Redis 只做路由，不做事实源。
3. 增加连接数、慢消费者断开、ACK 延迟、推送失败指标。

**验收**

- 10k 连接压测下内存和 goroutine 有界。
- 双实例下设备连在任一实例都能收到属于自己的消息。
- ACK 前进程崩溃后重连可按 seq 补齐。

### F-11 可观测性和健康检查不足

**证据**

- `cmd/api/main.go:330-334` 的 `/readyz` 只返回 ready，不做 DB 或依赖探测。
- 代码中没有 metrics endpoint、HTTP 延迟、DB pool、Outbox 年龄、队列深度、Worker 任务耗时实现；`docs/specs/10-runtime.md:124-140` 只有规格。
- `cmd/worker/main.go` 没有 health 端点。
- `internal/operator/service.go:155-170` 只有三个 COUNT，无法定位积压原因和最老事件年龄。

**影响**

MySQL 故障时 API 仍可能被负载均衡视为 ready，继续接收请求并失败。无法判断 Outbox 是发布慢、消费者失败还是死信增长。Worker 卡死没有外部信号。

**改进路径**

1. `/readyz` 检查 MySQL ping、缓存驱动 ping 和必要依赖；优雅关停时主动失败 ready。
2. API 与 Worker 暴露 metrics：HTTP 延迟、DB pool、Outbox pending/oldest age、Relay 失败、消费延迟、retry/dead letter、Worker 任务耗时。
3. Worker 增加 health 端点和心跳时间戳。

**验收**

- 停止 MySQL 后 `/readyz` 在一个探测周期内变 503，LB 摘流。
- 人为制造 Outbox 积压，告警在 1 分钟内触发。

### F-12 公网限流信任 X-Forwarded-For

**证据**

- `internal/platform/httpx/response.go:78-82` 直接返回整个 `X-Forwarded-For` 头。
- `cmd/api/main.go:280-301` 公共限流使用该值作为 subject。
- `internal/platform/ratelimit/ratelimit.go:27-36` 是固定窗口限流。

**影响**

服务直接暴露公网时，攻击者每个请求伪造不同 XFF，登录、注册、公共下载代理的 IP 限流失效。即使经过代理，当前也没有可信跳数解析，可能把客户端伪造链路计入 key。

**改进路径**

1. 增加 `WECHAT_TRUSTED_PROXIES`；非可信代理只用 `RemoteAddr`。
2. 信任代理时按可信链从右向左取第一个非可信 IP，不能使用整串 header。
3. 认证后接口优先按 user id 限流；公共接口可叠加全局与 IP 两级限流。

**验收**

- 直连场景伪造 1000 个 XFF 仍按真实 IP 限流。
- 经可信代理场景取到真实客户端 IP。

## 5. P2 问题

### F-13 边界检查脚本假阳性

**证据**

- `scripts/check-boundaries.sh:15` 调用 `go list`。
- 当前 Windows/WSL 环境执行输出 `go: command not found` 后，脚本仍输出 `boundary check OK` 并返回 0。
- 原因是 `go list` 的错误输出被 `2>/dev/null` 吞掉，循环未检查命令失败。

**影响**

CI 或本地缺少 Go 时会误报边界检查通过，违反提交物检查单。

**改进路径**

1. 先执行 `command -v go`，不存在直接失败。
2. 对每次 `go list` 检查退出码，不能吞错。
3. Windows CI 使用 PowerShell 或 Git Bash 原生 Go 路径，避免依赖 WSL 环境变量。

## 6. 其他风险

1. **备份是同步 HTTP 加空数据源**：`internal/backup/service.go:43-86` 在请求内同步导出，`cmd/api/main.go:212-231` 的 stub 返回空 JSON。10GB 级备份应改为异步任务、分片导出、可恢复 part 表和进度查询。
2. **朋友圈点赞通知不幂等**：`internal/moment/service.go:354-378` 中点赞行 `INSERT IGNORE` 幂等，但通知每次插入。应只有首次点赞生效时写通知，或给通知表增加幂等键。
3. **优雅关停缺少结果观测**：`cmd/api/main.go:147-150` 忽略 `Shutdown` 错误，请求未在超时内 drain 时没有日志、指标或摘流信号。

## 7. 不判定为问题的设计

1. 模块化单体本身不是问题。当前规模下，一个 Go module 加多进程拓扑比微服务更合适。
2. MySQL Outbox 作为事实源是正确方向。问题在 Relay 和 Inbox 未闭环，而不是模式选错。
3. 会话内连续 seq 需要某种串行化。`conversations` 行锁可以接受，但不能把权限检查、payload 解码和大 I/O 放在锁内；热点群需要专门压测。
4. 固定窗口限流可以作为 V1 选择。真正问题是进程内存驱动、XFF 信任和 fail-open 缺乏观测，而不是没有滑动窗口。

## 8. 建议整改顺序

### 第一阶段：恢复事实一致性

1. 修复 F-02：未实现驱动 fail fast。
2. 修复 F-04：消息发送事务内重新鉴权。
3. 修复 F-08：所有必需事务副作用返回错误。
4. 修复 F-09：租约版本条件提交。
5. 修复 F-13：边界脚本假阳性。

### 第二阶段：打通异步链路

1. 实现 none 模式本地路由和完整消费者接线。
2. 重做 Inbox 事务边界与 retry/dead letter 状态机。
3. 实现媒体引用和 durable GC 任务。
4. 接入 Redis、RabbitMQ、S3 并做真实集成测试。

### 第三阶段：达到容量和 HA 验收

1. 批量化消息历史、会话列表、朋友圈 Feed 查询。
2. Worker 任务隔离、限批、独立超时和多实例安全。
3. 实现 WebSocket Gateway、ACK 和跨实例分发。
4. 补齐 metrics、readyz、告警和负载测试。

## 9. 本次验证

已执行：

```powershell
go test ./...
go vet ./...
bash scripts/check-boundaries.sh
```

结果：

- `go test ./...` 通过。
- `go vet ./...` 通过。
- `scripts/check-boundaries.sh` 输出 `go: command not found` 后仍返回 OK，不能计为通过。

未执行：

- 未运行带 MySQL 的 integration 测试。
- 未运行 10,000 WebSocket 连接和消息 P95 压测。
- 未验证 Redis/RabbitMQ/S3 生产驱动，因为代码尚未实现。

## 10. 最终意见

这个项目的领域建模和文档治理明显高于普通原型，好友 epoch、Outbox、租约、模块边界等方向是对的。但当前代码与文档存在明显落差：文档宣称 runtime 完成，实际 Worker 没有 Relay 和消费者；配置宣称支持生产驱动，实际会静默降级或启动失败；多个核心事务把必需副作用错误吞掉。

建议停止横向扩功能，先修复 P0，再用真实 MySQL、Redis、RabbitMQ、S3 和双实例部署做端到端故障注入与容量验收。否则新增功能只会继续扩大不可验收面。
