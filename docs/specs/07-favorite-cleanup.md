# SPEC-07 favorite（收藏）与 storage-cleanup（存储清理）

> 依据：合同 §8（8.1 收藏 / 8.2 媒体引用 / 8.4 存储清理）、§13.5；交叉评审裁决 2（双层配额：物理字节 + 逻辑 10GB）。
> 读者：低等级 LLM 执行者。**全部技术判断已写死；未覆盖情况 → 停下记录 PROGRESS.md。**
> 前置：media 模块（SPEC-03）必须先完成（收藏媒体副本、GC 队列均依赖 media 表与服务）。

## 1. 范围

**做**：收藏（单条消息/聊天记录 1..100 条连续、快照、独立媒体副本、标签 ≤100 个）、收藏列表/详情/删除、媒体引用查询、存储清理任务（四类范围+四类筛选+预览+二次确认+逐对象状态+回收站 7 天）、配额展示。
**不做**：跨会话全局搜索（合同明确排除收藏之外的关键词检索？"聊天检索"在过期规则中被提及但 §18 验收未列全文检索——**裁决：V1 收藏列表仅按类型/标签/时间过滤，不做全文搜索**）。

## 2. 数据模型（迁移 `migrations/00005_favorite.sql`）

```sql
CREATE TABLE favorites (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  owner_id    BIGINT UNSIGNED NOT NULL,
  kind        VARCHAR(16) NOT NULL,     -- text|emoji|image|video|voice|file|link|card|location|chat_record
  content     JSON NOT NULL,            -- 内容快照（各 kind schema 见 §3）
  source_message_id BIGINT UNSIGNED NULL,
  source_sender_id  BIGINT UNSIGNED NULL,
  source_sent_at    DATETIME(6) NULL,
  source_type  VARCHAR(16) NULL,        -- 原消息类型快照
  total_size  BIGINT UNSIGNED NOT NULL DEFAULT 0,  -- 副本物理字节合计（配额计入）
  status      VARCHAR(16) NOT NULL DEFAULT 'active',  -- active|deleted
  active_flag TINYINT UNSIGNED GENERATED ALWAYS AS (IF(status='active', 1, NULL)) STORED,
  deleted_at  DATETIME(6) NULL,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_fav_dedupe (owner_id, source_message_id, active_flag),  -- I1 幂等兜底
  KEY idx_fav_owner (owner_id, status, id)
);

CREATE TABLE favorite_tags (
  owner_id  BIGINT UNSIGNED NOT NULL,
  tag       VARCHAR(32) NOT NULL,       -- ≤32 字符
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (owner_id, tag)
);

CREATE TABLE favorite_tag_items (
  favorite_id BIGINT UNSIGNED NOT NULL,
  tag         VARCHAR(32) NOT NULL,
  PRIMARY KEY (favorite_id, tag),
  KEY idx_ftag_tag (tag, favorite_id)
);
-- 完整性：favorite_tag_items.tag ∈ 该 owner 的 favorite_tags（服务层校验，无外键，遵循 0001 风格）

CREATE TABLE favorite_assets (          -- 收藏内条目顺序（chat_record 的 100 条）
  favorite_id BIGINT UNSIGNED NOT NULL,
  position    SMALLINT UNSIGNED NOT NULL,
  item_snapshot JSON NOT NULL,          -- 单条快照（消息视图，含 sender/type/payload 快照）
  PRIMARY KEY (favorite_id, position)
);

CREATE TABLE favorite_media_objects (   -- 独立媒体副本（不与消息媒体共用生命周期）
  favorite_id     BIGINT UNSIGNED NOT NULL,
  position        SMALLINT UNSIGNED NOT NULL,
  media_object_id BIGINT UNSIGNED NOT NULL,   -- 指向 media_objects 的一行，object_key 前缀 fav/
  created_at      DATETIME(6) NOT NULL,
  PRIMARY KEY (favorite_id, position),
  KEY idx_favmedia_obj (media_object_id)
);

CREATE TABLE storage_cleanup_jobs (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id     BIGINT UNSIGNED NOT NULL,
  scope       VARCHAR(16) NOT NULL,     -- message|moment|transfer|upload
  filter      JSON NOT NULL,            -- {conversation_id?, media_type?, created_before?, min_size?}
  preview     JSON NOT NULL,            -- 预览结果快照 {count,total_bytes,affected_messages}
  status      VARCHAR(16) NOT NULL DEFAULT 'previewed', -- previewed|confirmed|running|done|partial|failed
  confirmed_at DATETIME(6) NULL,
  finished_at DATETIME(6) NULL,
  created_at  DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_cleanup_user (user_id, status)
);

CREATE TABLE storage_cleanup_items (
  job_id     BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  cleanup_id BIGINT UNSIGNED NOT NULL,
  media_object_id BIGINT UNSIGNED NOT NULL,
  reference_id BIGINT UNSIGNED NOT NULL,   -- 被清理的 media_user_references.id
  state      VARCHAR(16) NOT NULL,          -- pending|success|failed
  error      VARCHAR(255) NULL,
  updated_at DATETIME(6) NOT NULL,
  PRIMARY KEY (job_id),
  KEY idx_cleanup_items_job (cleanup_id, state)
);
```
（`media_gc_queue` 与回收站表属 media 域：**SPEC-03 已含或在其迁移补齐**——若 0001 未建 `media_gc_queue`，本规格任务 1 在 00005 中补：`(media_object_id PK, enqueued_at, purge_after = enqueued_at + 7d, state queued|purged)`。）

