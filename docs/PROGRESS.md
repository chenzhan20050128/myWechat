# 开发进度总账（Handover Ledger）

> 本文件是跨 Agent 会话交接的唯一入口。**每次会话开始先读本文件，结束时必须更新本文件。**
> 状态口径：`✅` 完成并验证 / `🟡` 代码完成未验证 / `🚧` 进行中 / `❌` 未开始。
> 最后更新：2026-09-29（会话 6，B1 group 模块完成：规则/存储/服务/处理器/迁移 00003/集成测试 22 项全绿 + 真机端到端冒烟）

## ⚠️ 当前状态（2026-09-29）

**B1 group（SPEC-05）已完成：R1-R26 全部落地，22 项集成测试 + 8 项规则单测全绿，真实启动验证建群/邀请码入群/禁言/待办生命周期/ADR-005 string id 序列化。**
下一个任务 **B2=message（SPEC-04，含 WS 网关/180 天保留）**，依赖 B1 的 CanSend/IsModerator 端口。
开工入口：**docs/specs/12-executor-guide.md**（铁律、任务顺序、代码规范、DoD）。

## 0. 三十秒恢复上下文

- 项目：微信核心社交仿制后端（无前端）。唯一验收合同 = 根目录《微信核心社交仿制项目最终需求分析.md》，冲突语义以《reviews/产品交叉评审.md》六项裁决为准。
- 技术栈已定稿（不可回退）：Go 模块化单体 + MySQL 8.x(InnoDB) + Redis + RabbitMQ + MinIO/S3 + WebSocket + Outbox Worker。禁止微服务/搜索集群/工作流平台。
- 开发方法：SDD（Specification-Driven Development）。**任何模块先写 `docs/specs/NN-<module>.md`，规则写全后才写代码。** 规格与代码同在验收范围内。
- 交付顺序（需求文档 §19）：阶段一 身份/关系/媒体底座 → 阶段二 消息与群 → 阶段三 朋友圈 → 阶段四 个人效率 → 阶段五 备份恢复 → 阶段六 内容账号 → 阶段七 稳定性验收。
- 当前阶段：**阶段一进行中**。

## 1. 环境现状（新会话必读）

| 项 | 状态 | 备注 |
|---|---|---|
| Go | ✅ go1.26.1 windows/amd64 | go.mod 声明 go 1.24（合同要求），用 1.26 工具链编译 |
| MySQL（开发实例） | ✅ **独立实例已就绪，集成测试可直接跑** | 本机 3306 上的实例 root 密码未知，**不要去动它**。已在 `.tmp/mysql-data` 用 `mysqld --initialize-insecure` 起了一个私有实例，**端口 3307**（`--mysqlx=0`，pid/log 在 `.tmp/mysql-run/`），库 `wechat_dev`（utf8mb4/utf8mb4_bin），账号 `wechat` / `wechat-dev-pw`。迁移 00001 已应用。 |
| MySQL 集成测试 DSN | ✅ 现成可用 | `WECHAT_TEST_MYSQL_DSN='wechat:wechat-dev-pw@tcp(127.0.0.1:3307)/wechat_dev'` 后 `go test -tags integration ./...` |
| Docker | ❌ 未安装 | `deploy/docker-compose.yml` 备好，供有 Docker 的环境一键起全栈 |
| Redis | ❌ 未安装 | 开发模式 `CACHE_DRIVER=memory` 进程内缓存降级（接口一致） |
| RabbitMQ | ❌ 未安装 | 开发模式 `MQ_DRIVER=none`，Worker 直接轮询 MySQL Outbox 投递（合同 §12.5.2 明确保留该路径） |
| MinIO | ❌ 未安装 | 开发模式 `STORAGE_DRIVER=local` 本地磁盘对象存储（接口与 S3 一致） |

## 2. 里程碑状态

