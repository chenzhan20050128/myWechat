# 设计推敲记录（Design Review Log）

> 本文件记录对高风险设计点的逐项推敲。**每项必须写明：问题 → 候选方案 → 取舍理由 → 结论。**
> 依据来源说明：本环境无法访问互联网（WebSearch/WebFetch 均被网络策略阻断），以下引用的规范与模式（RFC 9700、tus 1.0/可断点上传、S3 Multipart、Transactional Outbox、keyset pagination）来自成熟公开知识，**未经在线复核**，结论以第一性原理推演为准。

---

## D1 访问令牌：opaque 单活令牌 vs JWT

**问题**：SPEC-01 最初设计把 `access_token_hash` 存在 `user_sessions` 行上，refresh 轮换时替换。审查发现一个 UX 边界：客户端提前 refresh（access 未过期就刷新）会立刻杀死仍在途的旧 access 请求。

**候选方案**：
- A. opaque 令牌 + session 行存哈希，refresh 时替换（单活令牌）。
- B. JWT(HS256) + 每请求查 session 吊销状态（多活令牌，旧 access 自然过期）。

**推敲**：
- 两者每次请求都要做一次吊销检查（cache → DB），性能等价；差异只在"同 session 能否多个有效 access 并存"。
- B 的收益场景是"提前 refresh + 大量并发在途请求"；本项目客户端形态（移动端单连接循环、401→refresh→重试的标准拦截器模式）几乎不触发该场景。
- B 的成本：签名密钥管理、JWT 库依赖、令牌调试工具链；A 全链路与 refresh 同构（都是哈希查表），认知负担最低。
- 关键约束（合同 §3.3"被退出设备立即失效"）：两者都靠"吊销即删 cache + DB 兜底"满足，无差异。

**结论**：选 A（维持原设计），并补两条契约：① API 规范明示"客户端收到 401 必须先 refresh 再重试一次"；② refresh 响应总是返回新 access。写死在 SPEC-01 §2.3。

---

## D2 Refresh 轮换与重放检测（对照 RFC 9700）

**问题**：refresh token 轮换后的重放如何处置？

**推敲**：OAuth 2.0 Security BCP（RFC 9700）推荐：refresh token 轮换 + **重放检测**——被轮换后的旧 token 再次出现，视为令牌族泄露信号，应吊销整个令牌族（本项目中 = 该设备会话）。SPEC-01 R16 已符合。

**补充决策**：
- 重放检测的窗口天然存在（攻击者与真实客户端竞争），我们选择**保守失败**（吊销会话，双方都要重新登录），这与 BCP 一致。
- 吊销时写审计 `auth.refresh.reuse_revoked`（含 IP），供运营侧发现异常。
- refresh 请求本身纳入限流（每 session 每分钟 10 次），防暴力猜测 48 字节随机数虽不可能，但防 DoS。

**结论**：R16 维持，补审计与限流两条到 SPEC-01。

---

## D3 分片上传：编号分片 vs 字节偏移（对照 tus / S3 Multipart）

**问题**：tus 1.0 用字节偏移（Upload-Offset）追加，S3 Multipart 用编号分片（part number）。选哪个？

**推敲**：
- 弱网移动端分片常**乱序到达、需并发上传**；字节偏移协议要求严格顺序追加（tus 的 PATCH 必须从当前 offset 开始），弱网下串行化每个分片，延迟叠加。
- 编号分片天然支持乱序与并发，且**与 S3 MultipartUpload API 一一对应**（part number ↔ UploadPart，完成时 CompleteMultipartUpload 校验清单）。这意味着未来"直传 S3"改造（客户端拿预签名 part URL 直传）时，我们的数据模型与协议**零迁移**。
- 服务端完成时重算全量 SHA-256 与逐片校验：合同 §8.3 要求服务端重新计算，编号分片在本地驱动下逐片落盘、完成时顺序读回拼接计算，与 S3 完成时校验每个 part 的 ETag 语义一致。

