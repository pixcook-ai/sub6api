# toSub2 管理的 OpenAI OAuth 控制接口

本阶段实现 Sub6 的受管 Codex OAuth 围栏、AT 安装和失效通知；toSub2 的 Codex 凭证源与完整恢复编排尚未接入，不能据此宣称端到端恢复可用。本地代码未部署，未操作真实账号。

## 所有权与限制

- 合同名：`tosub2-managed-oauth/v1`；本实现只接收 `provider=codex`。BPS 运行时仍由独立 bps-gateway 承担。
- 控制器是唯一 RT 写入/轮换者。Sub6 永不持有该账号的 RT 或 ID token，所有本地刷新入口拒绝受管账号，包括元数据损坏的受管标记。
- 只允许新建账号注册受管身份，拒绝对已有普通 OAuth 账号注入标记或热迁移，避免旧进程中的 RT 刷新仍在途。
- v1 仅 OpenAIGateway 内置 HTTP 上游；受管账号不经独立插件凭证解析，避免插件替换请求的授权归属。WebSocket 选项即使被普通配置接口打开，受管账号仍强制 off，WS ingress 直接拒绝，也不支持 Spark shadow。
- v1 不支持 Codex ticket probe/harvest 或整个 Live（包括 HTTP 创建与 WS sideband）。能力判断和直接执行入口均按受管标记是否存在拒绝；标记为 null 或损坏值也在联网前拒绝。普通账号保留原有 ticket/Live 行为。
- 管理员可预注册受管账号并签发/撤销账号级 controller key；toSub2 运行时使用独立的 `/api/v1/oauth-controller/` AT-only 路由，不需要保留管理员凭证。静态环境 allowlist 仅为备用。
- 401 优先恢复原 provider，不自动切 provider、换账号或重放本次推理。恢复不改 `status`、`error_message`、`schedulable`、代理、分组或模型配置。

## 创建并预注册身份

`POST /api/v1/admin/accounts`，示例：

```json
{
  "name": "managed-codex",
  "platform": "openai",
  "type": "oauth",
  "credentials": {},
  "schedulable": false,
  "managed_oauth": {
    "contract": "tosub2-managed-oauth/v1",
    "backend_id": "sub6-local",
    "binding_ref": "binding-1",
    "profile_ref": "profile-1",
    "provider": "codex",
    "subject": "top-level-token-sub",
    "workspace_id": "chatgpt-account-id",
    "identity_epoch": "epoch-1"
  }
}
```

`GET /api/v1/admin/accounts/create-capabilities` 返回 `schedulable_on_create=true` 和 `managed_oauth_contract`。创建时即在 `extra.managed_oauth` 持久化不可变身份：此时 `token_revision=0`、`initialized=false`、`ready=false`，且初始数据库写入就是暂停调度。此语义区别于尚未注册身份的 worker 空初始化。凭证仅可为空或包含 `model_mapping` / `compact_model_mapping`，不得包含 AT、RT、ID token。

`subject` 是 AT 顶层 `sub`；`workspace_id` 是 `https://api.openai.com/auth.chatgpt_account_id`。普通同身份 token 轮换保持 `identity_epoch` 不变，切换 provider/身份要创建另一绑定。

## 专用 controller 凭证

首选账号级持久 key。管理员创建/预注册账号后，以现有管理员认证调用 `POST /api/v1/admin/accounts/:id/oauth-controller-credentials`，严格请求体为 `{"operation_id":"规范小写非零UUID","key":"调用方生成并已安全保存的32字符以上随机密钥"}`。key 仅接受 32–4096 字节可见 ASCII，不接受空格。toSub2 必须先把候选 key 安全保存，再发送注册请求；响应丢失时用相同 operation/key 重试。同账号、同 operation、同 key hash、同完整身份且未撤销时幂等，不同负载返回 409。响应仅包含完整身份、`account_id`、`operation_id` 和 `active`，不返回 key 或 hash。

需要 migration `252_managed_oauth_controller_keys.sql`。Sub6 只持久化 SHA-256 hash，并把 key 绑定到账号及完整受管身份，包括 binding/epoch。凭证注册与账号元数据 CAS 原子执行；不能覆盖管理员状态、调度或 AT。管理员 `DELETE /api/v1/admin/accounts/:id/oauth-controller-credentials/:operation_id` 撤销已有凭证，重复撤销幂等；未知 operation 返回 409，不声称撤销成功。撤销记录和 hash 保留，旧 operation/key 均不能恢复。

运行时仅使用 `Authorization: Bearer <controller-key>`，可访问以下三个入口：

- `GET /api/v1/oauth-controller/accounts/:id/managed-credentials`
- `PUT /api/v1/oauth-controller/accounts/:id/managed-credentials`
- `GET /api/v1/oauth-controller/accounts/:id/managed-credentials/auth-events`

三者复用本文 AT-only 数据合同。每次执行前检查 key 仍有效，并重读当前受管身份；账号或完整身份不匹配时拒绝，已注册 revision 0 可读取和首次安装 AT。不接创建、普通凭证、调度、用户管理或推理能力，不降级到管理员认证。未知 key 统一 401；已认证但请求越权统一 403；不会向非法 key 暴露账号是否存在。当前 key 验证无缓存，撤销后的新请求不能继续使用。

备用环境配置在进程启动时读取 `SUB6_OAUTH_CONTROLLERS`，严格 JSON 数组，例如：

```json
[{"key_env":"TOSUB2_CONTROLLER_KEY","profile_refs":["profile-1"],"backend_ids":["sub6-local"],"account_ids":[17]}]
```

