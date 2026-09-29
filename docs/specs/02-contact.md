# SPEC-02 contact（好友关系与联系人）

> 依据：合同 §4；交叉评审裁决 3（epoch）；ADR-004/006。状态：已评审。

## 1. 范围

**做**：五种入口好友申请、申请生命周期、好友关系 epoch、删除/拉黑/no_message/hidden、备注、标签 CRUD 与批量打标、通讯录查询、陌生人有限资料。
**不做**：共同群查询（阶段二群模块后补接口）、朋友圈权限判定执行（阶段三消费 `friend_settings.moment_perm` 与 epoch 快照）、消息发送权限执行（阶段二消费 `message_perm`；本模块只负责写入与提供判定服务）。

## 2. 领域规则

### 2.1 申请（合同 §4.1）
- R1：入口 `source ∈ {phone, account_name, qrcode, group, card}`。阶段一 `qrcode`=`POST /contacts/qrcode` 签发个人二维码令牌（签名载荷 user_id+过期 7 天），`group`/`card` 仅校验来源合法性（group 要求当前与对方同群——阶段一群模块缺失，**group 入口暂返回 `RESOURCE_UNAVAILABLE` 并在 PROGRESS 记录为阶段二补全**；card 要求名片主人是申请人的好友）。
- R2：状态机：`pending → accepted|rejected|expired|cancelled`。`expired` 由 Worker 处理 7 天未决申请（阶段一提供 store 方法与 service `ExpireStaleRequests`，Worker 接入属阶段二调度）。
- R3：同一 (applicant, target) 同时最多一条 `pending`：重复提交只更新 `verify_text` 与 `updated_at`（唯一索引 `(applicant_id,target_id,status='pending')` 部分唯一——MySQL 用生成列 `pending_guard` 实现，同 §12.4 客服会话技法）。
- R4：已是好友 → `ALREADY_FRIEND`；自己加自己 → `INVALID_ARGUMENT`；目标不存在 → `RESOURCE_UNAVAILABLE`。
- R5：被对方拉黑（target 对 applicant `message_perm=blocked`）→ `RESOURCE_UNAVAILABLE`。
- R6：被拒绝后 24 小时内再申请 → `STATE_CONFLICT`（message 含剩余秒数）。
- R7：`verify_text` ≤ 200 字符。
- R8：同意（仅 target）：事务内 ①申请置 accepted ②`friendship_epochs` 取 max(epoch)+1 ③写 `friendships` 新 epoch 行 status=active ④建双方 `friend_settings` 缺省行（不存在才建，保留历史备注？——**不保留**：新 epoch 新周期，设置行随 epoch 重置为默认，历史设置行保留归档。裁决 3"新关系新权限"）⑤调 conversation 建 direct 会话。审计 `contact.request.accepted`。
- R9：拒绝（仅 target）→ rejected，记 `rejected_at`。取消（仅 applicant，且 pending）→ cancelled。
- R10：列表：收到的/发出的申请按时间倒序游标分页，每页默认 20 最大 50。

### 2.2 关系与 epoch（裁决 3）
- R11：`friendships(user_low, user_high, friendship_epoch, status, started_at, ended_at)`；当前有效关系 = `friendship_epochs.current_epoch` 指向的 status=active 行。
- R12：删除好友（任一方）：事务内当前 epoch 行置 `status=deleted, ended_at=now`，epochs 指针保留（指向已删除 epoch，使"当前关系"判定=非 active）。历史消息/收藏不受影响（合同 §4.4）。审计 `contact.friend.deleted`。
- R13：重新添加成功 → 新 epoch（R8②），旧 epoch 永久失效；新好友**不能**查看旧 epoch 朋友圈（阶段三执行，本模块保证 epoch 单调不复用）。
- R14：`GetActiveFriendship(userA, userB) → (epoch, ok)` 是对外核心服务；`ListFriendIDs(userID, cursor)` 供阶段三快照。

### 2.3 单向设置（合同 §4.3）
- R15：`friend_settings(owner_id, friend_id, epoch, remark≤64, message_perm, moment_perm, moment_notify)`，PK(owner_id, friend_id)。**设置行不带 epoch 归属当前关系**：删除好友时设置行保留，重新加友时 R8④ 重置默认（裁决 3 语义：新周期不继承旧设置）。
- R16：`message_perm ∈ {normal, no_message, blocked}`；`moment_perm ∈ {visible, hidden}`。
- R17：拉黑 = owner 置对方 `blocked`：立即生效（对申请 R5、消息阶段二、朋友圈阶段三）。拉黑不删除好友关系行。解除拉黑回 `normal`。
- R18：`no_message` 只禁对方发送，owner 仍可发（执行在阶段二；服务方法 `CanSendMessage(from, to) (bool, reason)` 阶段一提供：规则=to 对 from 为 blocked→false；to 对 from 为 no_message→false；from 对 to 为 blocked→false；非好友→false）。
- R19：备注/标签只对 owner 生效，不可被对方读取。

