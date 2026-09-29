# SPEC-08 backup（云端备份与恢复、设备迁移）

> 依据：合同 §10、§13.6；交叉评审裁决 2（恢复档案计入 10GB 配额）；合同 §5.7"过期消息不能通过备份或恢复接口绕过保留期"。
> 读者：低等级 LLM 执行者。**全部技术判断已写死；未覆盖情况 → 停下记录 PROGRESS.md。**
> 前置：message/favorite/moment/media 模块完成（备份要读各域快照端口）。

## 1. 范围

**做**：云端备份（单槽位、分片上传包、清单校验、原子替换、30 天过期）、恢复（临时批次→事务切换→只读恢复档案）、设备迁移（点对点传输通道：同一账号两设备经服务端中转）、恢复档案管理（只读/删除/配额）。
**不做**：跨账号导入、备份包导出/分享、恢复写入在线业务表。

## 2. 数据模型（迁移 `migrations/00006_backup.sql`）

```sql
CREATE TABLE backup_slots (
  account_id       BIGINT UNSIGNED NOT NULL,
  active_backup_id BIGINT UNSIGNED NULL,
  updated_at       DATETIME(6) NOT NULL,
  PRIMARY KEY (account_id)
);

CREATE TABLE backups (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id   BIGINT UNSIGNED NOT NULL,
  status       VARCHAR(16) NOT NULL DEFAULT 'creating',  -- creating|ready|restoring|failed|expired|deleted
  scope        JSON NOT NULL,       -- 备份范围快照 {sections:[profile,conv_settings,contact_settings,favorites,moments,messages,transfer]}
  total_size   BIGINT UNSIGNED NOT NULL DEFAULT 0,
  part_count   INT UNSIGNED NOT NULL DEFAULT 0,
  object_key   VARCHAR(255) NOT NULL DEFAULT '',  -- 对象存储前缀 backup/{account}/{id}/
  sha256       CHAR(64) NOT NULL DEFAULT '',      -- 全包哈希（parts 拼接后）
  created_at   DATETIME(6) NOT NULL,
  completed_at DATETIME(6) NULL,
  expires_at   DATETIME(6) NULL,   -- completed_at + 30d
  deleted_at   DATETIME(6) NULL,
  fail_reason  VARCHAR(255) NULL,
  PRIMARY KEY (id),
  KEY idx_backups_account (account_id, status)
);

CREATE TABLE backup_parts (
  backup_id BIGINT UNSIGNED NOT NULL,
  part_no   INT UNSIGNED NOT NULL,        -- 1..N 编号分片（复用编号分片协议语义）
  object_key VARCHAR(255) NOT NULL,
  size      BIGINT UNSIGNED NOT NULL,
  sha256    CHAR(64) NOT NULL,
  uploaded  TINYINT UNSIGNED NOT NULL DEFAULT 0,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (backup_id, part_no)
);

CREATE TABLE backup_manifest_items (     -- 校验清单（每类数据一行）
  backup_id   BIGINT UNSIGNED NOT NULL,
  section     VARCHAR(32) NOT NULL,      -- profile|conv_settings|contact_settings|favorites|moments|messages|transfer|media
  item_count  INT UNSIGNED NOT NULL,
  sha256      CHAR(64) NOT NULL,         -- 该 section JSON 序列化哈希
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (backup_id, section)
);

CREATE TABLE backup_restore_jobs (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id  BIGINT UNSIGNED NOT NULL,
  backup_id   BIGINT UNSIGNED NOT NULL,
  status      VARCHAR(16) NOT NULL DEFAULT 'running',  -- running|succeeded|failed
  fail_reason VARCHAR(255) NULL,
  started_at  DATETIME(6) NOT NULL,
  finished_at DATETIME(6) NULL,
  PRIMARY KEY (id),
  KEY idx_restore_account (account_id, status)
);

CREATE TABLE backup_restore_staging (    -- 临时批次（成功后行迁走，失败全删）
  job_id    BIGINT UNSIGNED NOT NULL,
  section   VARCHAR(32) NOT NULL,
  payload   JSON NOT NULL,               -- 与 restored_* 同构
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (job_id, section),
  KEY idx_staging_job (job_id)
);

CREATE TABLE restored_profile_snapshots (   -- 恢复档案（只读，不过期，计配额）
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id  BIGINT UNSIGNED NOT NULL,
  restore_job_id BIGINT UNSIGNED NOT NULL,
  profile     JSON NOT NULL,              -- {nickname,signature,region,...,conv_settings:[...],contact_settings:[...]}
  size_bytes  BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_rprofile_account (account_id, id)
);

CREATE TABLE restored_message_snapshots (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id  BIGINT UNSIGNED NOT NULL,
  restore_job_id BIGINT UNSIGNED NOT NULL,
  conversation_id   BIGINT UNSIGNED NOT NULL,   -- 快照内保留原会话 ID 仅作分组展示
  conversation_type VARCHAR(16) NOT NULL,
  counterparty JSON NOT NULL,             -- 对端摘要快照（昵称/群名）
  message    JSON NOT NULL,               -- 完整消息视图快照（含 payload）
  orig_message_id BIGINT UNSIGNED NOT NULL,
  orig_created_at DATETIME(6) NOT NULL,
  size_bytes  BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_rmsg_account (account_id, restore_job_id, conversation_id, id),
  KEY idx_rmsg_orig (account_id, orig_message_id)
);

CREATE TABLE transfer_handshakes (        -- 设备迁移中转通道
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id  BIGINT UNSIGNED NOT NULL,
  source_device VARCHAR(36) COLLATE utf8mb4_bin NOT NULL,
  target_device VARCHAR(36) COLLATE utf8mb4_bin NOT NULL,
  status      VARCHAR(16) NOT NULL DEFAULT 'pending',  -- pending|transferring|done|expired|cancelled
  payload_object_key VARCHAR(255) NOT NULL,  -- 中转包（复用备份包格式！裁决：设备迁移=生成一次 backup 格式包直传对象存储，目标设备用同一恢复流程）
  sha256      CHAR(64) NOT NULL,
  expires_at  DATETIME(6) NOT NULL,       -- 创建+1h
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_handshake_account (account_id, status)
);
```

