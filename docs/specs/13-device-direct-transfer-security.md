# SPEC-13 同账号设备直传与传输安全加固

> 状态：提案，未实现。
> 灵感来源：`D:\workspace\PairDrop\仿制PairDrop技术方案设计-精讲版.md`。
> 适用基线：`docs/specs/04-message.md`、`docs/specs/08-backup.md`、`internal/device`、`internal/ws`、`internal/media`。
> 目标读者：后端、客户端与安全评审人员。

## 1. 结论先行

PairDrop 可以给 myWechat 带来的最大价值不是“附近设备互传”这个产品能力，而是四个可迁移的工程决策：

1. **信令与数据分离**：服务器只转发 WebRTC 握手信息，不搬运文件字节。
2. **状态归属分析**：在线状态和传输会话是可丢失的短期状态，不应进入 MySQL 业务事实表。
3. **端到端确认**：连接建立、用户确认、文件哈希校验分层进行，不能只相信服务器口头状态。
4. **统一安全边界**：所有点对点信令必须在一个 choke point 校验同账号、同设备、会话有效性。

因此新增一条明确需求：**同账号在线设备之间的端到端文件直传通道**。它不替代现有文件传输助手，而是提供“不经过服务器字节面”的临时传输模式。

同时加固现有安全边界：

- WebSocket 不再使用长期 Access Token 作为 URL 查询参数，改用一次性 WS Ticket。
- TURN 凭证短时效下发，禁止静态写死在客户端。
- 任何“服务器明文中转文件”的降级路径默认禁止。

## 2. 为什么不是完整复刻 PairDrop

PairDrop 是无账号、附近设备、匿名发现的产品；myWechat 是账号中心的熟人社交系统。两者信任模型不同，不能直接搬。

| PairDrop 机制 | 是否采纳 | myWechat 处理方式 | 理由 |
|---|---:|---|---|
| IP 房间自动发现 | 不采纳 | 只展示同账号在线设备 | IP 不能代表身份，且会暴露网络拓扑 |
| 公共房间 | 不采纳 | 不提供匿名公共房间 | 与熟人社交边界冲突，扩大滥用面 |
| 长期 roomSecret 配对 | 不采纳 | 复用账号设备注册与会话 | 已有更强的账号身份，不应并行维护第二套信任凭证 |
| WebRTC DataChannel | 采纳 | 同账号设备直传 | 降低服务器带宽成本，提升传输隐私 |
| TURN 加密中继 | 采纳 | 打洞失败时兜底 | 无 TURN 则跨 NAT 场景不可用 |
| 应用层心跳 | 采纳 | 传输会话独立心跳 | 防止半开连接和幽灵设备 |
| 64KB 分块 + 1MB 窗口 | 采纳 | 直传文件协议 | 提供端到端背压，避免内存失控 |
| WS 明文降级 | 不采纳 | 仅允许显式云端传输 | 不做自动降安全 |

关键取舍：

1. **不做好友间 P2P 聊天**。聊天必须支持离线、多端同步、群聊和历史读取，P2P 只适合在线即时传输。
2. **不做匿名附近发现**。myWechat 的信任边界是账号和设备，不是 IP。
3. **不把直传状态写入 MySQL**。直传是短期会话，丢失后可重试，不是业务事实。
4. **不承诺云端文件端到端加密**。现有云端媒体仍由服务端鉴权和存储；端到端只适用于“直传且不保存到云端”的模式。

## 3. 产品需求

### 3.1 新能力：同账号设备直传

用户在文件传输助手中选择“发送到我的另一台设备”，系统展示当前同账号在线设备。选择目标设备后，文件通过 WebRTC DataChannel 直接传输。

规则：

