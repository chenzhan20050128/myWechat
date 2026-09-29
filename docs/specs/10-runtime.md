# SPEC-10 runtime（Outbox Relay、Inbox、重试/死信、Scheduler、Worker、可观测性）

> 依据：合同 §12.5（全部）、§14.5、§16.1、§17；ADR-012、ADR-002；平台既有 `internal/platform/outbox.Emit`、`mq`、迁移 0001 的 `outbox_events`/`inbox_events`/`async_retry_tasks`/`async_dead_letters` 表。
> 读者：低等级 LLM 执行者。**全部技术判断已写死；未覆盖情况 → 停下记录 PROGRESS.md。**
> 本模块是唯一"跨域协调层"：不实现任何业务副作用，只调用各域暴露的 Worker 接入函数（SPEC-04/05/06/07/08/09 的"任务分解"最后一步均已预留函数名）。

## 1. 范围

**做**：Outbox Relay（租约批量领取→发布→confirm→published）、RabbitMQ 适配器（topic exchange `wechat.events` + 7 类固定队列 + DLQ + publisher confirm + 手动 ACK）、Inbox 幂等消费框架、失败分类（永久/临时）、重试阶梯（1m/5m/30m，≤5 次）、死信与受控重放、Scheduler（扫到期记录投递）、Worker 进程（cmd/worker：全部消费者+lifecycle 批任务）、降级（none 驱动直投）、监控指标。
**不做**：RabbitMQ 延迟插件/TTL 重试队列、Kafka/NATS/Redis Streams（合同禁止）、事件回放平台。

## 2. 队列拓扑（合同 §12.5.3，唯一真源）

```
exchange: wechat.events (topic, durable)
  routing key = 事件类型（= outbox_events.event_type，如 message.stored）

queue（durable）                    binding key                消费者（cmd/worker 内）
wechat.message.push                message.*                  ws 推送（Deliver→Hub）
wechat.notification                moment.notified|todo.*     通知落库/推送
wechat.media.process               media.*                    缩略图/转码（media.Process）
wechat.lifecycle                   *.expired|upload.*        过期批任务触发
wechat.scheduler                   schedule.*                定时朋友圈/待办到期
wechat.content.service             official.*                文章通知扇出/客服通知
wechat.dlq.<queue>（7 个）          死信                       仅运营查看
```
（绑定键以实现时 `internal/platform/mq` 的 queue 常量为准；`none` 驱动不建队列，Worker 直接函数调用。）

## 3. 事件信封（RabbitMQ 消息体，小而全——合同"不复制大快照"）

```json
{"event_id":"uuid","event_type":"message.stored","aggregate_id":"456",
 "version":1,"attempt_count":0,"occurred_at":"...","payload_ref":{"table":"messages","id":456}}
```
消费者收到后**回 MySQL 读业务数据**，不信 payload（除信封字段）。

## 4. 组件与规则

### 4.1 Outbox Relay（合同 §12.5.4 七步，逐条实现）
- R1 领取（事务A）：
  ```sql
  BEGIN;
  SELECT id FROM outbox_events
   WHERE status='pending' OR (status='publishing' AND lease_until < NOW(6))
   ORDER BY id LIMIT 100 FOR UPDATE SKIP LOCKED;
  UPDATE outbox_events SET status='publishing', lease_owner=?, lease_version=lease_version+1,
         lease_until=NOW(6)+INTERVAL 60 SECOND WHERE id IN (...);
  COMMIT;
  ```