**结论**：维持编号分片（SPEC-03 R3/R4），并在 ADR-012 记录"分片协议向 S3 Multipart 演进路径"。

---

## D4 Outbox Relay 的并发与租约（对照 Transactional Outbox 模式）

**问题**：多 Relay/Worker 实例并发领取 outbox 事件，如何不丢不重？

**推敲**（Canonical Transactional Outbox，Chris Richardson 模式库）：
- 投递语义只能是 **at-least-once**；业务恰好一次 = 消费端 Inbox 唯一键去重（合同 §12.5.5 已定）。
- 领取语义：`SELECT ... WHERE status='pending' ... FOR UPDATE SKIP LOCKED LIMIT n` 批量行锁领取 → 置 `publishing` + 租约（owner、lease_expires_at）→ 事务提交后发布 → publisher confirm 后置 `published`。
- 崩溃窗口：confirm 前崩溃 → 租约过期 → 其他 Relay 重领 → 重复发布 → Inbox 兜底。**不允许**把"跨 MySQL 与 MQ 的两个动作"伪装成原子的（合同 §12.5.4 原话）。
- `MQ_DRIVER=none` 开发模式：Worker 进程内直读 outbox（同一 SKIP LOCKED 租约逻辑）→ 直接调用进程内 handler → 同一 Inbox 表去重。**与生产路径共享全部领取/幂等代码**，只是"发布"动作从 MQ publish 换成本地函数调用。

**结论**：Relay 领取逻辑实现一次、两个模式复用（platform/outboxrelay，阶段二落地）；阶段一只建表 + 事件写入器（`outbox.Emit(tx, event)` 平台助手，防止各模块手写插入格式漂移）。

---

## D5 好友 epoch 指针表：为什么不是 MAX(epoch)

**问题**：`friendship_epochs(user_low,user_high,current_epoch)` 指针表是否多余？直接 `SELECT MAX(epoch) FROM friendships` 不行吗？

**推敲**：
- 正确性：两把并发"同意申请"事务都跑 MAX+1 → 相同新 epoch → 唯一键 `(user_low,user_high,epoch)` 冲突回滚（能防错但靠撞索引兜底，一次重试后 epoch 仍可能撞第二次）。
- 指针表行在首次建关系时 `INSERT ... ON DUPLICATE KEY UPDATE current_epoch = current_epoch + 1`（原子自增，行锁序列化竞争）→ epoch 分配是**单点原子操作**，竞态在源头消除。
- 性能：有效关系判定是最高频查询之一（每条消息、每次朋友圈访问）；指针表 = 一次主键查行 + friendships 主键查行，稳定 O(1)；MAX(epoch) 无索引扫描或额外索引维护。
- 合同 §12.4 原文即"有效好友关系通过用户对主记录中的当前 epoch 指针确定"。

**结论**：指针表必要，非过度设计。锁顺序（ADR-006）：epochs 行 → friendships 行。

---

## D6 大小写与排序规则：account_name / phone 的精确唯一

**问题**：`Admin` 与 `admin` 是否同一账号名？手机号含空白/`+` 前缀变体是否同一账号？

**推敲**：
- 合同 §12.4："账号名和幂等键采用规范化输入与二进制排序规则，避免大小写或重音折叠造成错误冲突"。
- 决策：**写入即规范化**（account_name: trim + lower；phone: 去 `-`/空格，`+` 保留）+ 列排序规则 `utf8mb4_bin`（MySQL 8.0/8.4 均有；`utf8mb4_0900_bin` 是 8.0 别名，为兼容性用 `utf8mb4_bin`）。双保险：即使应用层规范化有 bug，数据库比较也不会折叠。
- 对比 `utf8mb4_0900_ai_ci`（默认）：`Admin`=`admin` 且 `a`=`á`，对账号标识是**安全漏洞**（撞库注册）。

**结论**：规范化函数沉到 `platform/validate`（单一来源），所有写路径强制经过；列排序规则 `utf8mb4_bin` 写入迁移。

---

## D7 游标分页的形状（对照 keyset pagination 最佳实践）

