# SPEC-01 auth / user / device（含 conversation 最小集）

> 依据：合同 §3、§5.1-5.2（仅会话创建）、§16.2；交叉评审无冲突项。状态：已评审。

## 1. 范围

**做**：注册、双标识登录、令牌签发/刷新/轮换、登录锁定、修改密码、临时密码强制改密、设备列表/退出、个人资料读写、注册副作用（默认资料/设备会话/文件传输助手会话/默认朋友圈设置占位）。
**不做**：短信验证码、自助找回密码（合同 §3.2 明确 V1 不做）、消息发送、朋友圈设置的实际应用（阶段三）。

## 2. 领域规则

### 2.1 注册（合同 §3.1）
- R1：字段：`phone`（必填、全局唯一、E.164 简化校验 `^\+?[0-9]{6,15}$`）、`password`、`account_name`、`nickname`。
- R2：密码 8–32 字符，必须同时含字母与数字；否则 `INVALID_ARGUMENT`。
- R3：`account_name` 6–20 位，仅小写字母/数字/下划线，规范化（trim+lower）后全局唯一（`utf8mb4_bin` 精确比对，ADR-011）；注册后不可修改。
- R4：`nickname` 2–32 字符。
- R5：字段不得含控制字符/首尾空白（规范化后为空即拒绝）；保留词黑名单（`admin`,`system`,`wechat`,`official`,`support`,`filehelper`）拒绝。
- R6：重复 phone 或 account_name 返回 `STATE_CONFLICT`（唯一索引兜底，并发安全，禁止先查再插——合同 §12.4）。
- R7：注册成功在**一个事务**内创建：users、user_profiles、当前 device+session、transfer 会话及成员、默认朋友圈设置行（`user_moment_settings`，阶段一仅建行）。失败整体回滚。
- R8：注册成功即视为登录，返回令牌对。审计：`auth.register`。

### 2.2 登录（合同 §3.2）
- R9：支持 `phone+password` 与 `account_name+password`，由 `login_id` 格式自动区分（含数字/`+` 开头且匹配 R1 按 phone，否则按 account_name）。
- R10：失败统一 `INVALID_CREDENTIALS`；成功前检查冻结标记（cache `lock:login:<user_id>`，存在即拒绝并同样返回 `INVALID_CREDENTIALS` 附带剩余秒数于 message，不区分原因）。
- R11：连续失败 10 次（cache 计数器，窗口内递增）→ 冻结 10 分钟；成功后清零。
- R12：`must_change_password=true`（临时密码）时登录成功但响应标记该标志，除改密接口外其余接口由中间件拒绝（`STATE_CONFLICT must_change_password`）。
- R13：审计：`auth.login.success` / `auth.login.failure`（含 IP、设备名，不记密码）。
- R13a：登录成功且存量哈希参数旧于当前配置（PHC 串解析比对）→ 透明 rehash 更新（design-review D10）。

### 2.3 令牌（ADR-003，经 design-review D1/D2 推敲定稿）
- R14：Access 30 分钟、Refresh 30 天，DB 只存 SHA-256 哈希。**单活 access 令牌**：refresh 后旧 access 立即失效；客户端契约 = 收到 401 必须先 refresh 再重试一次。
- R15：`POST /auth/refresh` 携带 refresh：校验哈希与 `user_sessions.refresh_token_hash` 匹配且未吊销未过期 → 事务内轮换（新 refresh 哈希覆盖、新 access 写入、旧 access cache 删除）。限流：每 session 每分钟 10 次。
- R16：旧 refresh 重放 → 吊销该 session（`revoked_at`），返回 `UNAUTHENTICATED`。审计 `auth.refresh.reuse_revoked`（RFC 9700 重放检测语义）。
- R17：refresh 旋转时重置 refresh 过期为新的 30 天。

### 2.4 密码（合同 §3.2）
- R18：改密需旧密码校验 + 新密码满足 R2；成功事务内更新哈希、`must_change_password=false`、**吊销除当前 session 外该用户全部 session**（DB 置 revoked + 删 cache）。审计 `auth.password.changed`。
- R19：运营重置（阶段一仅提供 service 方法 `ResetPasswordByOperator`，无 HTTP 面；运营端属阶段六 operator 模块）：生成 16 位一次性临时密码（字母+数字），置 `must_change_password=true`，吊销全部 session，审计 `auth.password.reset_by_operator`。

### 2.5 设备（合同 §3.3）
- R20：设备字段：device_id、设备名（客户端上报，≤64 字符，缺省 `unknown`）、platform（`ios|android|windows|mac|web|unknown`）、首次登录、最后活跃、最近 IP。
- R21：`GET /devices` 列出未吊销 session 对应设备（含 current 标记）。
- R22：`DELETE /devices/{device_id}` 退出指定设备：吊销其全部活跃 session；目标是当前设备自身 → `INVALID_ARGUMENT`（合同"不能误退出自身"，当前设备用 `/auth/logout`）。
- R23：`POST /devices/logout-others` 吊销除当前外全部 session。
- R24：`POST /auth/logout` 吊销当前 session。
- R25：被吊销 session 的后续请求立即 `UNAUTHENTICATED`（cache 删除 + DB 兜底）。审计 `auth.device.revoked`。