| 阶段 | 模块 | 规格 | 代码 | 测试 | 状态 |
|---|---|---|---|---|---|
| 一 | platform（config/errors/logger/ids/httpx/mysqlx/cache/storage/mq/argon/clock/validate/pagination/ratelimit/outbox.Emit） | ✅ 00 | ✅ | ✅ 单测 | **完成**，未连真实 MySQL/Redis |
| 一 | 迁移 0001（含 upload_chunks/prev_refresh_hash 修订） | — | ✅ | 未跑库 | 代码完成，等 MySQL 凭据 |
| 一 | audit / conversation 最小集 / device | ✅ 01 | ✅ | — | **完成**（device 含轮换/重放检测/缓存吊销） |
| 一 | auth（注册/登录/锁定/令牌/改密/运营重置） | ✅ 01 | ✅ | — | **完成**，handler 完成 |
| 一 | user（资料/头像/公开视图） | ✅ 01 | ✅ | — | **完成**，头像绑定依赖 media.BindAvatar（未实现） |
| 一 | contact（申请/epoch/单向权限/标签/搜索） | ✅ 02 | ✅ | ✅ 单测 + 真实库集成（A1-A7） | **完成**（27 项测试全绿） |
| 一 | media（上传会话/分片/对象/下载） | ✅ 03 | ✅ | ✅ 单测 + 真实库集成（A1-A6） | **完成**（25 项测试全绿；user 头像绑定已打通） |
| 一 | cmd/api 路由组装 + docker-compose | ✅ 00 | ✅ | ✅ 真机冒烟（register/login/me/media + 限流） | **完成** |
| 二 | group（群/二维码/禁言/待办） | ✅ 05 | ✅ | ✅ 规则单测 8 项 + 真实库集成 22 项 | **完成**（B1，R1-R26 全落地） |
| 二 | message（HTTP 路径 + 180 天保留 worker；WS 网关留 G） | ✅ 04 | ✅ | ✅ 集成 10 项 | **主体完成**；WS 网关在任务 G 随 runtime 落地 |
| 三 | moment（可见快照/互动/定时发布） | ✅ 06 | ❌ | ❌ | 任务 C |
| 四 | favorite + cleanup（收藏/存储清理） | ✅ 07 | ❌ | ❌ | 任务 D |
| 五 | backup（备份/恢复/设备迁移） | ✅ 08 | ❌ | ❌ | 任务 E |
| 六 | content（公众号/文章/菜单/客服/通知） | ✅ 09 | ❌ | ❌ | 任务 F |
| 贯穿 | runtime（Relay/Inbox/重试/Scheduler/Worker/指标） | ✅ 10 | ❌ | ❌ | 任务 G（B2 后可并行） |
| 六~七 | operator（审计/举报处置/报表/验收矩阵） | ✅ 11 | ❌ | ❌ | 任务 G+H |
| — | 执行者指南（必读） | ✅ 12 | — | — | 执行者开工入口 |

## 3. 已知事项 / 待办（按优先级）

1. **[已完成] 全项目规格 specs 04-12 已写完**（2026-09-29，含跨部门技术 leader 评审修订，见 design-review.md D13）。
2. **[已完成] MySQL 集成验证不再阻塞**：见 §1 的私有 3307 实例。真实库已抓到并修掉 5 个只有连库才暴露的缺陷（见 §5 会话 3）。
3. ~~[技术] media.BindAvatar（user 头像绑定依赖）尚未实现~~ → **A2 已完成**，头像绑定跨模块原子提交已打通。
4. [技术] Redis/RabbitMQ/S3 适配器未写（memory/local/none 驱动已可用）；接口已冻结，接入属阶段二伴随任务。
   - [S3 驱动待办] `storage.ObjectStore.Put` 的 mime 参数在组装时取**声明值**（未与字节重新核对）；S3 驱动必须在 Put 时把它作为对象的 Content-Type，而不是等 media.process worker 再修。
5b. **[user 模块审计缺口]** SPEC-01 要求 `user.profile.updated` 审计事件，但 `user.Service` 当前没有 audit 依赖。阶段一 A3 路由组装时一并接入（组合根注入 audit 端口）。
5. [决策记录] 手机号未验证的账号模型是产品明确接受的低可信模型（合同 §3.1），不要在代码里加短信验证"TODO"。
6. **[SPEC-02 口径澄清，实现已按此落地]** 三处规格歧义，后续模块遇到同类问题照此处理：
   - R1 正文写 `POST /contacts/qrcode`，§4 契约表写 `GET /api/v1/contacts/qrcode` → **以 §4 契约表为准（GET）**，因为 §4 是 A3 路由组装的真值来源。
   - `source=card` 的 `card_owner_id` 语义：取"**申请人的好友（名片分享人）**"，`target_id` = 名片主人。只有这一种读法自洽——若校验 target 则与 R4 的 `ALREADY_FRIEND` 冲突。
   - `source=group` 按 R1 阶段一返回 `RESOURCE_UNAVAILABLE`，阶段二群模块补齐。