## 3. 收藏内容快照 schema（content JSON 固定）

| kind | content | 备注 |
|---|---|---|
| text/emoji | `{"content": string}` | |
| image/video/voice/file | `{"media_object_id": "...", ...原消息 payload 字段}` | 副本=新建 media_objects 行（object_key=`fav/{owner}/{uuid}`），物理复制由 media.CopyObject 完成 |
| link | `{"url","title","summary","thumb_media_id"?}` | 缩略图同上复制 |
| card | `{"user_id","nickname","account_name","avatar_media_id"?}` | 快照，不复制头像 |
| location | `{"country","province","city","place_name"}` | |
| chat_record | `{"title": string, "count": int}` | 条目在 favorite_assets；title=自动"与 X 的聊天记录"或用户传 |

## 4. 领域规则（收藏，合同 §8.1）

- R1 创建入口 `POST /favorites`：
  - 单条：`{source: {message_id}}` 或直接新建收藏 `{kind, content, assets?}`（不来自消息的自建收藏，如直接粘贴文字）；
  - 聊天记录：`{source: {message_ids: [...]}}`，1..100 条、同会话、seq 连续（`conversation_seq` 严格递增 1）、**发起者每条都可读**（SPEC-04 R15 谓词复用——通过 message 模块端口）。
- R2 幂等：重复收藏同一**单条消息**不产生第二条——`source_message_id` 在 owner 范围查 active 收藏命中即返回原 `{favorite_id}`（**裁决：不加唯一键**，因为删除后可重新收藏；查重=服务层 SELECT）。聊天记录收藏不查重（每次都是新收藏）。
- R3 快照原子性：单事务=INSERT favorites + favorite_assets + favorite_tag_items + 媒体副本行（media.CopyObject 物理复制**在事务前完成**，事务失败则补偿删除副本——**裁决：先复制后入库，失败走 media 的孤儿对象清理（fav/ 前缀对象 24h 无引用即回收，复用未完成上传清理任务）**）。
- R4 不受原消息影响：撤回/过期/清理/好友变化/群解散后收藏仍完整可读（快照独立，读取不回查消息表）。
- R5 删除收藏：软删（status=deleted）；收藏媒体副本引用同事务删除 → media.OnReferencesRemoved → 无引用入 GC 队列。删除收藏不影响原消息。
- R6 标签：≤100 个/用户；单个 ≤32 字符；重名幂等（主键 UPSERT）；打标签时校验收藏属于本人且标签存在。
- R7 列表：`GET /favorites?kind=&tag=&before_id=&limit=50`（keyset：created_at desc, id desc）。
- R8 权限：仅创建者可读/改标签/删（owner_id 匹配，他人 NOT_FOUND）。
- R9 配额：收藏副本字节计入 10GB 逻辑配额（favorites.total_size 求和 + media 模块既有配额口径，**裁决：配额统一由 media.QuotaRemaining 提供，favorite 创建前查询，事务内不再复查**——超限 QUOTA_EXCEEDED）。

