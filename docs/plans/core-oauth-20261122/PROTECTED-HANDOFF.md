# OAuth 受保护所有权交接

本文件是 Sub6 / toSub2 交接协议与恢复边界的单一参考。对应 [显式交接规格](tickets/05-explicit-handoff.md)（Issue #7）。证据边界见 [验证记录](HANDOFF-RECOVERY-VERIFICATION.md)，真实凭据迁移尚未验收。

## 支持范围

正常使用遵循谁创建谁刷新，Sub6 可独立上号和运行。交接只把旧 toSub2 档案的 OAuth 授权所有权交给 Sub6，不自动改变部署、分组或调度选择；接收后仍须走既有 core runtime 安装和选择流程。

当前要求来源具有 bound 账号，且全部绑定都属于同一个目标 Sub6。无绑定、绑定未确认、未决发布和多目标交接必须拒绝；完整 transfer 路径在任何远端永久撤销前检查这些条件。prepare / reconcile 不代表所有权转移。

来源 toSub2 在私有根目录持有进程锁，冻结 registry 中该档案的新发布 reservation，并等待在途授权、刷新和发布结束。证据只涵盖该实例及其冻结的绑定/发布清单。接收端逐项用同一 Sub6 已保存的账号身份和后端配置重新读取围栏，不能证明未知外部系统或其他 vault 副本的全局清单完整性。

## Sub6 管理 API

前缀为 `/api/v1/admin/core-oauth`，沿用管理员鉴权、RBAC 和 response.Success 响应封装。Actor 来自鉴权上下文，不接受客户端 JSON。

| 方法和相对路径 | 请求与结果 |
| --- | --- |
| POST /profiles/:ref/handoff-invitations | 请求字段为 operation_id, profile_ref, source_profile_ref, provider, subject, workspace_id, identity_epoch, token_revision, expires_at；目标 profile_ref 必须等于 URL ref。返回同字段及 contract, public_key。 |
| POST /profiles/:ref/handoffs/accept | 仅接受 operation_id, wrapped_key, nonce, ciphertext；成功返回安全接收回执。 |
| GET /profiles/:ref/handoffs/:operation_id | 读回原管理员和操作号绑定的安全回执，是丢响应后的判定入口。 |
| POST /profiles/:ref/handoffs/:operation_id/abandon | 请求为 `{}`；仅可放弃 pending，删除暂存载荷及接收私钥，保留 abandoned 回执。accepted 不可放弃。 |

来源引用为 `oauth_<uuid>`，目标档案和操作号为 UUID。provider 为 codex 或 bps_excel。新 invitation 只能预留尚未授权、没有 grant、revision、identity epoch 或其他在途操作的同 provider 空档案。有效期必须在当前时间之后且不超过 15 分钟；toSub2 首次 transfer 预约约 10 分钟。相同管理员、操作及身份的 invitation 重试返回原公钥和原有效期，不续期。

安全回执仅含 operation_id, provider, profile_ref, source_profile_ref, subject, workspace_id, identity_epoch, token_revision, status, expires_at, transfer_supported。status 为 pending / accepted / abandoned，只有 accepted 为 transfer_supported:true。普通 profile、回执和控制台响应不含 AT、RT、ID token、密钥或传递载荷。

## 加密合同

- contract 固定为 sub6-core-oauth-handoff/v1。
- invitation 公钥为 RSA 2048、SPKI DER；接收私钥仅保存在 Sub6 加密 profile 记录中。
- 每次密封产生 32 字节 AES key 和 12 字节 nonce，以 AES-256-GCM 加密；ciphertext 末尾附 16 字节 GCM tag。
- AAD 为合同名、目标 profile_ref、operation_id 依次组成的三行 UTF-8 文本，各行之间一个 LF 换行字节，末尾无换行。
- RSA-OAEP-SHA256 包装 AES key，OAEP label 为空；wrapped_key 解码后须为 256 字节。
- 公钥、wrapped_key、nonce、ciphertext 均使用无填充的规范 raw base64url。密文含 tag 上限 160 KiB，HTTP JSON 请求体上限 256 KiB；拒绝额外字段、重复 JSON key 和 null 必填项。