- R-PT-01：仅允许同一账号下两个有效登录设备之间发起直传。
- R-PT-02：目标设备必须在线，且 WebSocket 连接已完成鉴权。
- R-PT-03：目标设备必须显式接受请求；默认不自动接收。
- R-PT-04：单个文件上限沿用 200MB。
- R-PT-05：单次批量最多 10 个文件，总大小最多 1GB。
- R-PT-06：直传会话绝对有效期 6 小时；30 秒无应用层心跳自动取消。绝对有效期覆盖弱网下传输 1GB 数据的时间，无心跳超时负责清理僵尸会话。
- R-PT-07：直传完成后文件只保存在接收设备本地；如需进入文件传输助手云端历史，用户必须显式选择“保存到云端”。
- R-PT-08：保存到云端时复用现有媒体上传、消息发送、权限和配额规则，且此时不再承诺服务器不可见。

### 3.2 用户体验

1. 发送端选择目标设备。
2. 服务端向目标设备推送直传请求，只包含文件数量和总大小，不包含文件名。
3. 目标设备接受后，双方建立 WebRTC 连接。
4. 双端显示通道校验码，用户确认一致后才允许传输字节。
5. 发送端通过 DataChannel 发送文件头、分块和完成标记。
6. 接收端逐块写入本地临时文件，并展示真实接收进度。
7. 接收端校验 SHA-256，成功后提示完成。

文件名、MIME、哈希等详细信息只在 DataChannel 内传输，服务器不接触。

### 3.3 云端回退

直传失败时不自动降级为服务器中转。

- R-FB-01：WebRTC 不可用时，客户端提示“直传不可用”。
- R-FB-02：用户可显式选择现有云端文件传输助手路径。
- R-FB-03：云端路径与直传路径在 UI 和协议上分离，不得混用同一个成功语义。
- R-FB-04：禁止默认开启任何明文 WebSocket 文件中转。

## 4. 架构设计

### 4.1 组件分工

```text
API Server
  |-- /ws/tickets：签发一次性 WS Ticket
  |-- /rtc/config：下发 STUN/TURN 配置
  |-- /device-transfers：创建直传会话

Gateway
  |-- 账号设备在线表
  |-- 转发 transfer.signal
  |-- 心跳与连接清理
  |-- 不解析 SDP / ICE / 文件内容

Redis
  |-- 临时 transfer session，TTL 6h
  |-- 一次性 WS Ticket，TTL 60s
  |-- 在线设备心跳，TTL 90s

MySQL
  |-- 账号、设备、会话事实
  |-- 审计事件
  |-- 不存直传字节、文件名或文件哈希

WebRTC
  |-- DTLS 加密 DataChannel
  |-- STUN 打洞
  |-- TURN 加密中继兜底
```

### 4.2 账号设备房间

PairDrop 的房间抽象保留，但房间键改为：

```text
account_id -> device_id -> authenticated connection
```

服务器不再根据 IP 建房，而是根据鉴权后的账号和设备建模。目标设备必须是当前账号的有效设备。

信令转发规则：

- R-SG-01：服务器只允许转发给同账号目标设备。
- R-SG-02：`sender` 身份来自服务端 Principal，不信任客户端自报。
- R-SG-03：`target_device_id` 必须属于同一账号且存在活跃 WS 连接。
- R-SG-04：任一设备会话被吊销后，立即终止相关信令和直传会话。
- R-SG-05：服务器不解析 SDP 和 ICE，只做长度、类型和目标校验。

### 4.3 临时状态存储

直传会话使用 Redis，而不是 MySQL：

```json
{
  "account_id": 123,
  "source_device": "uuid",
  "target_device": "uuid",
  "status": "pending",
  "file_count": 3,
  "total_size": 524288000,
  "created_at": "2026-09-30T10:00:00Z",
  "expires_at": "2026-09-30T11:00:00Z",
  "last_heartbeat_at": "2026-09-30T10:00:10Z"
}
```

状态集合：

```text
pending | connecting | transferring | completed | cancelled | failed
```

状态规则：