## 3. 领域规则（备份，合同 §10.2/§10.3）

- R1 槽位：`backup_slots` 每账号一行（首次备份时 INSERT）。`active_backup_id` 仅在新备份 status→ready 的同一事务中替换；失败/creating 状态不触碰指针。
- R2 备份创建（**导出为 Worker 异步任务——稳定性裁决：导出可能达 GB 级，绝不允许在 HTTP 请求内同步执行**）：
  1. `POST /backups`（秒级返回）：事务=backup_slots 行锁 → 校验无进行中 creating（有 → STATE_CONFLICT）→ INSERT backups(creating) + `outbox.Emit(tx, {Type:"backup.export", AggregateID: backup_id, Queue:"wechat.lifecycle"})`；
  2. Worker 消费 `backup.export`：调各域端口（§5）**分节流式**收集 JSON → 序列化 → 计算 manifest → 切 part（≤32MB）→ **直接写对象存储**（backup/{account}/{backup_id}/part-{n}，每 part 写完即记 backup_parts.uploaded=1 + sha256）；
  3. 全部 part 完成 → 校验（各 part sha256、manifest 各 section 哈希）→ 事务：backups(ready, completed_at, expires_at=+30d) + backup_slots.active_backup_id 原子替换 + audit；
  4. 任一环节失败 → backups(failed, fail_reason)，旧 ready 备份不动（指针未触碰）；
  5. 客户端 `GET /backups/current` 轮询状态（creating→ready/failed）。
  - Worker 侧幂等：导出任务重投时若 backups.status 已 ready → 直接 ACK；creating 则从"下一个未完成 part"续跑（part 表即断点）。
- R3 备份内容范围（合同 §10.3 白名单，服务端收集，**不接受客户端指定越界数据**）：
  - profile：user_profiles + user_moment_settings + conversation_settings；
  - contact_settings：friend_settings（remark/tag 关联/方向权限）+ contact_tags/contact_tag_members；
  - favorites：favorites(active) + favorite_assets + 标签关联 + 收藏媒体（**收藏媒体对象不复制**——备份包记录 media_object 的 object_key 与哈希，恢复时若对象仍存在则引用，已 GC 则档案中标注"媒体不可用"）；**裁决：收藏媒体体积大，云端备份只备元数据+小媒体（≤5MB 的 image/voice），大文件标 unavailable**——控制备份包规模。
  - moments：自己的动态（含已删？**仅 visible**）+ moment_schedules（含草稿=未发布任务）；
  - messages：创建时仍在保留期内且有权读取的消息（全部会话含 transfer）；
  - 校验清单 manifest。
