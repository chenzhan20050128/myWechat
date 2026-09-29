# SPEC-00 platform 基础设施层

> 依据：合同 §12/§15/§16；ADR-002/003/005/006/007/009。状态：已评审。

## 1. 范围

**做**：配置解析、结构化日志、错误模型、HTTP 响应/中间件（request_id、authn、恢复 panic）、MySQL 连接与事务助手、缓存接口（redis/memory）、对象存储接口（s3/local）、MQ 发布接口（rabbitmq/none）、UUID/随机令牌生成、时间助手。
**不做**：任何领域规则；不暴露给 handler 直接拼 SQL 的能力。

## 2. 规则

### 2.1 config
- R1：全部配置来自环境变量 `WECHAT_*`；`Load()` 返回强类型 `Config` 并在启动时校验（DSN 必填、驱动枚举合法、TTL>0）。
- R2：驱动枚举：`CACHE_DRIVER ∈ {redis, memory}`，`STORAGE_DRIVER ∈ {s3, local}`，`MQ_DRIVER ∈ {rabbitmq, none}`。未知值启动即失败。
- R3：dev 默认：HTTP `:8080`、memory/local/none 三驱动、`./data/objects` 本地存储目录；MySQL DSN 无默认。

### 2.2 errors
- R4：`AppError{Code, Message, HTTPStatus, internal cause}`；`errors.As` 可提取。
- R5：码表（HTTP 映射固定）：`OK(200)`、`UNAUTHENTICATED(401)`、`FORBIDDEN(403)`、`RESOURCE_UNAVAILABLE(404)`、`INVALID_ARGUMENT(400)`、`STATE_CONFLICT(409)`、`QUOTA_EXCEEDED(413)`、`SYNC_CURSOR_EXPIRED(409)`、`INVALID_CREDENTIALS(401)`、`ALREADY_FRIEND(409)`、`CONTENT_UNAVAILABLE(410)`、`RATE_LIMITED(429)`、`INTERNAL_ERROR(500)`。
- R6：未包装错误一律映射 `INTERNAL_ERROR`，日志记堆栈，响应不泄露内部细节。

### 2.3 httpx
- R7：统一响应 `{"code","message","data","request_id"}`；`request_id` 中间件优先生成 UUID 并写入 context 与响应头 `X-Request-Id`。
- R8：authn 中间件执行 ADR-003 校验链；通过后在 context 写入 `Principal{UserID, DeviceID, SessionID}`。
- R9：panic 恢复中间件返回 `INTERNAL_ERROR` 并记日志。
- R10：游标编解码：`base64url(json)`，解码失败返回 `INVALID_ARGUMENT`，不泄露内部结构。

### 2.4 mysqlx
- R11：DSN 强制参数 `parseTime=true&time_zone=%27%2B00%3A00%27&tx_isolation=%27READ-COMMITTED%27&charset=utf8mb4&collation=utf8mb4_bin`（在 Open 时校验/补齐）。
- R12：`WithinTx(ctx, fn)` 提交/回滚；死锁(1213)与锁超时(1205)自动重试至多 3 次（同幂等键由调用方保证安全）。
- R13：所有查询参数化；禁止字符串拼接 SQL（含 ORDER BY 白名单化）。

### 2.5 cache
- R14：接口：`Get/Set/Del/Incr/Expire/SetNX`，全部带 ctx 与 TTL 语义；memory 驱动用带过期堆的并发 map，语义与 redis 一致（含 SetNX 原子性）。
- R15：cache 是性能层，不是事实层：任何 key 丢失不得破坏正确性。

### 2.6 storage
- R16：接口：`Put(ctx, key, reader, size, mime)`、`Get(ctx, key)`、`Delete(ctx, key)`、`SignGetURL(ctx, key, ttl)`（s3 预签名 / local 签发 HMAC 令牌走 `/media/download` 代理）、`Exists`。
- R17：Key 由调用方生成（`obj/<yyyy>/<mm>/<uuid>`），storage 不做权限判断；下载鉴权在 media 模块。
- R18：禁止公开桶/公开目录直读（合同 §8.3）。

### 2.7 mq
- R19：`Publisher.Publish(ctx, queue string, event Event) error`；`none` 驱动直接返回 nil（Worker 直读 outbox，合同 §12.5.2）。
- R20：Event 只含 `event_id/type/aggregate_id/version/attempt` 与少量引用，不放大媒体/快照（合同 §12.5.3）。

### 2.8 ids / time
- R21：`ids.New()`=UUIDv4；`ids.NewToken(n)`=n 字节加密随机 base64url；`ids.SHA256Hex(s)`。
- R22：`clock.Now()` 返回 UTC；测试可注入假时钟。

## 3. 验收标准

- A1：`go build ./...` 通过；memory/local/none 驱动下 API 进程可启动（仅需 MySQL）。
- A2：authn 中间件对缺失/伪造/过期/已吊销令牌分别返回 `UNAUTHENTICATED`。
- A3：cache memory 驱动 SetNX 并发 100 goroutine 仅 1 个成功（单测）。
- A4：storage local 驱动 Put→SignGetURL→代理下载字节一致；篡改签名返回 403（单测）。