- R-ST-01：会话创建时写入 Redis，绝对 TTL 6 小时；心跳更新 `last_heartbeat_at`，但不得延长绝对 TTL。
- R-ST-02：完成或取消后保留 5 分钟供客户端查询，随后过期。
- R-ST-03：Redis 丢失不视为数据事故，客户端可重新发起。
- R-ST-04：MySQL 只写审计摘要，不写文件元数据。

### 4.4 状态归属分析

| 状态 | 归属地 | 原因 |
|---|---|---|
| 账号与设备身份 | MySQL | 长期事实，恢复和审计依赖 |
| 会话吊销状态 | MySQL | 必须全局一致，不能因缓存丢失而失效 |
| WS Ticket | Redis | 60 秒一次性凭证，丢失后重新签发 |
| 设备在线状态 | Redis + Gateway | 可自动恢复，不是业务事实 |
| 直传会话状态 | Redis | 临时过程状态，失败后可重试 |
| 文件元数据 | DataChannel / 客户端内存 | 服务器无需可见，避免元数据泄露 |
| 文件字节 | WebRTC DataChannel | 不进入 API、Gateway 或 MySQL |
| 审计摘要 | MySQL | 合规与风控需要，但必须最小化 |

该表是 PairDrop“逐项审问状态丢了会怎样”的方法迁移：不是简单地说“Redis 快、MySQL 慢”，而是按数据是否可再生、是否需要全局一致、是否属于用户长期资产来决定归属。

## 5. API 与协议需求

### 5.1 一次性 WS Ticket

现状风险：`SPEC-04 R20` 使用 `GET /ws?access_token=...`。Access Token 出现在 URL 中，容易进入代理日志、浏览器历史和运维日志。

新增：

```http
POST /api/v1/ws/tickets
Authorization: Bearer <access_token>
```

响应：

```json
{
  "ticket": "one-time-random-ticket",
  "expires_in": 60
}
```

规则：

- R-TK-01：Ticket 为 256 位密码学随机值。
- R-TK-02：TTL 60 秒。
- R-TK-03：一次性使用，WS 建连成功后立即删除。
- R-TK-04：绑定 user_id、device_id 和 session_id。
- R-TK-05：WS URL 使用 `?ticket=...`，不得携带 Access Token。
- R-TK-06：Ticket 被吊销会话、过期会话或设备退出后立即失效。
- R-TK-07：日志仍必须对 WS 路径脱敏，不记录 query string。

### 5.2 RTC 配置

新增：

```http
GET /api/v1/rtc/config
Authorization: Bearer <access_token>
```

响应：

```json
{
  "ice_servers": [
    {"urls": ["stun:turn.example.com:3478"]},
    {
      "urls": ["turn:turn.example.com:3478?transport=udp"],
      "username": "1717100000:user1",
      "credential": "short-lived-hmac"
    }
  ],
  "expires_in": 600
}
```

规则：

- R-RTC-01：TURN 凭证有效期不超过 10 分钟。
- R-RTC-02：凭证使用时间戳 HMAC 生成，不使用静态共享密钥。
- R-RTC-03：客户端不得把 TURN 凭证写入静态配置或本地长期存储。
- R-RTC-04：配置由服务端下发，便于轮换和灾备切换。

### 5.3 在线设备列表

新增：

```http
GET /api/v1/devices/online
```

响应：

```json
{
  "devices": [
    {
      "device_id": "uuid",
      "device_name": "MacBook Pro",
      "platform": "mac",
      "last_seen_at": "2026-09-30T10:00:00Z"
    }
  ]
}
```

规则：

- R-OD-01：只返回当前账号的在线设备。
- R-OD-02：不返回 IP、公网出口、内网地址或连接所在实例。
- R-OD-03：同一设备重复连接时沿用现有“新连接踢旧连接”规则。

### 5.4 创建直传会话

新增：

```http
POST /api/v1/device-transfers
```

请求：

```json
{
  "target_device_id": "uuid",
  "file_count": 2,
  "total_size": 33554432
}
```

响应：

```json
{
  "transfer_id": "uuid",
  "expires_at": "2026-09-30T11:00:00Z"
}
```