**问题**：OFFSET 分页在大表 + 并发写入下会跳页/重复。

**推敲**：
- 标准解法 keyset：`WHERE (sort_col, id) < (last_sort, last_id) ORDER BY sort_col DESC, id DESC LIMIT n`。MySQL 8.0 支持行构造器比较 `(a,b)<(x,y)` 走联合索引。
- 每个列表的排序键**必须含唯一尾键**（id）防同值抖动。
- 游标对客户端 opaque（base64(json)），服务端可演进格式；解码失败 → `INVALID_ARGUMENT`。
- 通用性：好友列表（user_id 升序）、申请列表（created_at,id 降序）、收藏（type,created_at,id）……抽 `platform/pagination` 泛型助手：编码/解码 + `Limit` 钳制（默认 20，最大 50/100 按接口定）。

**结论**：`platform/pagination` 泛型游标包（Go 1.24 泛型），各 store 只拼 WHERE 子句。

---

## D8 模块边界的编译期强制（对照 Modular Monolith 实践，Simon Brown）

**问题**：`internal/` 只防外部导入，不防 `contact` 直接 `SELECT FROM messages`。约定靠人守必腐化。

**推敲**：
- Modular monolith 的核心纪律：模块间只走 service 接口；共享内核（platform）只放横切能力。**边界必须工具化强制**。
- 方案：`scripts/check_boundaries.sh` 用 `go list -deps ./internal/<mod>` 断言依赖图：① platform 不依赖任何 domain；② domain 不依赖 domain 的 `store.go` 所属内部包（通过包结构隔离：每模块单一包，store 类型不导出即可从编译器层面防跨模块读表——**跨模块拿不到 *sql.Rows 之外的类型**）；③ domain→domain 只允许 import 根包（服务接口）。
- 更强方案（arch-go / depguard）：引入额外工具链，V1 先脚本 + 包可见性双保险，够用且零依赖。

**结论**：① 每模块**单一 Go 包**（store 类型不导出）；② CI 脚本 `scripts/check_boundaries.sh`（本会话交付）；③ service 接口定义在被调方，调用方 import 其根包。

---

## D9 限流：固定窗口够不够

**问题**：lookup 20 次/分钟、refresh 10 次/分钟用什么算法？

**推敲**：
- 令牌桶/滑动窗口更平滑，但需要定时器或多 key ZSET；固定窗口 INCR+EXPIRE 一条原子命令，误差 = 窗口边界突刺（最坏 2×配额）。
- 本项目场景（防爬 lookup、防刷 refresh、登录失败计数）对突刺不敏感，且 cache 接口（memory/redis 同语义）已具备原子 INCR。
- 登录锁定是**计数器+冻结标记**两个 key，成功清零——与限流分开实现（语义不同：锁定有"冻结期"状态）。

**结论**：`platform/ratelimit` 固定窗口（`cache.Incr` 原子实现），接口留 `Allow(n) (ok, retryAfter)`；将来要换滑动窗口只改实现，调用方不动。

---

## D10 密码哈希参数与可升级性

**问题**：Argon2id 参数硬编码还是可配？将来如何升级参数？

**推敲**：
- RFC 9106 推荐参考参数：Argon2id t=1,m=64MiB(交互式) 或 t=3,m=64MiB（更保守）。OWASP 基准：m=19MiB,t=2,p=1 起步，64MiB 属于高安全档。
- 哈希串自带参数前缀（PHC 格式 `$argon2id$v=19$m=..,t=..,p=..$salt$hash`），**验证时按串内参数跑**——这天然支持参数在线升级（新哈希用新参数，旧哈希按旧参数验证，登录成功时顺带 rehash）。
- 决策：配置可调（config.Auth.Argon*，默认 t=3/m=64MiB/p=2）+ PHC 串格式存储 + 登录成功且参数与当前配置不一致时**透明 rehash**（rehash-on-login 最佳实践）。

**结论**：`platform/argon` 包：Hash/Verify（解析 PHC 串）+ NeedsRehash + RehashOnLogin 语义放 auth service。