## 5. 领域规则（存储清理，合同 §8.4）

- R10 清理对象=**当前用户的 media_user_references**（用户访问引用），从不直接删对象实体。四类 scope：
  - message：`source_type='message' AND source_user_id=me` 的 user_references，按 filter（conversation_id/media_type/created_before/min_size）筛选；
  - moment：source_type='moment' 同理；
  - transfer：source_type='message' 且 conversation 属于我的 transfer 会话；
  - upload：`upload_sessions` 中我的 expired 未完成会话（直接清 upload 行+已传分片对象）。
- R11 三段式流程（严格）：
  1. `POST /storage/cleanup/previews` `{scope, filter}` → 服务端查询候选并**落 storage_cleanup_jobs(status=previewed, preview=快照)**，返回 `{cleanup_id, items:[{media_object_id, source, size, referenced_by_others:bool}], total_bytes, count, affected_messages}`；
  2. `POST /storage/cleanup/{id}/confirm` `{item_ids?:[...]}`（可子集）→ status=confirmed，items 全部落 storage_cleanup_items(pending)；
  3. Worker（或同步执行，**裁决：同步执行，条目数 ≤1000 时直接在 confirm 事务外逐条处理**；条目互独立、失败标 failed 可重试 `POST /storage/cleanup/{id}/retry`）。
- R12 逐条清理动作：`DELETE FROM media_user_references WHERE id=? AND source_user_id=me` → `media.OnReferencesRemoved(object)`：仍有任何业务引用（message/moment/favorite）→ 不动对象；无 → 入 media_gc_queue（purge_after=now+7d）。
- R13 保护：不清理他人引用（SQL 条件 source_user_id=me 天然保证）；当前备份相关对象不进候选（`source_type='backup'` 排除）；共享对象只删我的引用（展示 referenced_by_others=true 提示）。
- R14 清理后：消息/动态正文保留，媒体出"媒体已清理"（读取时 user_reference 不存在 → payload 原样但 assets 空 + `cleaned:true` 标志——**由 message/moment 读取层判定，本模块只删引用**）；旧下载地址（签名 URL）重新鉴权时失效（签名 URL 只授对象读取，鉴权层查 user_reference，已删 → 拒绝）；收藏副本不受影响。
- R15 回收站：media_gc_queue 7 天后 Worker 物理删除（调 storage.Delete + DELETE media_objects 行）；中断可重试；对象独立成功/失败。
- R16 清理设备缓存：纯客户端概念，无服务端接口（文档声明即可）。

## 6. API 契约（`/api/v1`，需鉴权）

| # | 方法 路径 | 请求 | 响应 |
|---|---|---|---|
| D1 | POST /favorites | §3/R1 | `{favorite_id}`（幂等命中返回原 id） |
| D2 | GET /favorites?kind=&tag=&before_id=&limit= | — | 50 条+游标 |
| D3 | GET /favorites/{id} | — | 详情（含 chat_record 条目） |
| D4 | DELETE /favorites/{id} | — | — |
| D5 | POST /favorites/{id}/tags | `{tags:[..]}` / DELETE `{tags}` | — |
| D6 | GET /favorite-tags | — | 标签列表 |
| D7 | POST /favorites/tags | `{tag}` | — |
| D8 | GET /storage/usage | — | `{physical_bytes, logical_bytes, quota_bytes, breakdown:{messages,moments,transfer,favorites}}` |
| D9 | POST /storage/cleanup/previews | `{scope, filter}` | R11.1 |
| D10 | POST /storage/cleanup/{id}/confirm | `{item_ids?}` | `{processed, failed}` |
| D11 | GET /storage/cleanup/{id} | — | 逐条状态 |
| D12 | POST /storage/cleanup/{id}/retry | — | — |