规则：

- R-CR-01：目标设备必须在线且属于当前账号。
- R-CR-02：请求中不包含文件名、哈希和 MIME。
- R-CR-03：服务端只校验数量和总大小限制。
- R-CR-04：创建成功后通过 WS 向目标设备推送 `transfer.request`。
- R-CR-05：同账号每分钟最多创建 10 个直传会话。

### 5.5 WebSocket 传输控制帧

所有帧使用 JSON 文本帧。

服务器到客户端：

| type | 语义 |
|---|---|
| `device.online` | 同账号设备上线 |
| `device.offline` | 同账号设备下线 |
| `transfer.request` | 收到直传请求 |
| `transfer.signal` | 收到不透明 SDP/ICE 信令 |
| `transfer.cancelled` | 对端取消或会话超时 |

客户端到服务器：

| type | 语义 |
|---|---|
| `transfer.accept` | 目标设备接受连接请求 |
| `transfer.reject` | 目标设备拒绝 |
| `transfer.cancel` | 任一端取消 |
| `transfer.signal` | 转发不透明 SDP/ICE |
| `transfer.heartbeat` | 会话心跳 |
| `transfer.complete` | 接收端确认全部文件完成 |

信令约束：

- R-WS-01：`payload` 最大 16KB。
- R-WS-02：单个 ICE candidate 最大 2KB。
- R-WS-03：每个会话最多 100 个 candidate。
- R-WS-04：未知 type 忽略，不直接断连。
- R-WS-05：60 秒内 3 条格式错误消息则断开连接。
- R-WS-06：每会话信令转发速率上限 120 条/分钟。

## 6. 端到端安全需求

### 6.1 通道校验码

WebRTC 的 DTLS 加密不自动防御恶意信令服务器替换 SDP。必须增加用户可校验的通道指纹。

规则：

- R-SEC-01：双方客户端从 DTLS certificate fingerprint 计算短校验码。
- R-SEC-02：校验码为 6 位数字或 4 个短词。
- R-SEC-03：双方 UI 必须展示校验码，由用户确认一致。
- R-SEC-04：用户确认前，客户端不得发送任何文件字节。
- R-SEC-05：校验失败或用户取消时立即关闭 DataChannel 和会话。

这比 PairDrop 的服务器转发更进一步：即使信令服务器被攻破，用户仍有机会发现 SDP 被替换。

### 6.2 元数据最小化

服务器可见：

- 账号 ID
- 源设备 ID
- 目标设备 ID
- 文件数量
- 总大小
- 会话状态
- 传输时长
- 结果

服务器不可见：

- 文件名
- MIME
- 文件哈希
- 文件内容
- 接收端保存路径

审计日志同样不得记录文件名和哈希。

### 6.3 文件完整性

文件协议运行在 DataChannel 内：

1. `file-header`：file_id、name、size、mime、sha256。
2. `chunk`：64KB 二进制块。
3. `partition-received`：每接收 1MB 回执。
4. `file-complete`：接收端校验 SHA-256 后发送。

规则：

- R-INT-01：DataChannel 必须使用 ordered reliable 模式。
- R-INT-02：发送窗口固定 1MB。
- R-INT-03：进度以接收端确认值展示，不以发送端缓冲为准。
- R-INT-04：哈希不匹配时删除本地临时文件并标记失败。
- R-INT-05：V1 不做跨会话断点续传；失败后重新发起。

### 6.4 TURN 安全

- R-TURN-01：TURN 必须部署，作为跨 NAT 兜底。
- R-TURN-02：TURN 只能看到 DTLS 加密后的字节。
- R-TURN-03：TURN 凭证短时效且可撤销。
- R-TURN-04：必须监控 TURN 中继流量和直连比例。
- R-TURN-05：不得用 WebSocket 明文中继替代 TURN。

### 6.5 现有迁移握手加固

当前设备迁移存在 6 位码。受 PairDrop 启发，应按“短生命周期、高限制、哈希存储”加固：