- R4 绝不备份（合同黑名单）：密码/令牌/设备令牌、群成员资格/角色/禁言/二维码资格/待办权限、他人资料、在线状态。导出器按白名单取数，黑名单天然不在端口内。
- R5 新备份失败：旧 ready 备份不受影响（指针未动）；失败备份 status=failed 保留记录可查看，可删除。
- R6 过期：Worker 扫 `status='ready' AND expires_at<=now` → status=expired + 删对象 + 若 active_backup_id 指向它则置 NULL。
- R7 主动删除：`DELETE /backups/current` → status=deleted + 删对象 + 指针 NULL + audit。

## 4. 领域规则（恢复，合同 §10.4）

- R8 前置校验：登录（鉴权）、backup.account_id=当前账号、status=ready、expires_at>now、manifest 校验通过（重算各 section 哈希）。
- R9 恢复流程（临时批次，严格六步）：
  1. `POST /backups/current/restore` → INSERT backup_restore_jobs(running)，backups.status→restoring；
  2. 逐 section 解析 → 写 backup_restore_staging（**临时表，非在线表**）；
  3. 校验：账号一致、媒体引用存在性（不存在标 unavailable，不失败）、JSON 结构完整；
  4. 事务切换：staging 行 → restored_profile_snapshots / restored_message_snapshots（补 size_bytes），事务内 DELETE staging 该 job 行 + jobs.status=succeeded + backups.status→ready（回）；
  5. 失败：DELETE staging + jobs.status=failed（fail_reason）+ backups.status→ready + audit——**在线数据全程零改动**。
- R10 恢复边界（合同）：不覆盖头像/昵称/设置/好友关系；不写 messages 表；不动在线朋友圈权限；不删现有收藏/消息/媒体；恢复档案独立只读。
- R11 恢复档案：不过期；计入 10GB 配额（size_bytes 求和，quota 端口统一）；可手动删除 `DELETE /restored-archives/{job_id}`（连消息快照一起删）；群聊记录恢复为个人只读快照（conversation_type=group 仅展示）。
- R12 重复恢复同一备份：生成新的 restore job 与新档案（**裁决：允许，档案追加**）；用户可删旧档案。

## 5. 设备迁移（合同 §10.1）

- R13 流程（裁决：复用备份包格式，服务端中转，不做 P2P 直连）：
  1. 新设备 `POST /transfer-handshakes`（已登录同账号）→ 返回 `{handshake_id, code}`，code=6 位数字（10 分钟有效，一次性）；
  2. 旧设备（同账号另一会话）`POST /transfer-handshakes/{id}/accept {code}` → status=transferring，获得备份包上传参数（与 R2 相同的导出+分片流程，payload 指向 handshake）；
  3. 上传完成后 `POST /transfer-handshakes/{id}/complete` → status=done；
  4. 新设备 `POST /transfer-handshakes/{id}/restore` → 走 R9 恢复流程（读中转包）。
- R14 约束：两设备必须同账号有效会话（device 会话校验）；handshake 1 小时过期；旧设备数据不删除；一次性使用（restore 后 status=done 不可再用）。

## 6. API 契约（`/api/v1`，需鉴权）

| # | 方法 路径 | 请求 | 响应 |
|---|---|---|---|
| E1 | POST /backups | `{scope?: 默认全量}` | `{backup_id}`（异步导出，轮询 E2） |
| E2 | GET /backups/current | — | `{backup_id?, status, size, created_at, expires_at, scope, fail_reason?}` |
| E3 | DELETE /backups/current | — | — |
| E4 | POST /backups/current/restore | — | `{restore_job_id}` |
| E5 | GET /restore-jobs?cursor= | — | 恢复任务列表 |
| E6 | GET /restored-archives/{job_id}/profile | — | 档案-资料节 |
| E7 | GET /restored-archives/{job_id}/messages?conversation_id=&cursor= | — | 档案-消息（只读，按会话分组） |
| E8 | DELETE /restored-archives/{job_id} | — | — |
| E9 | POST /transfer-handshakes | — | `{handshake_id, code}` |
| E10 | POST /transfer-handshakes/{id}/accept | `{code}` | 上传参数（同 E1 形状） |
| E11 | POST /transfer-handshakes/{id}/complete | `{parts:[...]}` | — |
| E12 | POST /transfer-handshakes/{id}/restore | — | `{restore_job_id}` |

