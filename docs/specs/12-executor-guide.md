# SPEC-12 执行者指南（低等级 LLM 执行者必读，读前不写任何代码）

> 本文件定义：你是谁、按什么顺序读什么、怎么写代码、什么时候必须停下。**本文件与各 SPEC 冲突时，以 SPEC 为准；SPEC 与合同（根目录《微信核心社交仿制项目最终需求分析.md》）冲突时，以合同为准；合同歧义以《reviews/产品交叉评审.md》六裁决为准。**

## 0. 你的工作模式（铁律）

1. **你不做设计决策。** 所有设计已在 docs/specs/00-11 写死。你的工作是把规格逐条翻译成代码。
2. **遇到规格未覆盖的情况：停下。** 在 docs/PROGRESS.md「已知事项」追加一条问题描述，然后转做任务清单中下一个不受阻塞的任务。**禁止即兴设计。**
3. **每个任务开始前**：读 PROGRESS.md → 读对应 SPEC 全文 → 读「§8 参考实现模式」列出的既有文件 → 再动手。
4. **每个任务结束后**：跑 §6 验证命令全绿 → 更新 PROGRESS.md 里程碑行与会话日志 → 才能开始下一个。
5. 规格中的「裁决」字样 = 已经替你做好的技术判断，直接照做，不要重新推敲。

## 1. 任务总顺序（严格按序，除非被阻塞）

```
A. 阶段一收尾
   A1. contact 模块（SPEC-02）           ← 当前第一优先
   A2. media 模块（SPEC-03）
   A3. cmd/api 路由组装（§7.1 组装表）
B. 阶段二
   B1. group 模块（SPEC-05；message 依赖它，必须先行）
   B2. message 模块（SPEC-04）+ internal/ws + cmd/gateway
C. 阶段三    moment 模块（SPEC-06）
D. 阶段四    favorite + cleanup 模块（SPEC-07）
E. 阶段五    backup 模块（SPEC-08）
F. 阶段六    content 模块（SPEC-09）
G. 贯穿      runtime 模块（SPEC-10，可与 B2 后并行）+ operator（SPEC-11）
H. 阶段七    验收矩阵 + 压测（SPEC-11 §8）
```
单个 SPEC 内部：严格按其「任务分解」小节的编号顺序执行。

## 2. 必读文件清单（每个会话开始时）

| 顺序 | 文件 | 目的 |
|---|---|---|
| 1 | docs/PROGRESS.md | 当前状态、已知事项、会话日志 |
| 2 | docs/specs/12-executor-guide.md（本文件） | 工作模式 |
| 3 | 当前任务的 SPEC | 全文精读 |
| 4 | docs/ARCHITECTURE.md | ADR-001~013（架构红线） |
| 5 | §8 列出的参考实现文件 | 模仿既有模式 |

## 3. 架构红线（违反=返工）