## 7. 跨模块端口（消费方定义）

```go
// message 端口（favorite 需要）
type MessageReader interface {
    // ReadableBatch 校验并返回一批消息的完整快照（R1/R15 谓词）。
    ReadableBatch(ctx, userID int64, messageIDs []int64) ([]MsgSnapshot, error)
    // TransferConversationID 返回用户 transfer 会话 ID。
    TransferConversationID(ctx, userID int64) (int64, error)
}
// media 端口（favorite/cleanup 需要；SPEC-03 落地）
type MediaPort interface {
    CopyObject(ctx, srcObjectID int64, newOwner int64, keyPrefix string) (newObjectID int64, err error)
    OnReferencesRemoved(ctx, objectIDs []int64) error
    QuotaRemaining(ctx, userID int64) (int64, error)
    UserReferences(ctx, userID int64, f RefFilter) ([]RefItem, error)
}
```

## 8. 幂等与并发

- I1：单条消息收藏幂等由 `uk_fav_dedupe (owner_id, source_message_id, active_flag)` 生成列唯一键兜底（与 group_members/official_followers 同模式）：并发重复收藏后提交者撞键 → 回滚 → 回读已有行返回原 `{favorite_id}`。source_message_id 为 NULL（chat_record/自建）不受该键约束。
- I2：清理 job 状态机单向 previewed→confirmed→done/partial，confirm 幂等（重复 confirm 对 done 任务返回结果不重复执行：条件 UPDATE status）。
- I3：GC 入队 INSERT IGNORE（PK=media_object_id）。

## 9. 测试用例

| ID | 用例 | 断言 |
|---|---|---|
| T1 | 收藏单条文本 | favorites 行+快照 |
| T2 | 重复收藏同一条 | 返回原 favorite_id（含并发） |
| T3 | 收藏 101 条聊天记录 | INVALID_ARGUMENT |
| T4 | 聊天记录 seq 不连续 | INVALID_ARGUMENT |
| T5 | 收藏含不可读消息 | INVALID_ARGUMENT |
| T6 | 原消息撤回/过期后读收藏 | 快照完整 |
| T7 | 删除收藏 | 软删+副本入 GC；原消息不动 |
| T8 | 标签 101 个 | QUOTA_EXCEEDED |
| T9 | 列表过滤 kind/tag + 分页 | keyset 稳定 |
| T10 | 清理预览四类 scope 各自正确 | R10 |
| T11 | 共享对象清理 | 只删我的引用，对象不入 GC |
| T12 | 无引用对象清理 | 入 GC，7 天后物理删（FakeClock） |
| T13 | 清理失败（storage 桩报错） | 该条 failed 其余 success，可重试 |
| T14 | 备份引用对象不在候选 | R13 |
| T15 | 清理后旧签名 URL | 鉴权拒绝 |
| T16 | 配额：收藏超 10GB | QUOTA_EXCEEDED |
| T17 | 他人收藏不可见/不可删 | NOT_FOUND |

## 10. 任务分解

1. 核对/补齐 media_gc_queue（00005 内，如缺）。
2. 迁移 00005_favorite.sql。
3. `internal/favorite/store.go` → service.go（R1-R9）→ dto.go → handler.go（D1-D7）。
4. 单测 T1-T9。
5. `internal/cleanup`（独立小包，避免 favorite 臃肿）：R10-R16 + D8-D12 + 单测 T10-T15。
6. 边界脚本 DOMAINS 加入 favorite、cleanup。

## 11. 验收

收藏永久保留且不受原消息生命周期影响；100 条上限/连续性校验；幂等；标签上限；清理三段式+逐对象状态+回收站 7 天；共享对象保护；配额 10GB 生效。