- R2 发布：逐条（同聚合内保序：**同 aggregate_id 的事件串行发布**——领取后按 (aggregate_id, id) 分组，组内顺序 publish，组间并发 ≤8）持久消息 + publisher confirm + mandatory；channel 按 `aggregate_id % 8` 选路保组内有序。
- R3 确认后（事务B）：`UPDATE outbox_events SET status='published', published_at=? WHERE id=? AND lease_version=?`（乐观版本，防租约被抢后误标）。
- R4 崩溃窗口：未 confirm → 保持 pending/publishing 超租约被重领；已 confirm 未更新 → 重复发布，由消费端 Inbox 去重（合同明确允许）。
- R5 发布异常（broker 拒绝/unroutable）：`status='failed'` + last_error，**不停 Relay**（下轮跳过 failed，进入重试由 §4.3）。RabbitMQ 连接断开：Relay 暂停并指数退避重连（1s 起 ×2 封顶 30s），Outbox 积压（合同 §12.5.6 降级语义）。
- R6 `failed` 事件重试：入 async_retry_tasks（consumer_name='relay'），阶梯同 §4.3，超限进死信。
- R6a **Relay 节奏与背压**：空转时 200ms 轮询一次；连续空转 10 次退避至 2s（有新 outbox 写入时由 Emit 后可选的进程内通知立即唤醒——同进程场景）；领取到批次后**循环连续处理直到无新事件**（追赶积压）。单批 100 条、组间并发 8，避免单 Relay 打满 DB 连接池（Relay 使用的连接数上限 = 8 + 2）。多 Relay 实例靠 SKIP LOCKED 天然分摊。

### 4.2 Inbox 消费框架（合同 §12.5.5 六步）
```go
// internal/runtime/inbox.go
type Handler func(ctx context.Context, env Envelope) error      // 业务副作用（内部自带事务）
type Classifier func(err error) FailureClass                    // permanent | transient
Consume(queue, handler, classifier):
  1. 收到消息，不 ACK
  2. 事务：INSERT INTO inbox_events (consumer_name, event_id, status='processing', first_received_at)
        —— 撞唯一键 → 读现有行：
           processed  → 直接 ACK 丢弃（重复事件）
           processing → 该消费者先占（单消费者进程内串行，直接继续）
  3. handler(ctx, env) 执行业务副作用（handler 内部事务与 Inbox 标记同一事务？——
     裁决：handler 自带业务事务；框架在业务事务成功后单独事务标 processed。两事务间崩溃
     → 重投 → Inbox 已 processing → 重新执行 handler → 业务幂等（各域 R 规则）兜底。
     （合同允许：重复投递必须由消费者安全变成一次业务效果——即业务幂等，非框架原子性））
  4. transient 失败：事务写 async_retry_tasks(event_id, consumer_name, attempt+1,
        next_attempt_at=now+阶梯, last_error)；成功后 ACK
  5. permanent 失败：inbox status='failed' + 失败原因；ACK
  6. attempt>5：inbox status='dead'；写 async_dead_letters；ACK
```
- R7 阶梯：`next_attempt_at = now + [1m, 5m, 30m][(attempt-1) % 3]`；attempt 计数 ≤5。
- R8 重复事件：Inbox 唯一键 `(consumer_name, event_id)` 去重（迁移 0001 已建）。

### 4.3 Scheduler（合同：所有时间以 MySQL 为准）
- R9 单循环（每 10s tick，SKIP LOCKED 领取）扫四类到期记录并投递：
  - `async_retry_tasks WHERE status='retrying' AND next_attempt_at<=now` → 重新发布**原 event_id** 到原队列；
  - `moment_schedules WHERE status='scheduled' AND run_at<=now` → 直接调 moment.PublishDueSchedules（**裁决：不绕队列，Scheduler 进程内直接执行抢占函数**——§14.2 要求短事务抢占，队列化无增益）；
  - `group_todos` 逾期 → group.ScanOverdueTodos（同上直调）；
  - 租约回收：moment.RecoverScheduleLeases / backup.ReapZombieRestoreJobs。
- R10 Scheduler 与 Worker 同进程（cmd/worker 内 goroutine），多实例安全（SKIP LOCKED + 条件更新）。

### 4.4 Lifecycle 批任务（合同 §14.4 + §8.4 + §10.2，全部 Worker 内定时循环）
| 任务 | 周期 | 函数（各域提供） |
|---|---|---|
| 消息过期 | 1min | message.ExpireMessages（SPEC-04 R26，批 1000 SKIP LOCKED） |
| 备份过期 | 1h | backup.ExpireBackups（SPEC-08 R6） |
| 上传会话过期 | 10min | media.ExpireUploadSessions（24h 未完成，SPEC-03） |
| 媒体 GC | 1h | media.PurgeGCQueue（7 天回收站物理删除，SPEC-07 R15） |
| 禁言清理 | 1d | group.CleanupExpiredMutes（SPEC-05 R16） |
| 孤儿收藏副本清理 | 1d | media 清 fav/ 前缀无引用对象（SPEC-07 R3 裁决） |
- R11 所有批任务幂等、可重入、SKIP LOCKED，多 Worker 实例安全。