- **模块边界（ADR-001/013）**：internal/<domain>/ 是唯一包结构（service.go/store.go/dto.go/handler.go 四件套）。跨模块调用只走对方导出的 Service 方法或消费方定义的 interface（Go 惯用法：接口定义在使用方包内）。**绝不跨模块直查表**（contact 包的 SQL 里不准出现 moments 表名）。每次提交跑 `scripts/check-boundaries.sh`。
- **平台层**：internal/platform/* 不准 import 任何 internal/<domain>。可复用能力（游标、限流、签名、哈希、时钟……）先查 platform 已有，禁止在业务包重造（清单见 SPEC-00）。
- **三进程**：cmd/api（HTTP）、cmd/gateway（WS）、cmd/worker（异步）。业务代码不感知进程：service 只面向 context。
- **禁用清单**：微服务、Kafka/NATS/Redis Streams、搜索集群、工作流引擎、通用权限 DSL、已读回执、在线状态（合同 §12.1）。看到这些词出现在你的脑中——停。

## 4. 代码规范（模仿式开发）

- **先读参考文件再写**（§8）。新文件的结构、命名、注释密度、错误包装风格（`fmt.Errorf("pkg: 动作: %w", err)`）与参考文件保持一致。
- 事务模式：一律 `mysqlx.WithinTx(ctx, s.db, func(ctx, tx) error {...})`（含死锁重试）；包级 `XxxTx(ctx, tx, ...)` 函数供跨模块事务组合（参考 internal/user/service.go 的 CreateAccountTx）。
- 时间：一律 `s.now.Now()`（注入的 clock.Clock，ADR-005），UTC，DATETIME(6)，**禁止 time.Now() 与 DB 默认时间**。
- ID：DB 内 int64，JSON 出入参一律字符串（strconv.FormatInt）。
- 错误：返回 `apperrors.Invalid/FORBIDDEN/...`（platform/errors 映射表），handler 层 `httpx.Error(w, r, err)` 统一信封。禁止在 handler 里写裸 `http.Error`。
- 入参解码：`httpx.DecodeBody`（DisallowUnknownFields 已内置）；路径参数 `r.PathValue("id")` + ParseInt 校验。
- 分页：一律 platform/pagination 的 keyset 游标，禁止 OFFSET。
- SQL：参数化占位符；写操作先想锁序（ADR-006 文档）；唯一键幂等用「INSERT IGNORE/ON DUPLICATE KEY + 撞 1062 重读」惯用法（参考 conversation.CreateDirectTx）。mysqlx.Open 的 DSN 已带 `innodb_lock_wait_timeout=5`（锁等待 5s 快速失败，WithinTx 识别为可重试/繁忙错误——SPEC-04 R2 依赖此设定；A3 任务在 mysqlx 中补此 DSN 参数时同步加单测）。
- 注释：包注释一段（是什么、依据哪条 SPEC/合同）；导出符号注释一行；**不要写中文注释长篇解释——保持与既有代码同等密度**。
- 每个 service 方法对应 SPEC 的一条 R 规则：在方法注释里标注规则号（如 `// R9 撤回：2 分钟窗口`），便于评审对照。

## 5. 测试规范

- 每个模块 `*_test.go` 覆盖 SPEC 的 T 表**逐条**（T 编号写在测试名里：`TestR9RecallWithinTwoMinutes`）。
- 依赖注入 stub：跨模块端口在测试文件里定义 fake 结构体（参考 platform_test.go 风格）。
- 需要真 MySQL 的测试：`//go:build integration` 标签，`WECHAT_TEST_MYSQL_DSN` 环境变量；无 DSN 时 `t.Skip`。
- 时间相关用例用 clock.Fake（platform/clock 已有）前拨/后拨。

## 6. 每任务验证命令（全绿才算完成）

```bash
go build ./...
go vet ./...
go test ./...            # 单测（无 integration）
bash scripts/check-boundaries.sh
# 迁移类任务另跑（需 DSN）：
go run ./cmd/migrate status
```

## 7. cmd/api 组装表（任务 A3 的唯一蓝图）

```
main.go 装配顺序：
1. config.Load → logger → clock
2. mysqlx.Open（DSN 校验）+ 连接池调优（高并发稳定性）：
   SetMaxOpenConns(WECHAT_DB_MAX_OPEN，默认 50) / SetMaxIdleConns(默认 10)
   / SetConnMaxLifetime(默认 30m) / SetConnMaxIdleTime(默认 5m)
3. cache.New(memory|redis) / storage.New(local|s3) / mq.New(nop|rabbit)
4. 领域 service 构造（依赖序）：
   audit → conversation → device → auth → user(←media stub 注入点：A3 阶段先传 noop
   实现，media 完成后替换) → contact → media → group → message → moment
   → favorite → cleanup → backup → content → operator
5. chi router：/api/v1 路由逐 SPEC 的 API 表挂载；中间件链
   RequestID → Recover → Metrics →（业务组）RateLimit → Authenticator(RequireAuth)
   /ws 与 /healthz、/readyz、/metrics 挂根级别
6. http.Server{ReadTimeout:10s, WriteTimeout:30s, IdleTimeout:120s}
   + 每请求 ctx 超时 10s（中间件 context.WithTimeout）
7. 优雅关停：先 /readyz 变 503（LB 摘流）→ 等 5s 排空 → ctx 超时 10s →
   先关 HTTP（含 WS）→ 停 Relay/Scheduler → 关 DB
```
**默认限流表（platform/ratelimit 固定窗口，fail-open）**：登录/刷新等鉴权类按 SPEC-01（登录锁定 10 次/10min、刷新 10/min）；一般写接口 100 次/min/用户；媒体分片 PUT 600 次/min/用户；发送消息 120 次/min/用户；读接口 300 次/min/用户。值放 platform/config 常量，不做每接口配置化。
**外部调用超时（全部强制）**：MySQL 查询走请求 ctx；cache 200ms（超时 fail-open 降级直查 DB）；storage 读 10s / 写 30s；MQ publish 仅 Relay 内（不在请求路径）。
启动参数/环境变量全部走 platform/config（WECHAT_*），禁止新增配置读取路径。

**cmd/worker 装配**（SPEC-10 §4.6）：同样设连接池（默认 MaxOpen 20——Worker 与 API 分进程，池独立）；优雅关停顺序=停消费（不再 ACK 新消息）→ 处理完在飞事件（上限 30s）→ 停 Scheduler → 关 DB。

## 8. 参考实现模式（照抄结构）

| 要学什么 | 参考文件 |
|---|---|
| service+事务+Tx 组合函数 | internal/user/service.go |
| handler/DTO/视图/错误映射 | internal/user/handler.go |
| 跨模块端口（消费方接口） | internal/user/handler.go 的 MediaBinding |
| 唯一键幂等惯用法 | internal/conversation/service.go CreateDirectTx |
| 审计写入 | internal/auth/service.go（LogTx 调用点） |
| 令牌/会话安全 | internal/device/service.go |
| platform 组件用法大全 | internal/platform/platform_test.go |
| 迁移书写风格 | migrations/00001_phase1_foundation.sql |
| 边界脚本 | scripts/check-boundaries.sh |

## 9. 何时必须停下（硬条件）

- SPEC 与现实冲突（表不存在、字段名不符、参考文件已被改动得面目全非）。
- 需要引入 SPEC 未列的新依赖（go.mod 新包）。
- 需要改 platform 层接口签名。
- 测试连跑 3 次同一失败且原因不明。
- 任何「要不要……」类的设计问题。

停下动作：PROGRESS.md「已知事项」新增编号条目（现象、复现、你的判断、需要的决策），然后按 §1 顺序做下一个不阻塞任务。**禁止**：删测试、跳过验证、在代码里留 `// TODO 决定一下` 然后继续。

## 10. 完成的定义（DoD，每任务逐项自检）

- [ ] SPEC 的 R 规则全部有代码落点（注释标规则号）
- [ ] SPEC 的 T 用例全部有测试且通过
- [ ] §6 四命令全绿
- [ ] PROGRESS.md 已更新（里程碑行 + 会话日志一段：做了什么/改了哪些文件/遗留什么）
- [ ] 未引入新依赖、未改 platform 接口（除非 SPEC 明确要求）
- [ ] 边界脚本通过（新增模块已加入 DOMAINS 列表）

## 11. 迁移文件编号对照表（执行时按此创建，不要改号）

| 文件 | 内容 | 出处 |
|---|---|---|
| 00002_message.sql | 消息域全部表 | SPEC-04 §2 |
| 00003_group.sql | 群/待办全部表 | SPEC-05 §3 |
| 00004_moment.sql | 朋友圈全部表 | SPEC-06 §3 |
| 00005_favorite.sql | 收藏/清理 + media_gc_queue（如缺） | SPEC-07 §2 |
| 00006_backup.sql | 备份/恢复/迁移握手 | SPEC-08 §2 |
| 00007_content_account.sql | 内容账号/客服/通知 | SPEC-09 §2 |
| 00008_operator.sql | 运营/举报/处置 | SPEC-11 §2 |

每个迁移必须写配套 Down（DROP TABLE 反序）。0001 已存在，禁止修改——需要变更走新编号迁移。

## 12. 环境事实（不要浪费时间重新发现）

- GOPROXY 必须是 `https://goproxy.cn,direct`（proxy.golang.org 被墙，已 go env -w 配好）。
- 本机无 Docker/Redis/RabbitMQ/MinIO：开发驱动 memory/local/none（合同允许的降级路径），集成测试需用户提供 MySQL DSN 后 `-tags=integration`。
- go.mod 声明 go 1.24（合同要求），本机工具链 1.26 可编译。
- WebSearch/WebFetch 在本环境不可用——不要尝试联网查资料，一切以仓库文档为准。
