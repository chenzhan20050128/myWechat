# 压测报告（2026-10-01）

## 环境

- 被测系统：`cmd/api` 单实例，HTTP + 标准库路由。
- 数据库：本机 MySQL `127.0.0.1:3307 / wechat_dev`。
- 依赖：`cache=memory`、`storage=local`、`mq=none`；未接入 Redis、RabbitMQ、S3。
- 负载机：与 API 同一台 Windows 主机；`64` 并发闭环 worker。
- 流量：50 个虚拟账号、每账号 4 个好友、每会话 20 条文本、每账号 10 条朋友圈；请求按账号轮转。
- 结论口径：这是单机开发原型基线，不能外推为生产容量。

## 结果

### 限流关闭 / 服务端能力观察

配置为 `WECHAT_RATE_LIMIT_ENABLED=false`，目标 400 QPS，持续 60 秒。

- 实际总吞吐：约 `338.2 QPS`，共 `20,295` 次请求。
- 非限流错误：`22` 次，均为客户端超时（状态 0），约 `0.108%`。
- 无 4xx、无 429、无 5xx envelope。

| 场景 | QPS | P50 | P90 | P95 | P99 | 超时 |
|---|---:|---:|---:|---:|---:|---:|
| profile | 84.8 | 32.9ms | 143.5ms | 203.2ms | 313.7ms | 13 |
| history | 67.6 | 54.9ms | 172.8ms | 225.7ms | 345.7ms | 0 |
| conversations | 60.9 | 140.1ms | 312.6ms | 378.5ms | 516.2ms | 0 |
| feed | 40.5 | 116.4ms | 280.2ms | 338.1ms | 432.1ms | 3 |
| friends | 33.8 | 38.6ms | 163.9ms | 215.7ms | 311.4ms | 0 |
| login | 27.0 | 384.9ms | 600.2ms | 667.2ms | 873.7ms | 4 |
| send | 23.6 | 73.2ms | 207.7ms | 271.2ms | 382.2ms | 2 |

对照合同目标：消息发送 P95 `271.2ms < 300ms`，消息历史 P95 `225.7ms < 300ms`，朋友圈首屏 P95 `338.1ms < 500ms`。登录因 Argon2id 校验消耗 CPU，P95 约 `667ms`。

### 限流开启 / 200 QPS 校验

修复消息读/写限流分组后，目标 200 QPS，持续 10 秒，50 个虚拟账号。

- 实际总吞吐：约 `199.9 QPS`，共 `5,996` 次请求。
- 429：`0`。
- 非限流错误：`5` 次客户端超时。

说明：该轮仅校验 200 QPS 下限流配置不再误伤消息读/写路径，不作为服务端最大能力结果。

## 发现与修复

1. `DELETE /api/v1/moments/comments/{id}` 与 `DELETE /api/v1/moments/{id}/like` 在 Go `ServeMux` 下路由冲突，API 无法启动；已调整为 `DELETE /api/v1/moment-comments/{id}`。`DELETE /api/v1/moments/schedules/{id}` 同理调整为 `DELETE /api/v1/moment-schedules/{id}`。
2. 会话列表批量设置查询把 `[]any` 作为单个 SQL 参数展开，导致 `GET /conversations` 稳定 500；已修复为变参展开。
3. 会话列表对 `transfer` 会话错误调用“另一个直接成员”查询，导致注册后无消息账号访问会话列表返回 404；已按类型渲染文件传输助手标题。
4. 消息历史、会话列表和详情误用 100/min 写限流；已按规格归入 300/min 读限流，消息发送单独使用 120/min。
5. 新增 `WECHAT_RATE_LIMIT_ENABLED`，默认 `true`；仅显式设置 `false` 时为无限流模式。生产禁止关闭。

## 证据

- 完整 JSON：`.tmp/loadtest-report-norate-400qps.json`。
- 限流校验 JSON：`.tmp/loadtest-smoke-rate-afterfix.json`。
- 命令见 `docs/loadtest-plan.md`。