### 4.5 none 驱动直投（开发降级）
- R12 `MQ_DRIVER=none` 时：Relay 领取 outbox 后**进程内直接路由**：按 event_type 前缀映射到本地 handler 表（与队列绑定一致），同步调用（带 5s 超时），失败走同一重试阶梯。**语义与 RabbitMQ 版一致**（至少一次+幂等），仅无跨进程分发。**部署拓扑裁决（避免双 Relay 争抢与空转 Hub）**：none 模式下**只部署 cmd/api 单进程**（启动参数 `--embed-worker --embed-gateway`，none 驱动时二者默认 true）——Relay/Scheduler/Lifecycle/WS Hub 全部内嵌；**cmd/worker 仅在 rabbit 模式下部署**。若 none 模式下误起 cmd/worker，SKIP LOCKED 保证不重复消费，但会分走事件导致内嵌 Hub 收不到推送（文档注明，不做运行时互斥）。

### 4.6 Worker 接线总表（cmd/worker/main.go 组装顺序）
```
1. 加载 config → 建 DB/Cache/Storage/MQ 驱动
2. 实例化各域 service（按依赖序：conversation→group→message→moment→favorite→backup→content）
3. 注册 handler：
   wechat.message.push   → wsHub.Deliver
   wechat.notification   → moment/todo 通知落库+ws 推
   wechat.media.process  → media.Process(object_id)
   wechat.content.service→ content.FanoutArticleNotifications
   wechat.lifecycle      → 触发对应批任务（也可由定时器直接跑，裁决：定时器直接跑，此队列仅承接事件触发型清理）
   wechat.scheduler      → 空（Scheduler 直调，见 R9）
4. 启动：Relay、Scheduler、Lifecycle 定时器、（可选）内嵌 ws Hub
```

## 5. 可观测性（合同 §17、§12.5.6）——三大支柱全覆盖

### 5.1 指标（metrics）
- R13 `internal/platform/metrics`：轻量 Prometheus 文本格式暴露（自研 counter/gauge/histogram，**不引第三方依赖**），cmd/api 与 cmd/worker 各暴露 `:9090/metrics`。
- **HTTP 层（httpx 中间件统一埋点，业务代码零侵入）**：
  - `http_requests_total{route,method,code}`（counter）——route 用 chi 路由模板（非原始路径，防标签爆炸）
  - `http_request_duration_seconds{route,method}`（histogram，buckets 5ms..5s）
  - `http_requests_in_flight`（gauge）
- **业务关键路径**：
  - `message_send_duration_seconds`（histogram）、`message_send_total{result}`（result=stored|rejected|duplicate）
  - `ws_connections`（gauge，当前连接数）、`ws_connections_total`、`ws_slow_consumer_disconnects_total`、`ws_push_queue_depth`（gauge）
  - `auth_login_total{result}`（success|bad_password|frozen|locked）
  - `db_pool_in_use` / `db_pool_wait_count` / `db_pool_wait_duration_seconds`（database/sql 自带 Stats()）
- **异步链路**：
  - `outbox_pending{}`（gauge，SELECT COUNT 每 30s）、`outbox_oldest_age_seconds`、`outbox_published_total`
  - `relay_publish_failures_total`、`queue_depth{queue}`（RabbitMQ 管理 API 可选，none 模式跳过）
  - `consumer_processed_total{queue}`、`consumer_failures_total{queue,class}`、`consumer_processed_duration_seconds{queue}`、`retry_pending{}`、`dead_letters_total{queue}`
  - `lifecycle_task_duration_seconds{task}`、`lifecycle_task_rows{task}`（每轮处理行数）

### 5.2 日志（logging）
- R15 结构化 JSON（platform/logger）：每条 HTTP 请求一行访问日志（request_id、route、状态码、耗时、userID 若已鉴权——**不记录 /ws query string 与任何 token/密码**）；Relay/Consume/批任务每事件含 event_id、queue、attempt、耗时、error。**慢日志**：处理耗时 >1s 的请求/事件打 WARN + `slow=true`。错误日志统一携带 request_id/event_id 便于串联。
- 请求链路：httpx.RequestID 中间件已生成 request_id 并写入响应头 `X-Request-ID` 与日志（既有）；Relay 投递时把 outbox 事件 id 放入信封，消费日志全程携带。