## 7. 跨模块端口（backup 包定义，各域实现）

```go
type BackupSources interface {
    Profile(ctx, userID int64) (json.RawMessage, error)              // user+conv_settings+moment_settings
    ContactSettings(ctx, userID int64) (json.RawMessage, error)      // contact 域
    Favorites(ctx, userID int64) (json.RawMessage, error)            // favorite 域
    OwnMoments(ctx, userID int64) (json.RawMessage, error)           // moment 域（含草稿任务）
    ReadableMessages(ctx, userID int64) (json.RawMessage, error)     // message 域（180 天内可读全集，分节流式）
}
```
`ReadableMessages` 数据量可能大：**裁决：导出走"服务端写对象，客户端只上传校验分片"不对称流程**——即 E1 返回的 upload_url 实际是服务端生成的 part 下载+回传校验流程过于复杂；简化裁决：**备份内容全部由服务端生成并直接写对象存储，客户端仅调用 complete 触发校验**（E2/E3 的分片 PUT 由服务端内部完成，API 对客户端表现为：POST /backups（异步开始）→ GET /backups/current 轮询 status=creating→ready）。**此裁决消除客户端上传复杂度，且"完整上传、校验和清单生成成功后才覆盖"语义不变。** E1 响应改为 `{backup_id}`，客户端轮询 E4。

## 8. 幂等与并发

- I1：同账号并发创建备份：backup_slots 行锁（`SELECT ... FOR UPDATE`）+ creating 状态检查 → 第二个 STATE_CONFLICT。
- I2：complete 幂等：status 条件更新（creating→ready）；重复 complete 对 ready 返回当前状态。
- I3：指针替换原子：slots.active_backup_id 与 backups.ready 同事务。
- I4：恢复任务并发：restoring 状态检查（同备份同时只一个恢复）。
- I5：staging 清理：jobs 失败即删，且 Worker 兜底扫描 running>30min 的僵尸 job（Worker 重启场景）。

## 9. 测试用例

| ID | 用例 | 断言 |
|---|---|---|
| T1 | 创建备份全流程 | 各 section manifest 齐全；ready；指针替换 |
| T2 | 新备份 creating 中旧备份仍可恢复 | 指针未动 |
| T3 | 新备份失败 | 旧 ready 完整 |
| T4 | 校验失败（篡改 part 哈希） | failed，不替换 |
| T5 | 备份内容黑名单 | 导出 JSON 无 token/群资格/他人资料字段 |
| T6 | 30 天过期 Worker | expired+对象删除+指针 NULL |
| T7 | 恢复成功 | 档案可读；在线 messages/user 表零变化 |
| T8 | 恢复失败（清单损坏） | staging 删除、账号数据不变、备份回 ready |
| T9 | 非本账号备份恢复 | FORBIDDEN |
| T10 | 过期备份恢复 | STATE_CONFLICT |
| T11 | 恢复档案配额 | 计入 10GB |
| T12 | 删除恢复档案 | 连消息快照删；配额释放 |
| T13 | 迁移握手：错 code/超时/一次性 | 精确拒绝/过期/不可重用 |
| T14 | 迁移恢复后旧设备数据不动 | R14 |
| T15 | 收藏大媒体备份 | 标 unavailable 不失败 |

## 10. 任务分解

1. 迁移 00006_backup.sql。
2. `internal/backup/store.go` → `exporter.go`（BackupSources 端口聚合+分节序列化）→ `service.go`（R1-R12）→ `handler.go`（E1-E10）。
3. 单测 T1-T12（各域端口 stub）。
4. 迁移子域：handshake（R13-R14）+ E11-E14 + 单测 T13-T14。
5. Worker 接入点：`ExpireBackups` / `ReapZombieRestoreJobs`（SPEC-10 调度）。
6. 边界脚本 DOMAINS 加入 backup。

## 11. 验收

单槽位单有效备份、失败不覆盖、30 天过期、恢复六步事务、恢复档案只读+不过期+计配额、黑名单数据绝不入包、迁移通道一次性、全程不写在线业务表。