- R-MG-01：迁移码有效期缩短到 10 分钟。
- R-MG-02：最多尝试 5 次，超限立即作废。
- R-MG-03：数据库只存 Argon2id 哈希，不存明文。
- R-MG-04：成功或失败超限后一次性销毁。
- R-MG-05：同账号、同设备、同 IP 维度分别限流。

PairDrop 的 roomSecret 不引入迁移流程；账号设备注册已经是更强的身份源。

## 7. 可靠性与高可用

### 7.1 心跳

- R-HB-01：WS 连接保持现有 30s ping / 60s timeout。
- R-HB-02：直传会话额外每 10s 发送 `transfer.heartbeat`。
- R-HB-03：30s 无心跳将会话标记 `reconnecting`。
- R-HB-04：再 30s 无心跳则自动取消。
- R-HB-05：心跳由应用层处理，不依赖 TCP 或协议层 pong。

### 7.2 信令断线

- 信令 WS 断开时，已建立 DataChannel 可短暂保留。
- 客户端 30 秒内用新 WS Ticket 重连。
- 重连后携带原 transfer_id 恢复会话。
- 超时则双方取消。

### 7.3 Gateway 重启

- Gateway 重启不要求恢复进行中的直传会话。
- 客户端自动重连并重新上报在线状态。
- 未开始的请求可重新发起。
- 已开始的 DataChannel 若仍存活，可在信令恢复后继续；否则取消。

## 8. 性能与容量

目标：

- 10,000 并发 WS 连接下，信令转发 P95 < 100ms。
- 单 Gateway 管理 10,000 连接时心跳流量不超过 5MB/s。
- 直传成功后，API 与 Gateway 的文件带宽为 0；TURN 中继带宽单独计量。
- TURN 中继只承担打洞失败流量。

容量假设：

- 每连接每 30 秒一条心跳。
- 每个直传会话峰值信令不超过 100 条。
- 文件字节不经过 API 和 Gateway。

## 9. 可观测性

必须暴露：

- `ws_connections`
- `ws_ticket_issued_total`
- `ws_ticket_rejected_total`
- `device_transfer_sessions_active`
- `device_transfer_requests_total`
- `device_transfer_success_total`
- `device_transfer_failure_total{reason}`
- `device_transfer_duration_seconds`
- `device_transfer_bytes_total`
- `rtc_direct_ratio`
- `turn_relay_bytes_total`
- `turn_allocations_active`

禁止指标和日志包含：

- 文件名
- 文件哈希
- 文件内容
- SDP
- ICE candidate
- 用户 IP 作为标签

## 10. 多实例与路由

V1 可以先做单 Gateway，但协议和数据结构必须为多实例预留，不能把在线状态只放在某个进程内存里。

### 10.1 在线路由表

Redis 保存设备到 Gateway 实例的路由：

```text
device:route:{device_id}
{
  "account_id": 123,
  "gateway_id": "gateway-1",
  "last_seen_at": "..."
}
```

规则：

- R-ROUTE-01：设备 WebSocket 建连成功后写入路由，TTL 90 秒。
- R-ROUTE-02：每次应用层心跳刷新 TTL。
- R-ROUTE-03：连接断开或心跳超时后删除路由。
- R-ROUTE-04：同一设备新连接踢旧连接时，必须原子替换路由中的 Gateway 信息。
- R-ROUTE-05：路由丢失只导致设备显示离线，不得影响账号和设备事实数据。

### 10.2 跨实例信令

多 Gateway 部署时：

1. Gateway 收到客户端 `transfer.signal`。
2. 校验 sender Principal、transfer session 和目标设备。
3. 查询 Redis 路由表。
4. 通过 Redis Pub/Sub 或 RabbitMQ fanout 将消息投递到目标 Gateway。
5. 目标 Gateway 只投递给对应 device_id 连接。

规则：