### 5.3 告警阈值与健康检查
- R14 告警规则（以 metrics 表达，文档化；不建独立告警系统，交给部署侧 Prometheus/Grafana 规则）：队列消息年龄 >5min、outbox 未发布 >1min、死信增长（rate>0 持续 5min）、消息发送 P95 >300ms 持续 5min、错误率 >1%、DB 池等待 >100ms P95、WS 断连率突增。
- R16 健康检查：`GET /healthz`（cmd/api：DB ping + 依赖驱动 ping，200/503）；`GET /readyz`（启动完成后 200，优雅关停期间 503——供 LB 摘流）；cmd/worker 暴露 `:9091/healthz`。

## 6. 死信与重放（合同 §12.5.5）
- R17 死信行含：event_id、consumer_name、原信封 JSON、last_error、dead_at。
- R18 重放工具（运营）：`POST /admin/dead-letters/{id}/replay`（operator 鉴权）→ 生成新投递（原 event_id，attempt 清零）→ 消费端 Inbox 规则不变。重放前人工修复失败原因（运营流程，不在代码内校验）。

## 7. 测试用例

| ID | 用例 | 断言 |
|---|---|---|
| T1 | Relay 领取→发布→confirm→published | 状态机完整 |
| T2 | 发布后崩溃（confirm 前杀进程模拟） | 事件可重领；重复发布被 Inbox 去重 |
| T3 | 租约超时抢占 | 第二个 Relay 领走 |
| T4 | 乐观版本：旧 Relay 迟到 confirm | 不误标 published |
| T5 | 同 aggregate 保序 | 组内 id 顺序投递 |
| T6 | 重复 event_id 消费 | 业务副作用恰一次（stub 计数） |
| T7 | transient 失败 | 入 retry，1m 后重投（FakeClock） |
| T8 | 5 次后 | dead + async_dead_letters 行 + ACK |
| T9 | permanent 失败 | 立即 failed，不重试 |
| T10 | none 驱动直投 | 与 MQ 版同语义（同一 handler 表） |
| T11 | Scheduler 到期扫描 | 四类各触发一次，重复 tick 幂等 |
| T12 | RabbitMQ 断连 | Relay 退避重连，Outbox 积压，恢复后续发（需要真 RabbitMQ，标 integration） |
| T13 | 指标端点 | /metrics 输出上述指标名 |

## 8. 任务分解

1. `internal/platform/mq`：RabbitMQ 适配器（streadway/amqp 或 rabbitmq/amqp091-go，goproxy.cn 可达）——连接管理/重连、exchange+队列+DLQ 声明、Publish(confirm)、Consume(手动 ACK)；none 驱动补 `LocalRouter`。+ 单测（none 路径全测；RabbitMQ 路径 integration tag）。
2. `internal/runtime/relay.go`（R1-R6）+ 单测 T1-T5（fake publisher）。
3. `internal/runtime/inbox.go`（R7-R8）+ 单测 T6-T9。
4. `internal/runtime/scheduler.go`（R9-R10）+ 单测 T11。
5. `internal/runtime/lifecycle.go`（R11 接线表）。
6. `cmd/worker/main.go`（§4.6 组装）+ `--embed-worker`/`--embed-gateway` 开关。
7. `internal/platform/metrics`（R13-R16，含 httpx 请求埋点中间件、database/sql Stats 采集）+ cmd/api、cmd/worker 挂载 + T13。
8. 死信重放 admin 接口（R17-R18，operator 鉴权——阶段六前用 SPEC-09 的 IsOperator 配置端口）。
9. 边界脚本 DOMAINS 加入 runtime（runtime 只 import platform + 各域 service 接口，不得 import 各域 store）。

## 9. 验收

Outbox 事实层与 RabbitMQ 分发层职责分离可验证；重复投递业务恰一次；重试阶梯 1/5/30m 且 ≤5 次；死信可运营重放；降级模式语义一致；所有到期时间以 MySQL 为准；监控指标齐备。
