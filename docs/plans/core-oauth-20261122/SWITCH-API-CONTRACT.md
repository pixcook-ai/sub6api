# Managed deployment 双向新会话切换 API

本合同固定于 2026-11-22。ROOT 负责接 routes；本切片负责 service 与 AccountHandler 方法。管理员鉴权沿现有 admin group。无 token、RT、PKCE、cookie 或凭据出现在 DTO。

## 入口
- `GET /api/v1/admin/managed-deployments/:id/switch-options` → `GetManagedDeploymentSwitchOptions`
- `POST /api/v1/admin/managed-deployments/:id/switch` → `SwitchManagedDeployment`

GET 可带 `requested_model` 和逗号分隔的 `required_capabilities` 查询参数。不提供完整上下文时仍返回 safe state，但 `context_required=true`，选项 `available=false`、`reason=request_context_required`。UI 必须由操作员显式输入模型并选择至少一个能力；不得推测模型或能力。查询参数与 POST 执行相同预检，GET 可用于重验操作员选择。

POST 严格对象，未知/重复键、过长 body、非法 revision 或上下文返回 400：
```json
{
  "expected_route_revision": 1,
  "selected_provider": "codex",
  "requested_model": "operator-explicit-model",
  "required_capabilities": ["responses"],
  "schedule_new_sessions": true
}
```
`expected_route_revision` 必须是 GET 读取的 revision；`selected_provider` 为 `codex` 或 `bps_excel`。`requested_model` 非空、最长 256；`required_capabilities` 为非空去重数组，能力名使用现有 scheduler 的 endpoint capability，首版接受 `responses`、`chat_completions`、`embeddings`、`alpha_search`、`live`、`bps_files`、`bps_local_responses`。未知能力拒绝。`schedule_new_sessions:true` 为明确调度意图，省略或 false 拒绝；服务绝不恢复被人工停调的账号。

## Safe state
沿现有 API `data` envelope：
```json
{
  "deployment_id": "deployment_UUID",
  "group_id": 1,
  "route_revision": 2,
  "selected_provider": "codex",
  "applies_to": "new_sessions",
  "context_required": false,
  "requested_model": "operator-explicit-model",
  "required_capabilities": ["responses"],
  "options": [
    {"provider": "bps_excel", "account_id": 11, "profile_ref": "oauth_UUID", "available": true, "reason": ""},
    {"provider": "codex", "account_id": 12, "profile_ref": "oauth_UUID", "available": true, "reason": ""}
  ]
}
```
两份 binding 由服务读取，UI 不回传/修改主体、workspace、backend 或 binding。预检验证原绑定来源、同主体/工作区、目标授权/人工调度/组/模型/能力和当前 scheduler 政策；失败不写 route。成功 revision +1；重复选择当前 provider 为幂等 no-op、revision 不变，但仍校验 CAS 和目标预检。

reason 是稳定安全码：`request_context_required`、`authorization_unavailable`、`identity_mismatch`、`workspace_mismatch`、`scheduling_paused`、`group_mismatch`、`model_not_supported`、`capability_mismatch`、`policy_restricted`。

## 错误与边界
- 400 `MANAGED_DEPLOYMENT_INVALID`：请求/上下文非法。
- 404 `MANAGED_DEPLOYMENT_NOT_FOUND`：deployment 不存在。
- 409 `MANAGED_DEPLOYMENT_CONFLICT`：route revision CAS 或不可变绑定不一致；UI 刷新 options 后由操作员重试，不能自动覆盖。
- 503 `MANAGED_DEPLOYMENT_UNAVAILABLE`：预检不通过或能力未接线；UI 读取 options 的 reason 展示，不自动改 provider。

旧 session / 在途请求 / response / file 等资源保持原 session immutable binding。两份授权均保留。仅本地合成验收不证明真实 OAuth、RT 旋转或 BPS 能力可用。