- R-MR-01：信令必须先验权再转发。
- R-MR-02：目标 Gateway 不信任来源 Gateway 附带的身份，只信任消息中由来源 Gateway 注入的服务端签名或内部认证。
- R-MR-03：内部跨实例消息必须携带 `created_at`；目标 Gateway 丢弃超过 5 秒的陈旧信令。若使用 RabbitMQ，则同时设置消息 TTL。
- R-MR-04：路由不存在时返回 `device.offline` 或拒绝转发，不得广播到所有 Gateway。
- R-MR-05：跨实例转发失败按临时失败处理，可由客户端重试 WebRTC candidate 交换。

### 10.3 Gateway 故障

- 单个 Gateway 故障时，其连接全部断开。
- 客户端重新签发 WS Ticket，连接剩余 Gateway。
- Redis 路由 TTL 自动清理故障实例上的旧路由。
- 已建立的 DataChannel 若仍可用，可继续传输；控制面恢复后再上报心跳。
- 未建立 DataChannel 的会话自动取消或由用户重试。

## 11. 数据与审计

### 11.1 Redis 数据

| Key | 用途 | TTL |
|---|---|---|
| `ws:ticket:{ticket}` | 一次性 WS Ticket | 60s |
| `device:route:{device_id}` | 设备所在 Gateway | 90s |
| `transfer:session:{transfer_id}` | 直传会话状态 | 6h |
| `rl:transfer:{account_id}` | 账号创建频率限制 | 60s |
| `rl:signal:{transfer_id}` | 信令频率限制 | 60s |

### 11.2 MySQL 审计事件

只记录摘要：

```json
{
  "event_type": "device_transfer.completed",
  "actor_id": 123,
  "detail": {
    "transfer_id": "uuid",
    "source_device_id": "uuid",
    "target_device_id": "uuid",
    "file_count": 3,
    "total_size": 524288000,
    "duration_ms": 45000,
    "result": "completed"
  }
}
```

禁止写入：

- 文件名
- MIME
- SHA-256
- 文件内容
- SDP / ICE

### 11.3 与现有数据模型的关系

直传不写入：

- `messages`
- `message_assets`
- `media_objects`
- `media_references`
- `media_user_references`
- `conversation_settings`

只有用户显式选择“保存到云端”后，才走现有媒体上传和文件传输助手消息链路。此时产生的是新的云端媒体对象和消息，与直传会话没有外键关联。

## 12. 滥用防护

| 风险 | 控制措施 |
|---|---|
| 枚举目标设备 | 只能选择同账号在线设备，不接受任意 device_id |
| 伪造 sender | sender 由服务端 Principal 注入 |
| 信令洪泛 | 每会话 120 条/分钟，candidate 总数 100 |
| 大 payload 攻击 | 单帧 16KB，candidate 2KB |
| 会话堆积 | 每账号每分钟 10 个，绝对 TTL 6 小时，30 秒无心跳取消 |
| 半开连接 | 应用层心跳与 TTL |
| 恶意 SDP 替换 | 用户确认 DTLS 指纹校验码 |
| 服务器明文中继 | 禁止 WS fallback |
| TURN 滥用 | 短时效凭证、分配数与流量监控 |
| 日志泄露 | WS query 脱敏，审计只存摘要 |

## 13. 测试需求