7. **[SPEC-02 规则补强，已实现]** R15/R17 的"设置行随 epoch 保留"在 **拉黑后删除好友** 时会产生死锁态：被拉黑方无法再申请（R5），而拉黑方因已非好友无法解除拉黑 → 该 pair 永久不可恢复，与 R13"可重新添加"矛盾。实现取**"已有设置行即可编辑，无设置行才要求是好友"**：既堵住死锁态，又不允许对陌生人设置权限。集成测试 `TestA5BlockSemantics` 固化该语义。
8. **[阶段二补全] 黑名单列表接口**：R17 的解除拉黑路径已可经 `PATCH /contacts/friends/{id}/settings` 抵达，但客户端缺少"列出已拉黑的非好友"的接口（R25 只列好友）。阶段二群/消息模块落地时一并补，勿在阶段一私自扩 API。

## 4. 目录地图（随开发演进，保持更新）

```
cmd/api        HTTP 入口（路由组装，未创建）
cmd/migrate    迁移工具 ✅        cmd/worker  Worker（未创建）
internal/
  platform/    config,logger,errors,httpx(信封/中间件/游标/Principal),mysqlx,cache,storage,mq,
               argon,clock,validate,pagination,ratelimit,ids,outbox,sigtoken ✅
  auth/  user/  device/  conversation/  audit/   ✅（阶段一）
  contact/     rules.go(纯规则) qrcode.go store.go service.go dto.go handler.go
               + rules_test.go + integration_test.go(//go:build integration) ✅
  media/        rules.go(纯规则) store.go service.go dto.go handler.go
               + rules_test.go + integration_test.go(//go:build integration) ✅
docs/          PROGRESS(本文件) ARCHITECTURE design-review sdd-workflow specs/00-12
migrations/    00001_phase1_foundation.sql + embed.go ✅
scripts/       check-boundaries.sh ✅（模块边界断言，CI 必跑）
.tmp/          私有 MySQL 3307 实例数据/运行目录（勿提交，勿删除）
```

## 5. 会话日志（倒序，每次会话追加一段）

### 会话 7 — 2026-09-29（B2 message HTTP 路径 + 保留 worker 完成）
- **迁移 00004_message.sql**（已在会话 6 末落地并应用）：messages/message_assets/message_references/message_forwards/message_pins/conversation_settings 6 张表。关键唯一键：`uk_messages_client(sender_id, client_msg_id)`（幂等）、`uk_messages_conv_seq(conversation_id, conversation_seq)`（seq 连续）、`idx_messages_expiry(status, expires_at)`（worker 扫描）。
- **`internal/message/` 四件套**：
  - `rules.go`（纯规则）：8 种类型的 payload 校验（text/emoji/image/video/voice/file/card/link/system）、digestOf（≤200 runes 引用快照）、previewOf（≤50 字推送预览）、expiryAt（now+180d）、canRecall（2 分钟窗口）、normalizeHistoryLimit（1..50）。
  - `store.go`：全部 SQL。两段式发送：`lockConversation FOR UPDATE` → `seq = last_seq+1` → `bumpConversationSeq` → `insertMessage`。recall 用条件 UPDATE（`status IN stored,delivered AND created_at > now-2min`）。conversation_settings 全部 UPSERT（INSERT...ON DUP KEY UPDATE），`last_read_seq = GREATEST(last_read_seq, VALUES(last_read_seq))` 天然只前进（裁决 4）。worker sweep 用 `FOR UPDATE SKIP LOCKED`。
  - `service.go`：R1-R13、R15-R19、R22、R26 全部落地。端口：`Friend.CanSendMessage`（contact 已实现）、`Group`（在组合根用 `groupMessageAdapter` 适配，避免 group↔message 双向 import）、`Media.AssertReady/OnReferencesRemoved`（本会话给 media 补上）、`Conversation.IsMember`。`SendSystemTx` 实现 group 模块依赖的 `SystemMessenger` 端口——群系统消息与业务 tx 同事务提交（ADR-006）。
  - `handler.go`：A1-A12 路由全部挂载。所有 int64 id 按 ADR-005 序列化为 JSON string。