`key_env`、`profile_refs`、`backend_ids` 必填，两个 scope 列表非空且精确匹配；`account_ids` 可省略以支持预注册新账号，若提供必须是非空正整数列表，并与 profile/backend 同时匹配。禁止未知/重复字段、重复列表项、重复 env 引用或重复 key；secret 仅从指定 env 读取，进程只保留其 hash。未配置环境列表时，账号级持久 key 正常工作；非空但错误/空数组的配置整体 fail-closed。持久 key 首先验证，只有数据库中完全未知的 key 才可匹配显式静态 allowlist；已撤销或旧身份 key 不得用同一 secret 回退到环境配置。

## 安装 AT

`PUT /api/v1/admin/accounts/:id/managed-credentials`：

```json
{
  "contract": "tosub2-managed-oauth/v1",
  "backend_id": "sub6-local",
  "binding_ref": "binding-1",
  "profile_ref": "profile-1",
  "provider": "codex",
  "subject": "top-level-token-sub",
  "workspace_id": "chatgpt-account-id",
  "identity_epoch": "epoch-1",
  "expected_token_revision": 0,
  "token_revision": 3,
  "operation_id": "36dc40ec-a9f6-4e46-a4e0-ab899ce1b85e",
  "access_token": "controller-supplied-access-token",
  "expires_at": "2026-11-22T12:00:00Z"
}
```

身份逐项匹配；新 revision 必须大于 expected revision，允许跳号，上限为 JavaScript 安全整数。`operation_id` 是规范小写非零 UUID；最后一次 operation 的同一负载重试幂等，不同负载冲突。数据库精确比较旧身份/版本元数据，凭证与 scheduler outbox 同一 SQL 提交；更新失效则 409。安装成功仅清除受管 `auth_blocked`，不清除管理员禁用、错误或暂停。

请求只接受声明字段、拒绝重复字段和尾随 JSON、最大 128 KiB。AT claim 检查要求顶层 `sub`、workspace、官方 Codex `client_id` 或 `azp` 一致；显式到期时间不可晚于 AT `exp`。这是 claim 一致性校验，未验证 JWT 签名，也不证明上游真实接受授权。

`GET /api/v1/admin/accounts/:id/managed-credentials` 与安装成功的响应 `data`：

```json
{
  "contract": "tosub2-managed-oauth/v1",
  "backend_id": "sub6-local",
  "binding_ref": "binding-1",
  "profile_ref": "profile-1",
  "provider": "codex",
  "subject": "top-level-token-sub",
  "workspace_id": "chatgpt-account-id",
  "identity_epoch": "epoch-1",
  "account_id": 17,
  "token_revision": 3,
  "expires_at": "2026-11-22T12:00:00Z",
  "last_operation_id": "36dc40ec-a9f6-4e46-a4e0-ab899ce1b85e",
  "auth_blocked": false,
  "event_cursor": 0,
  "initialized": true,
  "ready": true
}
```

该控制接口不返回 AT、RT、ID token 或负载 hash。`initialized` 仅表示至少安装过一次 AT；`ready` 表示受管授权未阻断且尚未过期，不表示管理员已开启调度。

## 401 通知与恢复

`GET /api/v1/admin/accounts/:id/managed-credentials/auth-events?after=0` 的 `data`：

```json
{
  "events": [{
    "contract": "tosub2-managed-oauth/v1",
    "backend_id": "sub6-local",
    "binding_ref": "binding-1",
    "profile_ref": "profile-1",
    "provider": "codex",
    "subject": "top-level-token-sub",
    "workspace_id": "chatgpt-account-id",
    "identity_epoch": "epoch-1",
    "cursor": 1,
    "event_id": 1,
    "failed_token_revision": 3,
    "origin": "upstream_oauth",
    "code": "upstream_oauth_unauthorized",
    "observed_at": "2026-11-21T12:00:00Z"
  }],
  "next_cursor": 1,
  "cursor_gap": false
}
```

每账号单调整数 `event_id=cursor`，每 token revision 最多记录一次有效 401；保留最近 128 条，消费位置落出保留窗口时 `cursor_gap=true`，消费者必须先重读当前状态再恢复。`after` 大于当前 cursor 返回 409。

真实 HTTP401、HTTP200 的 SSE 授权失败及管理探测，均在取得 AT 的同一次数据库读取中冻结身份、revision 与 AT，通过私有 context 贯穿发送和响应解析。传输层仅验证该快照及实际 bearer，绝不重新读取数据库、按同一 AT 推断一个更新的 revision。缺少快照的旧 string-only 取 token 调用不得发送受管请求。取得 r1/T 后、进入传输层前即使重新安装 r2/T，这个请求仍只属于 r1；晚到 401 不影响 r2。事件仅阻断受管授权就绪状态。控制器恢复原授权，随后用更高 revision 安装新的 AT；失败保持阻断，等待人工处理，不切换到另一 provider。

## 本地验证边界

聚焦 Go 单测覆盖创建暂停、AT 安装校验、版本跳号/幂等、旧 401、缓存绕过、刷新拒绝、HTTP/SSE 不重放、管理配置保留、事件截断、批量凭证全批次拒绝、ticket/Live 的联网前拒绝与普通账号能力负控，以及 SQL CAS/outbox 形状。SQL mock 不等同 PostgreSQL 执行；真实数据库 CAS 验证与真实 OAuth 恢复验收应分别记录，未执行项目不得视为通过。