### 2.6 资料（合同 §3.4）
- R26：`GET /users/me`、`PATCH /users/me`：nickname≤32、gender∈{unspecified,male,female}、region{country,province,city}、signature≤100、status≤32（可清除为空串）。
- R27：头像走 media：`POST /users/me/avatar {media_object_id}`——校验对象为当前用户所有、状态 ready、MIME∈{image/jpeg,image/png,image/webp}、≤10MB，然后绑定 `user_profiles.avatar_media_id`。审计 `user.profile.updated`。
- R28：`GET /users/{id}` 仅返回 id/nickname/avatar/account_name（非好友不泄露手机号/地区/签名，合同 §4.5；好友可见更多信息由 contact 读路径组装，阶段一保持一致从简）。
- R29：资料修改不追溯历史快照（合同 §3.4）。

### 2.7 conversation 最小集（阶段一切面）
- R30：类型枚举 `direct|group|transfer|official_service`；阶段一只创建 `direct` 与 `transfer`。
- R31：`CreateTransferConversation(tx, userID)`：每用户唯一（`conversations.transfer_owner_id` 唯一索引兜底），幂等。
- R32：`CreateDirectConversation(tx, a, b)`：成员对唯一（生成列 `direct_key = SHA2(CONCAT(LEAST(m1,m2),':',GREATEST(m1,m2)))` 唯一索引），已存在则复用返回。供 contact 同意申请时调用。
- R33：`IsMember(tx, conversationID, userID)` 供鉴权复用。

## 3. 数据模型（迁移 0001 相关表）

- `users(id, phone UNIQ, account_name UNIQ, password_hash, must_change_password, status, created_at, updated_at)`
- `user_profiles(user_id PK/FK, nickname, gender, region_country, region_province, region_city, signature, status_text, avatar_media_id NULL)`
- `user_devices(id, user_id, device_name, platform, first_login_at, last_active_at, last_ip)`（device_id 由服务端生成 UUID，客户端首次登录可带 name/platform）
- `user_sessions(id, user_id, device_id, access_token_hash UNIQ, access_expires_at, refresh_token_hash UNIQ, refresh_expires_at, created_at, last_seen_at, last_ip, revoked_at NULL, revoke_reason)`
- `user_moment_settings(user_id PK, notify_enabled DEFAULT 1)`（占位，阶段三使用）
- `conversations(id, type, transfer_owner_id NULL UNIQ, direct_key BINARY(32) NULL UNIQ, last_seq DEFAULT 0, created_at)`
- `conversation_members(conversation_id, user_id, membership_epoch, joined_at, left_at NULL, UNIQ(conversation_id,user_id,membership_epoch))`

## 4. API 契约（阶段一开放面）

| 方法 | 路径 | 说明 | 主要错误 |
|---|---|---|---|
| POST | /api/v1/auth/register | R1–R8 | INVALID_ARGUMENT, STATE_CONFLICT |
| POST | /api/v1/auth/login | R9–R13 | INVALID_CREDENTIALS |
| POST | /api/v1/auth/refresh | R15–R17 | UNAUTHENTICATED |
| POST | /api/v1/auth/logout | R24（鉴权） | — |
| POST | /api/v1/auth/password | R18（鉴权） | INVALID_CREDENTIALS, INVALID_ARGUMENT |
| GET | /api/v1/devices | R21（鉴权） | — |
| DELETE | /api/v1/devices/{deviceID} | R22（鉴权） | INVALID_ARGUMENT, RESOURCE_UNAVAILABLE |
| POST | /api/v1/devices/logout-others | R23（鉴权） | — |
| GET | /api/v1/users/me | R26（鉴权） | — |
| PATCH | /api/v1/users/me | R26（鉴权） | INVALID_ARGUMENT |
| POST | /api/v1/users/me/avatar | R27（鉴权） | INVALID_ARGUMENT, RESOURCE_UNAVAILABLE |
| GET | /api/v1/users/{id} | R28（鉴权） | RESOURCE_UNAVAILABLE |

响应 data 形状：register/login/refresh → `{user_id, access_token, access_expires_at, refresh_token, refresh_expires_at, device_id, must_change_password}`。

## 5. 幂等与并发

- 注册/登录无客户端幂等键；并发唯一性全部由 DB 唯一索引保证（合同 §12.4 禁止先查再插）。
- refresh 并发：同一 session 两个 refresh 竞争 → 行锁序列化，后到者因哈希不匹配走 R16 吊销（保守安全）。
- 改密与登录并发：改密事务提交前旧 session 仍可用，提交后 cache 删除立即生效。

## 6. 验收标准（映射合同 §18.1）

- A1 重复手机号/账号名注册失败（唯一索引验证）。
- A2 错误密码与不存在账号返回完全相同结构（`INVALID_CREDENTIALS`），计时差异 < 常量噪音（对不存在账号也执行一次 Argon2 校验对抗时序探测）。
- A3 连续 10 次失败后第 11 次即冻结；10 分钟后恢复。
- A4 改密后其他设备 access/refresh 立即失效，当前设备不受影响。
- A5 退出指定设备后其 access token 立即 401；当前设备不能被该接口退出。
- A6 注册成功响应含 transfer 会话可用前提（`conversations.type=transfer` 行存在）。
- A7 临时密码登录返回 `must_change_password=true`，未改密前访问 `/users/me` 被拒。