明文仅在受保护传递内部存在，字段为 contract, operation_id, profile_ref, source_profile_ref, provider, subject, workspace_id, identity_epoch, token_revision, source_quiesced, publication_fences, grant。source_quiesced 必须为 true，身份与版本须和 invitation 一致；token_revision 为 1 至 9007199254740991 的整数。grant 字段为 access_token, refresh_token, id_token, client_id, subject, workspace_id, email, expires_at，不得经普通 console response 导出。

接收端不进行 OAuth token 请求。解密、grant 验证及实时围栏核对后，通过一次 profile CAS 同时持久化 grant 与 accepted 回执，并清除接收私钥。相同 envelope 的 accepted 重试读回原回执，不重复激活；不同密文或身份的重试冲突。

## 永久发布围栏

### Codex

管理员 POST /api/v1/admin/accounts/:id/oauth-publication-fence 请求含 operation_id, token_revision, identity；同路径 GET 读回回执，未建立围栏时返回 404。identity 为 tosub2-managed-oauth/v1 身份，含 contract, backend_id, binding_ref, profile_ref, provider, subject, workspace_id, identity_epoch。

账号行锁和独立事务确保永久 publisher fence、该账号全部 controller key 撤销、schedulable=false、scheduler outbox 写入一起提交，回执只能在提交后返回。围栏阻止旧 publisher 写入和新 controller 注册，不以枚举已知 key 代替永久围栏。回执含 operation_id, identity, token_revision, account_id, fenced:true。

### Excel / BPS

旧 worker 控制通道 /_control/v1/publication-fence 建立永久发布围栏并保留持久回执。接收 Sub6 使用账号已保存的 Files 后端配置和独立 Files service key，请求 GET /internal/bps-files/v1/publication-fence 获取只读 proof；Files key 不授予 worker 控制写权限。

proof 合同为 bps-publication-fence-proof/v1，字段为 contract, identity, fenced, receipt。identity 是 tosub2-managed-oauth/v1 身份加 token_revision；receipt 为相同身份字段加 operation_id。接收端逐项核对 backend、binding、profile、subject、workspace、epoch、revision、operation 与已存账号/config 和实时 proof 一致。

明文中的 publication_fences 为 1–128 项 `{kind, account_id, receipt}`，kind 为 codex_controller 或 bps_worker。不接受重复 account/binding，不使用发送方任意 URL 代替已保存的可信配置。

## 操作顺序

1. 确认单目标绑定及所有未决发布结果。Excel 路径先手动停止旧 BPS 账号调度，并保持停止直到替代 runtime 安装及验收；worker fence 不等于撤销旧账号已有 AT 的推理能力。
2. 在目标 Sub6 创建同 provider 的空 core profile，记下目标 profile_ref；在 toSub2 prepare 并保存 operationId。后续始终使用此操作号。
3. 调用 POST /api/oauth/profiles/:source/ownership-handoff/transfer，请求形状如下。目标要求 HTTPS；仅本机 127.0.0.1 / [::1] 可使用 HTTP。目标 admin key 仅用于当次调用，不进入 journal。

   ```json
   {
     "operation_id": "原操作号",
     "destination_profile_ref": "目标 Sub6 profile UUID",
     "target": {"baseUrl": "https://sub6.example", "adminApiKey": "由安全管理通道提供"}
   }
   ```

4. 客户端持久记录目标摘要、身份和 invitation；本地停写、永久围栏和读回通过后，密封 grant、持久保存 envelope，再提交接收。控制台不返回 envelope。
5. 客户端必须再次 GET 读取匹配的 accepted 回执，才能持久标记 accepted、删除 journal 中的 envelope、清理来源 vault grant；清理后旧 owner 仍永久闭锁。
6. 完成条件为 accepted 回执已持久保存、来源 cleanup_pending:false。随后另走既有 core runtime 安装和选择流程，单独验收调度和真实请求。transfer 不自动执行 runtime 操作。