---

## D11 平台层复用组件清单（本轮沉淀）

依据"platform 多沉淀可复用能力"的要求，盘点并落地以下组件（领域模块**禁止**各自重新发明）：

| 组件 | 职责 | 消费方 |
|---|---|---|
| `platform/argon` | 密码哈希/验证/PHC解析/重哈希判定 | auth, operator(阶段六) |
| `platform/clock` | 可注入时钟（测试假时钟） | 全部 |
| `platform/validate` | 字符串规范化（phone/account_name）、长度、字符集、保留词、hex | auth, user, contact, media |
| `platform/pagination` | 泛型 keyset 游标编解码 + limit 钳制 | 全部列表接口 |
| `platform/ratelimit` | 固定窗口限流（cache 原子实现） | login, lookup, refresh |
| `platform/cache` | Cache 接口 + memory/redis 驱动 | ratelimit, authn, 会话缓存 |
| `platform/storage` | ObjectStore 接口 + local/s3 驱动、签名下载 URL | media |
| `platform/mq` | Publisher 接口 + none/rabbitmq 驱动 | worker(阶段二) |
| `platform/mysqlx` | DSN 强制参数、WithinTx 死锁重试、IsDuplicate | 全部 store |
| `platform/httpx` | 信封/中间件/游标/Principal 与 Authenticator 接口 | 全部 handler |
| `platform/errors` | 错误模型 + HTTP 映射 | 全部 |
| `platform/ids` | UUID/随机令牌/SHA256 | 全部 |
| `platform/outbox` | `Emit(tx, event)` 统一写入器（防格式漂移） | 全部写事件模块（阶段二起用） |

**红线**：platform 不得 import 任何 domain 包（D8）；Principal 的填充由 auth 模块的 Authenticator 实现完成，platform 只定义接口。

---

## D12 复查后需回写的规格变更

1. SPEC-01 §2.3 补：401→refresh→重试一次的客户端契约；refresh 限流 10次/分/session；重放吊销写审计。
2. SPEC-01 §2.2 补：登录成功且哈希参数旧于配置时透明 rehash（D10）。
3. SPEC-03 补：分片协议=S3 Multipart 同构的演进说明（D3）。
4. SPEC-00 补：D11 组件清单的包名与职责（v1.1）。

---

## D13 规格全量评审（2026-09-29，视角：外部门技术 leader，重点=高并发/稳定性/可观测性）

specs 04-12 写完后做的对抗性复查。每条=质疑 → 结论 → 处置。**已全部回写进对应 SPEC**，此处留痕。

### 正确性问题（6 条，全部已修）

| # | 质疑 | 结论 | 处置 |
|---|---|---|---|
| 1 | SPEC-04 R16 `SYNC_CURSOR_EXPIRED` 永不触发（消息行永久保留 → MIN 可见 seq 恒为 1） | 成立，与合同"确认序号过旧"语义脱节 | 重定义为 180 天可用窗口判定（SPEC-04 R16） |
| 2 | SPEC-07 I1 收藏幂等"接受竞态产生两条"违反合同"重复收藏必须幂等" | 成立 | favorites 补 active_flag 生成列唯一键（owner, source_message_id）（SPEC-07 §2/I1） |
| 3 | SPEC-08 API 表 E1-E3（客户端传分片）与 §7 裁决（服务端导出）自相矛盾 | 成立 | API 表改为异步导出+轮询，编号重排（SPEC-08 §5） |
| 4 | SPEC-04 未读数含 recalled/expired 消息 | 成立（recalled 占位不应计未读） | R18 加 status IN ('stored','delivered') |
| 5 | SPEC-04 conversation_settings 懒创建行上 UPDATE 会 0 行生效 | 成立 | R13 明确全部写操作 UPSERT |
| 6 | SPEC-11 隐藏内容要求 ALTER status 扩枚举 | 不需要（列是 VARCHAR 非 ENUM），误导执行者 | 改为"仅谓词纳入 moderated"（SPEC-11 R7） |