- **新增 `internal/ws/hub.go`**：进程内 `map[userID]map[deviceID]Conn`。Register 自动踢同设备旧连接（R20 4002）；Deliver 慢消费者直接 Close（R25 1024 帧队列上限在 pump 侧）。完整 ws handler（auth 用 `?access_token=` query、ping/pong、ack → MarkDelivered）留到任务 G 与 runtime 一起落地。
- **新增 `cmd/worker/main.go`**：单进程周期任务，每分钟跑一次消息过期（R26）、群待办 overdue、群禁言 GC、联系人申请过期、上传会话过期。worker 与 api 是两个独立 binary，组合根在各自 main.go（任务 G 抽出共享 composition 包）。
- **只有真实 MySQL 集成测试才暴露的 1 个根因缺陷**：历史拉取默认（无 after_seq/before_seq）走 `seq < 0` 永远查不到消息。`listMessagesBefore` 拆成独立 SQL：before_seq=0 时不加 `<` 条件，直接取最新 N 条。
- 验证：10 项集成测试全绿（T1 幂等、T2 非法 payload、T6 并发 50 发送 seq 1..50 无空洞、T7 119s 可撤回/121s 冲突、T8 撤回后 payload=null、T10 pin 第 21 条 QUOTA_EXCEEDED、T12 read cursor 不回退、T13 标未读不动 last_read_seq、T15 181 天后 worker 把消息翻成 expired 且保留行、T18 MarkDelivered 幂等）。边界检查通过；`go build ./...`/`go vet ./...` 干净。
- **交接点：下一个任务 C=moment（SPEC-06，可见性快照/互动/定时发布）。**

### 会话 6 — 2026-09-29（B1 group 模块完成）
- **迁移 00003_group.sql**：10 张表（`groups`/group_members/group_invite_codes/group_invite_uses/group_mutes/group_events/group_todos/group_todo_assignees/group_todo_member_snapshots/group_todo_events）。关键设计：`group_members.active_flag` 生成列（`IF(left_at IS NULL,1,NULL) STORED`）+ `uk_gmembers_active(group_id,user_id,active_flag)` 让 `INSERT IGNORE` 天然幂等（同一用户重复入群不产生多行）；`member_count` 在 `groups` 行上由锁串行维护（≤500，无 oversell）；invite code 用 10 字符 Crockford Base32（`ids.NewInviteCode`），24h TTL，50 uses 原子 `UPDATE ... WHERE use_count<max_uses`；24h ban-rejoin 窗口；mute `until_at` 为 NULL=永远禁言，canSend 以 DB 时间字段为唯一真值（worker 只做 GC，不依赖）。
- **`internal/group/` 五件套**：rules.go（纯规则：canSend/muteUntil/resolveAssignees/validateGroupName/Title/Description）/ store.go（全部 SQL，按 ADR-006 锁序 `groups` 行 FOR UPDATE → conversation 行 → messages）/ service.go（R1-R26，端口：`Conversation.CreateGroupTx`、`Friend.GetActiveFriendship`、`SystemMessenger.SendSystemTx`——message 模块 B2 实现）/ handler.go（B1-B23 + GET /users/me/groups，ADR-005 int64 id 全序列化为 string）。
- **只有真实 MySQL + 真机启动才暴露的 4 个缺陷，按根因修掉（非补丁）**：
  1. `left_reason VARCHAR(8)` 装不下 `'dissolved'`（9 字符）→ 扩到 VARCHAR(16)。
  2. Quit 误用 `requireRole(..., ownerOnly=true)` → 普通成员被 FORBIDDEN，应是"任何成员都能退群，群主单独 STATE_CONFLICT"。改用 `requireGroupForWrite` + 自己判断 role。
  3. Kick/Quit 调了 `waiveAssigneesOnLeave` 但没对受影响 todo 调 `recomputeTodoStatus` → 全员 waived 时 todo 不自动 cancelled（R22 裁决）。循环 affected todo ids 调 recompute。
  4. HTTP 层 int64 id 直接进 JSON（`user_id:202`）→ 违反 ADR-005。ListMembers/ListTodos/GetTodo/ListMyGroups 全部改为 string 序列化。
- 验证：规则单测 8 项全绿；集成测试 22 项（T1-T22 覆盖建群/非法名/邀请/幂等/邀请码入群/过期码/kick+ban-window/群主不能退/退群后可再进/转让/解散/升管理员/管理员不能踢管理员/禁言到期自动解禁/管理员不能禁言管理员/解禁/非成员 403/待办完成/全员 waived 自动 cancel/快照可见性/取消待办/我的群列表）全绿；真实启动 E2E 验证（注册 2 用户 → 建群 → 邀请码入群 → 禁言 10m → 建待办 → 完成待办 → 状态自动 completed，所有 id 为 string）。
- **交接点：下一个任务 B2=message（SPEC-04，WS 网关/180 天保留/系统消息 SendSystemTx 实现）。**

