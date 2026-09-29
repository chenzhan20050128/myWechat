# SPEC-03 media（对象存储与上传底座）

> 依据：合同 §8.2/§8.3（上传部分）、§15.3；ADR-002/005。状态：已评审。

## 1. 范围

**做**：上传会话（含分片）、完成校验（服务端重算大小/MIME/SHA-256）、媒体对象与业务引用/用户访问引用表、鉴权下载地址、未完成上传 24h 过期、头像绑定支撑（SPEC-01 R27）。
**不做**：缩略图/转码流水线（Worker 阶段二接入，表已建 `media_variants`）、存储清理（阶段四）、媒体 180 天过期（随消息，阶段二）、配额扣减（阶段四统一）。

## 2. 领域规则

### 2.1 上传会话（合同 §8.3）
- R1：`POST /media/uploads {file_name, size, mime, sha256, purpose}` → 创建 `upload_sessions(id=UUID, owner_id, …, chunk_size, status=open, expires_at=now+24h)`。
- R2：`size` ≤ 200MB（合同单对象上限）；`purpose ∈ {message, moment, favorite, avatar, transfer}`（阶段一只校验枚举与归属，不消费）。
- R3：>10MB 必须分片（合同 §8.3）；服务端按 size 决定 `chunk_size`（≤10MB: 单片；>10MB: 5MB 固定），`total_chunks=ceil(size/chunk_size)`。（design-review D3：编号分片与 S3 MultipartUpload API 同构——part number ↔ UploadPart，未来客户端直传 S3 时数据模型零迁移；弱网乱序/并发上传友好。）
- R4：`PUT /media/uploads/{id}/chunks/{n}`：body=分片字节；仅 owner 可操作、会话 open 且未过期；分片大小必须=chunk_size（最后一片可小）；重复上传同一片按内容覆盖但完成时以服务端重算哈希为准（幂等安全）。
- R5：分片暂存 `STORAGE` 的 `tmp/<upload_id>/<n>` key；`GET /media/uploads/{id}` 返回已收到分片位图。
- R6：`POST /media/uploads/{id}/complete`：事务外拼接分片→临时流，服务端重算 size/mime(嗅探)/sha256，与声明比对，不符 → `INVALID_ARGUMENT`（会话保留可重传）；一致则事务内写 `media_objects(status=ready, …)` + 标记会话 `completed`，随后异步触发 media.process 事件（MQ_DRIVER=none 时仅落 outbox 行）。
- R7：过期会话（>24h 未完成）由 `media.ExpireUploads(ctx)` 清理：删 tmp 分片、会话置 `expired`。阶段一提供方法，调度接入阶段二 Worker。
- R8：MIME 白名单：image/{jpeg,png,webp,gif}、video/mp4、audio/{mpeg,aac,amr}、application/octet-stream（文件）。头像另受 SPEC-01 R27 约束。
- R9：`sha256` 声明值 64 位小写 hex；服务端不信任客户端 MIME 与大小（合同 §8.3"服务端重新计算"）。

### 2.2 对象与引用（合同 §8.2）
- R10：`media_objects(id, owner_id, bucket_key, size, sha256 UNIQ 不做全局去重——合同未要求秒传，索引即可, mime, status, created_at)`。status：`ready|cleaned|deleted`（processing/uploading 态由会话承担，对象落库即 ready；阶段二引入处理流水线时再前置 processing）。
- R11：`media_references(id, object_id, biz_type, biz_id, created_at)`：业务引用（消息/动态/收藏/头像）。阶段一仅 avatar 绑定写入 biz_type=`avatar`。
- R12：`media_user_references(object_id, user_id, granted_by_ref_id, revoked_at NULL)`：用户访问引用。头像对象：owner 自身 + 按资料可见性在下载时实时判定（阶段一简化：头像下载走 `/media/objects/{id}/download`，鉴权规则=对象 owner 本人 或 任意登录用户——头像属公开资料部件，合同 §3.4 未限制头像可见性）。
- R13：同一对象多用户可读时，任一用户的访问引用撤销不影响他人（合同 §8.2；清理执行在阶段四，本模块保证引用模型可表达该语义）。

### 2.3 下载（合同 §8.3/§15.3）
- R14：`GET /media/objects/{id}/download-url`：鉴权（见 R12/各 biz 规则，阶段一：owner 或 avatar 场景）→ 返回短期 URL（S3 预签名 15 分钟 / local 驱动返回 `/media/download?key=..&exp=..&sig=..` HMAC 签名，由 API 代理输出字节）。
- R15：权限变化/清理后重新获取地址必须失败（合同 §15.3）——下载地址不持久化，每次重新鉴权生成。
- R16：local 驱动代理端点校验 HMAC 签名与过期，失败 `FORBIDDEN`。

## 3. 数据模型

- `upload_sessions(id CHAR(36) PK, owner_id, file_name, declared_size, declared_mime, declared_sha256, purpose, chunk_size, total_chunks, status, expires_at, created_at, updated_at)`
- `upload_chunks(upload_id, chunk_index, size, received_at, PK(upload_id, chunk_index))`——**分片到达的唯一事实源**（SSOT：会话表不设 received_chunks 反范式计数列，位图与完成度判定一律由本表派生）
- `media_objects(id, owner_id, bucket_key UNIQ, size, sha256, mime, purpose, status, created_at, updated_at, INDEX(owner_id), INDEX(sha256))`
- `media_variants(id, object_id, kind, bucket_key, size, width, height, duration_ms, status, created_at)`（阶段二消费）
- `media_references(id, object_id, biz_type, biz_id, created_at, INDEX(object_id), INDEX(biz_type,biz_id))`
- `media_user_references(id, object_id, user_id, granted_by_ref_id, revoked_at, created_at, INDEX(object_id,user_id))`

## 4. API 契约

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | /api/v1/media/uploads | R1–R3 |
| GET | /api/v1/media/uploads/{id} | R5 |
| PUT | /api/v1/media/uploads/{id}/chunks/{n} | R4（raw body，`Content-Type: application/octet-stream`） |
| POST | /api/v1/media/uploads/{id}/complete | R6 → `{media_object_id}` |
| POST | /api/v1/media/uploads/{id}/abort | 主动放弃：删分片、会话 aborted |
| GET | /api/v1/media/objects/{id}/download-url | R14 |
| GET | /api/v1/media/download | R16（local 驱动代理，签名校验） |

## 5. 幂等与并发

- complete 幂等：会话 status 条件更新 `open→completed`，并发/重复调用第二个返回首个的 `media_object_id`（先查会话关联对象）。
- 分片覆盖写安全：同一片重复 PUT 字节覆盖，最终哈希校验兜底。
- 拼接在临时文件进行，失败不产生半成品对象（对象行只在全部校验通过后落库）。

## 6. 验收标准

- A1 >10MB 拒绝单片上传路径（total_chunks>1）；≤10MB 单片直传成功。
- A2 声明 sha256/大小与实际不符 → `INVALID_ARGUMENT`，无对象落库。
- A3 完成幂等：两次 complete 返回同一 media_object_id，仅一个对象。
- A4 非 owner 操作上传会话 → `RESOURCE_UNAVAILABLE`。
- A5 下载地址过期/签名篡改 → 403/401；权限撤销后重新获取地址失败（阶段一以 avatar 场景验证）。
- A6 24h 过期清理方法删除分片且会话置 expired（单测覆盖，调度属阶段二）。