GET ownership-handoff 展示本地状态和 transfer 摘要。prepareLocal / reconcile 响应自身始终 transfer_supported:false；GET/prepare 控制台包装仅在已有 accepted transfer 记录时展示 true。POST ownership-handoff/reconcile 接受 `{operation_id,target:{baseUrl,adminApiKey}}`，只确认远端围栏。安全取消期间的 transfer 状态为 abort_pending，完成后为 aborted，二者 transfer_supported 均为 false。

## 中断与恢复

| 场景 | 操作与完成条件 |
| --- | --- |
| 仅本地停写，尚无 transfer 记录及远端撤销记录 | POST ownership-handoff/abort，body 仍仅含原 operation_id。先持久取消，再持久解除同操作号本地围栏；原操作返回 aborted 才算完成。 |
| 已有 transfer 记录，但尚未开始远端撤销 | 同一 abort 请求另须 target:{baseUrl,adminApiKey}。持久进入 abort_pending，在协调器同一操作锁内核实目标未激活并完成 abandon；持久保存匹配的 receiver abandoned 回执后，才恢复源并记录 aborted。 |
| 已开始远端撤销（remote_started 边界）或永久撤销结果未知 | 不允许 abort 恢复源。保持 fence，使用同一 source / destination / target / operation 重试 transfer 或 reconcile；必要时在 Sub6 重新授权。不得恢复旧 owner 或复用旧 RT。 |
| accept 响应丢失、进程重启 | 重试相同 transfer。先 GET 原接收操作；pending 时可重发已保存 envelope，accepted 时只补记回执和来源清理。 |
| 来源写盘或目录 sync 结果未知 | 保留文件现场，重启后读取原日志；不得清日志或换操作号绕过。失败保持闭锁。 |
| invitation 过期、pending 或身份冲突 | accept 对过期返回 CORE_OAUTH_HANDOFF_EXPIRED（409），不自动解除 fence；原管理员可通过 Sub6 abandon API 放弃 pending。已远端撤销后，在 Sub6 重新授权。 |
| accepted 后来源 vault 清理失败 | 保持围栏，重试相同 transfer，直到匹配的 accepted 回执仍存在且 cleanup_pending:false。 |
| 已 accepted | 不可 abandon 或回滚至旧 owner；后续刷新和 401 恢复由 Sub6 负责。 |

取消目标预约时始终先 GET 原 operation ID，严格匹配 source 身份、操作及目标。已存在 pending（包括过期）时直接 abandon，再 GET abandoned；已存在 abandoned 时直接核对原回执，不重复 POST invitation，也不续期。无本地 invitation 且 GET 返回 404/409 时，才 POST 同操作号取消预约；404/409 本身及空 profile 都不是未激活证明，POST 成败均须重新 GET 原操作后核对，再 abandon / GET abandoned。仅创建此前不存在的取消预约可使用新的短 TTL，不能重写或延长已存在 invitation。任何读回、写盘或来源恢复失败均保留可重试状态和源围栏；同操作号重试，不能换号绕过。

取消流程以 coordinator 的同一档案操作锁排斥 reconcile：持久 abort_pending → 接收端未激活证明和 abandoned 回执持久化 → 源 abort → 持久 aborted。存在 transfer 时，返回的 handoff 包含 profileRef、operationId、destinationProfileRef、status:aborted、transfer_supported:false、cleanup_pending:false 和安全 abandoned receipt；纯本地取消仍返回 status、operationId、transfer_supported。已远端开始撤销即使当前没有成功回执，也不能进入源恢复路径。安全取消完成后可用新的 operation ID 开始新的交接，旧接收操作保持 abandoned，旧操作不能再激活。

pending（包括过期）闭锁目标授权、刷新、401 恢复和凭据快照，直到 accepted 或显式 abandon。回执原 revision 保持不变，后续正常刷新可提升 live grant revision。未知结果不启用共享 RT fallback。