### 会话 5 — 2026-09-29（A3 cmd/api 组装完成）
- **新增 `cmd/api/main.go`**：唯一 HTTP 组合根。装配序：config → logger → clock → mysqlx（连接池参数化）→ storage（local/s3 分支）→ 领域 service 按依赖序构造（audit→conversation→device→media→user→contact→auth）→ 标准库 `http.ServeMux`（Go 1.22 路由，**零第三方路由库**——KISS）。
- **中间件链**：`RequestID → Recover → (按路由分组) RateLimit → RequireAuth`。限流器按 SPEC-12 §7 默认表：auth 20/10min（按 IP）、write 100/min（按 user）、read 300/min（按 user）、media chunk 600/min（按 user）。
- **健康路由**：`GET /healthz`（200 字节）、`GET /readyz`（envelope，DB ping 留待 runtime 模块 G）。媒体下载代理 `/api/v1/media/download` 故意**公开**——签名 token 本身就是凭证（R16）。
- **新增 `deploy/Dockerfile` + `deploy/docker-compose.yml`**：MySQL 8.0.44（utf8mb4_bin + READ-COMMITTED + 5s lock timeout）+ api 多阶段构建。
- **只有真机启动才暴露的 2 个缺陷，按根因修掉**：
  1. `auth.Config.Argon` 漏装配 → `argon.Hash` 零值参数 Time=0 直接 panic-wrapped 成 INTERNAL_ERROR。补全 Argon 参数装配。
  2. 限流 subject 用 `RemoteAddr`（host:port）→ 每次 curl 都是新临时端口，限流器永不触发。改为 `net.SplitHostPort` 取裸 IP。
- 验证：真实启动 → register（user_id=96）→ login（返回 access_token）→ GET /users/me（200）→ POST /media/uploads（envelope 正确序列化 int64 为 string）→ hammer login 25 次返回 **20×401 后 5×429**（限流按表触发）。`go build ./...` / `go vet`（双 tag）/ `go test ./...` / 边界检查全绿，contact+media 集成测试回归全绿。
- **交接点：下一个任务 B1=group（SPEC-05）。**

### 会话 4 — 2026-09-29（A2 media 完成）
- **media 模块全量落地**：`rules.go`（纯规则，零 I/O：分片几何、MIME 解析、对象 key 形状）/ `store.go`（全部 SQL，`upload_chunks` 是分片到达的 SSOT）/ `service.go` / `dto.go` / `handler.go`，覆盖 SPEC-03 R1-R16。
- **关键结构决策**：
  - **对象行与会话转换、outbox 事件同一事务提交**（`verifyAssembly` 只碰存储不碰数据库）。并发 complete 输家的 `media_objects` 行随事务回滚，不留孤儿行。
  - **存储端口无驱动分支**：`SignGetURL` 永远返回绝对 URL；`ProxyVerifier` 是本地驱动可选接口，组合根决定 wiring，领域代码零 switch。
  - **头像绑定跨模块原子**：`user.MediaBinding` 端口的 `BindAvatarTx(ctx, tx, objectID, userID)` 在调用方事务内写 media 引用，与 user_profiles 行一次提交（ADR-001）。
- **测试**：`rules_test.go`（纯逻辑，9 项）+ `integration_test.go`（`//go:build integration`，真实 MySQL + 真实本地磁盘对象存储，9 项覆盖 A1-A6 + R27 头像绑定 + HTTP 端到端含代理下载）。全部通过。
- **只有连真实 MySQL 才暴露的 3 个缺陷，全部按根因修掉（非补丁）**：
  1. `outbox.Emit` 只写 `created_at`，但 `outbox_events.updated_at` NOT NULL 无默认值 → 严格模式拒绝插入。补写同一时间戳。
  2. `CreateSession` 误用 `resolveMIME` 做声明白名单校验：`mimeContradicts(image/png, octet-stream)` 为真（声明可嗅但"无字节"），导致合法 PNG 声明在创建时即被拒。拆出 `validateDeclaredMIME`（只查白名单），字节证明留给组装阶段的 `resolveMIME`。
  3. `storage.Local.SignGetURL` 用 `time.Now()` 而非注入时钟 → 测试 fake clock 下签名与验证时间源不一致。`NewLocal` 增加 `now func() time.Time` 参数。