| ID | 场景 | 断言 |
|---|---|---|
| T1 | WS Ticket 一次性使用 | 第二次建连失败 |
| T2 | WS Ticket 过期 | 60 秒后拒绝 |
| T3 | 设备会话吊销后使用 Ticket | 拒绝建连，已有连接断开 |
| T4 | 跨账号目标设备 | 拒绝创建直传 |
| T5 | 目标设备离线 | 拒绝创建直传 |
| T6 | 用户未确认通道校验码 | 不发送文件字节 |
| T7 | 通道校验码不一致 | DataChannel 关闭，会话取消 |
| T8 | 200MB 文件 | 内存有界，SHA-256 校验成功 |
| T9 | 哈希不匹配 | 本地临时文件删除，状态 failed |
| T10 | 信令超限 | 限流或断连 |
| T11 | Gateway 重启 | 客户端重连，会话按规则恢复或取消 |
| T12 | TURN fallback | 传输成功且 API/Gateway 无文件字节 |
| T13 | 审计日志 | 无文件名、哈希和内容 |
| T14 | 显式云端保存 | 走现有上传与消息链路 |
| T15 | 多 Gateway 路由 | 信令只投递到目标实例 |
| T16 | 同设备重连踢旧连接 | 路由被替换，旧连接关闭 |
| T17 | Redis 会话丢失 | 客户端可重新发起，无业务数据损坏 |
| T18 | 迁移码 5 次错误 | 握手作废并审计 |
| T19 | 直传请求拒绝 | 不产生 DataChannel 和云端对象 |
| T20 | 直传完成后不保存云端 | MySQL 无消息和媒体行 |

## 14. 分阶段落地

### Phase 1：安全底座

1. 实现 WS Ticket。
2. 实现 RTC 配置接口。
3. 完善 WS 应用层心跳和在线设备表。
4. 补充信令限流与日志脱敏。
5. 迁移现有 `/ws?access_token=` 方案。

交付价值：即使用户仍使用云端文件传输助手，也消除了 Access Token 进入 URL 的风险。

### Phase 2：直传 MVP

1. 实现账号设备房间。
2. 实现 `transfer.request / accept / reject / cancel / signal / complete`。
3. 客户端实现 DataChannel、通道校验码和文件协议。
4. 接入 Redis 会话状态和审计摘要。

交付价值：同账号设备之间可以不经过服务器字节面传输文件。

### Phase 3：生产可用

1. 部署 coturn。
2. 实现短时效 TURN 凭证。
3. 实现多 Gateway 路由与跨实例信令。
4. 补齐指标、告警和容量报告。
5. 压测 10,000 WS 连接与 TURN 中继比例。

交付价值：直传能力具备公网可用性和运维可观测性。

## 15. 明确不做

1. 不做匿名 IP 房间。
2. 不做公共房间。
3. 不做好友间 P2P 聊天。
4. 不做长期 roomSecret 配对。
5. 不做服务器明文 WS 文件降级。
6. 不做跨账号文件直传。
7. 不做直传断点续传，V1 失败后重新发起。
8. 不把直传结果自动写入云端消息历史。
9. 不承诺云端存储文件的端到端加密。
10. 不用直传替代备份与恢复；备份仍需要可校验清单和原子槽位。

## 16. 开放问题

| 问题 | 建议 |
|---|---|
| Web 客户端是否必须支持直传 | V1 先支持桌面和移动原生客户端；Web 可后置 |
| 是否允许后台自动接收 | 不允许，必须用户显式接受 |
| 通道校验码使用数字还是短词 | 数字实现简单，短词更适合口播确认；可按客户端能力选择 |
| 是否记录 TURN 用量到用户维度 | 记录账号维度用量，不记录文件维度 |
| Redis Pub/Sub 还是 RabbitMQ 路由 | 小规模用 Redis Pub/Sub；需要持久化或更高可靠性时用 RabbitMQ fanout |
| 是否支持局域网直连优化 | WebRTC ICE 已自动优先局域网候选，不做额外发现协议 |

## 17. 验收标准

- 同账号设备可以直传 200MB 文件。
- API 与 Gateway 不接收文件字节。
- 服务器日志与审计不包含文件名、哈希和内容。
- 未确认通道校验码时不能开始传输。
- 目标设备未接受时不能建立连接。
- 跨账号、吊销设备和离线设备均无法发起。
- TURN 兜底可用。
- WS Ticket 一次性且短时效。
- 无明文服务器中转降级。
- 直传失败时用户可显式选择云端路径。
- 直传未选择云端保存时不产生消息和媒体行。
- 多 Gateway 下信令能路由到目标设备所在实例。