### 2.4 标签（合同 §4.5）
- R20：`contact_tags(id, owner_id, name≤32, UNIQ(owner_id,name))`；重命名冲突 → `STATE_CONFLICT`。
- R21：`contact_tag_members(tag_id, friend_id, UNIQ)`；批量添加/移除幂等（重复添加不产生重复行，移除不存在不报错）。
- R22：删除标签：级联删成员行；不影响好友关系与历史朋友圈快照（合同 §4.5）。
- R23：打标对象必须是当前好友（active epoch），否则 `RESOURCE_UNAVAILABLE`。

### 2.5 查询（合同 §4.5）
- R24：陌生人查询 `GET /contacts/lookup?phone=|account_name=`：只返回 user_id/nickname/avatar/account_name/is_friend；不存在 → `RESOURCE_UNAVAILABLE`。限流（每分钟 20 次/用户，cache 计数）。
- R25：好友列表 `GET /contacts/friends`：游标分页（按 user_id 升序），返回 user_id/nickname/remark/avatar/account_name/tags。
- R26：好友搜索 `GET /contacts/friends/search?q=`：匹配 nickname/remark（`LIKE '%..%'` 参数化转义，限 100 好友内全扫可接受——MVP 口径，好友数大时阶段四再优化；记录于 PROGRESS 已知事项）。
- R27：通讯录不泄露非好友敏感资料（R24/R28 同 SPEC-01 R28）。

## 3. 数据模型

- `friend_requests(id, applicant_id, target_id, source, verify_text, status, pending_guard TINYINT GENERATED, rejected_at, created_at, updated_at, UNIQ(applicant_id,target_id,pending_guard))`
- `friendships(user_low, user_high, friendship_epoch, status, started_at, ended_at, PK(user_low,user_high,friendship_epoch))`
- `friendship_epochs(user_low, user_high, current_epoch, PK(user_low,user_high))`
- `friend_settings(owner_id, friend_id, remark, message_perm, moment_perm, moment_notify, created_at, updated_at, PK(owner_id,friend_id))`
- `contact_tags(id, owner_id, name, created_at, UNIQ(owner_id,name))`
- `contact_tag_members(tag_id, friend_id, PK(tag_id,friend_id))`

## 4. API 契约

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | /api/v1/contacts/requests | 发起申请 `{target_id|phone|account_name|qrcode_token|card_owner_id, source, verify_text}` |
| GET | /api/v1/contacts/requests?box=received\|sent | R10 |
| POST | /api/v1/contacts/requests/{id}/accept | R8 |
| POST | /api/v1/contacts/requests/{id}/reject | R9 |
| POST | /api/v1/contacts/requests/{id}/cancel | R9 |
| DELETE | /api/v1/contacts/friends/{friendID} | R12 |
| PATCH | /api/v1/contacts/friends/{friendID}/settings | `{remark, message_perm, moment_perm, moment_notify}` R15–R19 |
| GET | /api/v1/contacts/friends | R25 |
| GET | /api/v1/contacts/friends/search | R26 |
| GET | /api/v1/contacts/lookup | R24 |
| GET | /api/v1/contacts/qrcode | 签发本人二维码令牌 |
| GET/POST | /api/v1/contacts/tags；PATCH/DELETE /api/v1/contacts/tags/{id} | R20–R22 |
| POST/DELETE | /api/v1/contacts/tags/{id}/members | R21 `{friend_ids:[...]}` |

## 5. 幂等与并发

- 申请重复提交：唯一索引 + ON DUPLICATE 更新 verify_text（R3）。
- 同意并发双击：申请行 `SELECT ... FOR UPDATE`，非 pending → 返回当前状态（幂等成功语义）。
- 删除好友与同意申请并发：锁顺序 ADR-006（epochs 行先于 friendships 写）。
- accept/reject/cancel 三操作互斥由状态机保证。

## 6. 验收标准（映射合同 §18.1）

- A1 五种入口校验路径正确（group 入口按 R1 的阶段性返回，PROGRESS 有记录）。
- A2 重复申请合并为一条，verify_text 更新。
- A3 同意后双方互见好友；direct 会话存在且唯一。
- A4 删除后 `GetActiveFriendship`=false；重新加友 epoch+1 且旧 epoch 不复用。
- A5 拉黑后对方申请/（未来的）消息被拒，共享群不受影响的判定服务返回正确。
- A6 标签同名冲突、批量打标幂等、删除标签不级联好友关系。
- A7 陌生人 lookup 不返回 phone/region/signature。
