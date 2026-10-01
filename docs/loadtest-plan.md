# 高并发接口压测方案

## 目标

验证当前单机开发原型在真实业务混合流量下的吞吐与延迟，重点覆盖技术评审中的热点接口：登录、鉴权读、好友列表、会话列表、消息历史、消息发送和朋友圈 Feed。压测结果用于判断当前实现的实测 QPS、P95 和错误率，不用于声称生产容量。

## 流量模型

数据集模拟一个活跃社交服务的小型切片：

- 50 个活跃账号，iOS / Android 客户端混合。
- 每个账号与 4 个邻居建立双向好友关系，并生成独立单聊会话。
- 每个会话预置 20 条文本消息，发送方交替出现。
- 每个账号发布 10 条 `all_friends` 朋友圈，内容模拟日常动态。
- 压测请求按账号轮转，避免单账号限流掩盖服务端容量。

混合流量权重与真实移动端短周期行为对齐：

| 场景 | 方法与路径 | 权重 | 说明 |
|---|---|---:|---|
| login | `POST /api/v1/auth/login` | 8% | Argon2id 校验、设备会话写库；虚拟公网 IP |
| profile | `GET /api/v1/users/me` | 25% | 鉴权缓存和 DB 回源热路径 |
| friends | `GET /api/v1/contacts/friends` | 10% | 关系列表读取 |
| conversations | `GET /api/v1/conversations` | 18% | 会话列表，评审标出的高热读接口 |
| history | `GET /api/v1/conversations/{id}/messages?limit=20` | 20% | 消息历史 |
| send | `POST /api/v1/conversations/{id}/messages` | 7% | 会话行锁、序列号、Outbox 写入 |
| feed | `GET /api/v1/moments/feed?limit=10` | 12% | 朋友圈首屏 |

单账号读限流为 300/min，写限流为 100/min。默认目标 QPS 200、50 个压测账号，可保证账号平均读速率低于限流，同时保留压力。登录请求每次使用不同虚拟公网 IP，模拟真实公网客户端分布，避免本地负载机单 IP 被 auth 限流误判。本机压测启动 API 时需将 `WECHAT_TRUSTED_PROXIES=::1`，这样负载机发出的代理头才会按可信代理处理；生产环境不能盲目信任该头。

限流默认必须开启。只在单独压测 API 且网络不可达公网时，设置 `WECHAT_RATE_LIMIT_ENABLED=false` 或脚本 `-DisableRateLimit` 关闭业务限流，用于观察服务端本身容量；此时结果不代表生产可用容量，必须单独标注。

## 运行方式

使用真实 MySQL，不清空业务表；每次压测生成带时间戳的独立账号与数据。建议使用 18080 避免与开发 API 冲突。

```powershell
$env:WECHAT_MYSQL_DSN='wechat:wechat-dev-pw@tcp(127.0.0.1:3307)/wechat_dev'
go run ./cmd/migrate up
$env:WECHAT_HTTP_ADDR=':8080'
$env:WECHAT_TRUSTED_PROXIES='::1'
$env:WECHAT_RATE_LIMIT_ENABLED='false'
go run ./cmd/api
go run ./cmd/loadtest -base 'http://[::1]:8080' -users 50 -concurrency 64 -duration 60s -target-rps 200 -report .tmp/loadtest-report.json
```

也可使用封装脚本：

```powershell
.\scripts\run-loadtest.ps1 -Users 50 -Concurrency 64 -DurationSeconds 60 -TargetRPS 200
```

## 判定

对比需求基线：

- 消息发送和历史查询 P95 < 300ms。
- 朋友圈首屏 P95 < 500ms。
- 非限流错误率 < 1%。
- 结果中单独列出 429，不与 5xx、超时混在一起。

当前环境没有 Redis、RabbitMQ、S3 和多实例；这是本地降级驱动和单机 API 的容量基线，不是生产高可用验收结果。