### 高并发问题（5 条，全部已修）

| # | 质疑 | 结论 | 处置 |
|---|---|---|---|
| 7 | 消息发送在会话行锁内做全部校验，锁持时间放大；群 500 人并发全串行 | 部分成立（串行是合同 gapless seq 的固有代价，但锁内工作量可砍） | R2 改两段式：锁外快照预检+幂等预查，锁内只 3 个 INSERT；锁等待 5s 快速失败（SPEC-04 R2、SPEC-12 §4） |
| 8 | 朋友圈 feed 逐条 canView = 每页 20×4 查询的 N+1，P95<500ms 危险 | 成立 | R5 改批量权限过滤（按页聚合作者一次查）；单条 canView 仅用于详情类接口（SPEC-06 R5） |
| 9 | 会话列表未读数+最后消息逐会话查询是热点 N+1 | 部分成立 | 给出明确查询预算（每页 ≈45 条索引查询），不建计数表，记录优化备选路径（SPEC-04 §6.2） |
| 10 | 群批量邀请 50 人 × 逐人系统消息 = 50 次 seq 分配 | 成立 | 聚合为一条系统消息，group_events 仍逐人审计（SPEC-05 R9） |
| 11 | message_receipts 表（每消息×每设备）无限增长永不清理 | 成立，且无业务必要（合同只要"至少一个设备确认"） | 删表；MarkDelivered=条件 UPDATE 天然幂等（SPEC-04 R22/§2） |

### 稳定性问题（5 条，全部已修）

| # | 质疑 | 结论 | 处置 |
|---|---|---|---|
| 12 | 备份导出 GB 级数据在 HTTP 请求内同步执行必超时 | 成立（严重） | 改 Worker 异步任务（outbox backup.export），part 表即断点续跑（SPEC-08 R2） |
| 13 | none 驱动下 api/worker 各内嵌 Relay 会分走事件、Hub 收不到推送 | 成立 | 拓扑裁决：none=cmd/api 单进程全内嵌；cmd/worker 仅 rabbit 模式部署（SPEC-10 R12） |
| 14 | 过期消息逐条发 outbox 事件 → 保留期边界事件风暴 | 成立 | 不发事件，客户端经同步/未读数感知（SPEC-04 R26） |
| 15 | DB 连接池未设定/未观测 → 高并发下连接耗尽表现为随机超时 | 成立 | SPEC-12 §7 池参数（api 50/worker 20）+ SPEC-10 §5.1 db_pool 指标 |
| 16 | RabbitMQ 模式多 Gateway 实例只投递单实例本地连接 | 成立 | V1 明确单实例约束（10k 连接内可承载），广播拓扑留作后续版本，禁止执行者自行实现（SPEC-04 R23） |

### 可观测性补强（用户明示要求，SPEC-10 §5 重写为三支柱）

- HTTP 层统一埋点中间件（route 模板标签防爆炸、in_flight、duration histogram）——业务零侵入
- 业务关键指标：消息发送、WS 连接/慢消费者、登录结果
- db_pool Stats 采集；慢请求/慢事件 WARN 日志
- /readyz 就绪探针（LB 摘流）+ 优雅关停顺序（api 与 worker 分别定义）
- 告警规则清单（发送 P95>300ms、错误率>1%、outbox>1min、死信增长等）

### 评审后维持原设计的质疑（记录理由）

- **会话 seq 行锁串行**：合同 §5.5"连续递增 conversation_seq"要求 gapless，唯一无空洞方案就是单行串行点；已将锁内工作量压到最小。分段/号段方案会留空洞，违反合同。
- **WS query 参数传 token**：浏览器 WS 无法自定义 header 的行业现实；已加访问日志脱敏 + 30min 短寿命双缓解。
- **未读数实时 COUNT**：500 人群 × 180 天窗口内索引 range count 实测可承受；计数表的一致性代价（每写放大）大于收益。
- **缓存/限流 fail-open**：可用性优先于精确性，且登录锁定等安全路径不走 fail-open（锁定计数在 DB 事务内）。