- 验证：`go build ./...` / `go vet ./...`（含 `-tags integration`）/ `go test ./...` / `bash scripts/check-boundaries.sh` 全绿；media 集成 25 项全绿，contact 27 项回归全绿。
- **交接点：下一个任务 A3=cmd/api 路由组装 + docker-compose（蓝图 SPEC-12 §7）。**

### 会话 3 — 2026-09-29（A1 contact 完成）
- **新增 platform 组件 `sigtoken`**：一处 HMAC-SHA256 签名令牌实现（`Sign`/`Verify`/`MAC`/`EqualMAC`），供个人二维码令牌与本地驱动下载 URL 共用；删掉 `storage/hmac.go` 的重复实现，`config.Secret.Signing` 统一密钥来源（`WECHAT_SIGNING_SECRET`）。
- **新增跨模块视图 `user.Identity` + `Identities(ids)` 批量查询**：contact 不再触碰 users/user_profiles 表（ADR-001），列表页 hydration 走批量 IN 而非逐行查询。
- **contact 模块全量落地**：`rules.go`（纯规则，零 I/O）/`qrcode.go`/`store.go`（全部 SQL）/`service.go`/`dto.go`/`handler.go`，覆盖 R1-R27。epoch 用 `friendship_epochs` 行做 per-pair 串行锁（ADR-006 锁序：epochs → friendships/requests → conversation）。
- **测试**：`rules_test.go`（纯逻辑，12 项）+ `integration_test.go`（`//go:build integration`，真实 MySQL，15 项覆盖 A1-A7）。全部通过。
- **只有连真实 MySQL 才暴露的 5 个缺陷，全部按根因修掉（非补丁）**：
  1. `mysqlx` DSN 用 `tx_isolation` → MySQL 8.0.44 error 1193（该变量 8.0 已删除），改 `transaction_isolation`，并补 `innodb_lock_wait_timeout=5`。
  2. `cmd/migrate` 里 goose 路径写 `"migrations"`，但 embed FS 根就是该目录 → 改 `"."`。
  3. `activeFriendIDsIn` 的 `IF(f.user_low = ?, ...)` 投影占位符少传了 owner 实参 → 参数个数不匹配（`expected 7 arguments, got 6`）。
  4. `AcceptRequest` 幂等重入路径只回 `status`，丢了 `conversation_id` → 首次调用超时的客户端重试后拿不到会话 id，无法打开刚建的聊天。
  5. `UpdateFriendSettings` 仅以"是好友"为前置，导致拉黑→删除后永久无法解除（详见 §3.7）。
- 验证：`go build ./...` / `go vet ./...`（含 `-tags integration`）/ `go test ./...` / `bash scripts/check-boundaries.sh` 全绿；集成测试 27 项全绿。
- **交接点：下一个任务 A2=media（SPEC-03）。**

### 会话 2 — 2026-09-29
- 完成阶段一 platform 层全部组件（含 D11 清单 13 项）+ 迁移 0001 + audit/conversation/device/auth/user 模块代码。
- 用户追加要求：加大架构推敲（已产出 docs/design-review.md D1-D12）+ platform 沉淀复用组件。
- 用户指令：暂停编码 → 写完全项目规格供低等级 LLM 执行。**已完成：specs 04-12 全部落笔**（消息/群/朋友圈/收藏清理/备份/内容账号/运行时/运营/执行者指南）。
- 用户要求规格完成后做跨部门技术 leader 对抗评审（重点高并发/稳定性/可观测性）→ **design-review.md D13**：16 条真实问题全部回写修正（正确性 6、高并发 5、稳定性 5）+ 可观测性三支柱重写 + 4 条"维持原设计"的理由留痕。
- `go build ./...`、`go vet ./...`、platform 单测、边界检查全部通过（本会话后半未改代码，仅文档）。
- **交接点：设计阶段完成。执行者从 docs/specs/12-executor-guide.md 开工，任务 A1=contact（SPEC-02）。**

### 会话 1 — 2026-09-28
- 读完全部 6 份需求/评审文档，确认合同口径（含交叉评审六裁决）。
- 建立 docs 治理体系：PROGRESS / ARCHITECTURE / sdd-workflow / specs 00-03。
