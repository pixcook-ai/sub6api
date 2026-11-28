# Claude CLI 协议兼容验收

## 本次兼容变更

- `/v1/messages/count_tokens` 的普通转发和 Anthropic API Key 透传保留上游的 429、529，分别返回 `rate_limit_error`、`overloaded_error`，并在过滤规则允许时保留这两个状态的 `Retry-After`。进入统一错误处理的响应也会保留允许的 `request-id` 和 `x-should-retry`，并把允许的 ID 写入 `request_id`。上游明确不支持计数端点时，仍走独立的 404 降级路径。`x-should-retry` 加入共享响应头允许名单，对使用 `FilterHeaders` 的其他入口同样生效，`ForceRemove` 仍可移除该头。官方 SDK 的 `x-should-retry` 指示优先于状态码规则。 [转发实现](../backend/internal/service/gateway_count_tokens.go) · [共享响应头过滤](../backend/internal/util/responseheaders/responseheaders.go) · [Anthropic API 错误](https://platform.claude.com/docs/en/api/errors#request-id) · [官方 SDK 重试代码](https://github.com/anthropics/anthropic-sdk-typescript/blob/554d74b26bebc9d065372aa5919d2a7fa6df9b3c/src/client.ts#L1995-L2014)
- `/v1/messages` 的普通转发和 Anthropic API Key 透传在返回最终上游错误时，按过滤规则保留 `Retry-After`、`request-id`、`x-should-retry`，并将允许的 ID 写入 `request_id`。默认映射仅对 Anthropic 原生 `overloaded_error` 保留 529，显式错误透传规则优先。切换账号后，由最终响应提供请求 ID 和重试头。 [服务回归](../backend/internal/service/gateway_messages_error_contract_test.go) · [网关处理器回归](../backend/internal/handler/gateway_handler_anthropic_messages_error_unit_test.go)
- `POST /api/event_logging/batch` 与 `POST /api/event_logging/v2/batch` 共用本地确认处理器。两个入口忽略事件内容，返回 HTTP 200 和空响应体。 [路由实现](../backend/internal/server/routes/common.go) · [路由测试](../backend/internal/server/routes/common_test.go)
- SDK 兼容配置可显式选择 `entrypoint: sdk-cli`，生成与已观测 Linux 无交互模式一致的 UA 后缀和压缩声明，并在普通 OAuth 模拟分支中省略 SDK stream helper 头。省略该字段时保留旧行为；不接受空值、`null` 或未知入口。可选的逐模型 Messages/计数 beta 配置相互独立，保留管理员拦截与过滤规则。[配置说明](claude-compatibility-profile.md) · [Linux 参考配置](examples/claude-linux-2.1.330-profile.json)
- TLS 客户端缓存按模板内容区分，修改同名模板后建立新传输，内容相同则继续复用；新传输保留独立配置快照，每次握手独立复制版本列表，避免 uTLS 改写其他连接的配置。HTTP `deflate` 优先支持标准 zlib 封装，并兼容常见 raw DEFLATE；不把 zlib 校验失败改作 raw 重试。[传输实现](../backend/internal/repository/http_upstream.go) · [握手隔离回归](../backend/internal/pkg/tlsfingerprint/dialer_profile_test.go)

## 验证结果

1. 官方 Claude CLI `2.1.330` 的 macOS arm64 二进制已通过发布清单的 SHA-256 校验。本机用于验收的 `claude.orig` 与官方发布件逐字节一致，摘要为 `f7c5093b9dd6560d08af28bc27b123fb55091deae3180ac39df199522da86815`。 [官方发布清单](https://downloads.claude.ai/claude-code-releases/2.1.330/manifest.json)
2. 真实 CLI 在本机通过 `GatewayService` 的 API Key 透传路径完成普通文本流和 `Read` 工具往返。测试使用合成上游，确认 CLI 读出测试文件，并在下一轮转发正确的 `tool_result`。 [真实 CLI 协议测试](../backend/internal/service/gateway_claude_cli_contract_test.go)
3. 两条本地遥测路由的回归测试通过。对官方 `2.1.330` 二进制内的响应分支完成九项离线回放，`200` 或 `204` 空响应按整批接受，合法 `accepted_count` 用于确认部分接受，非 2xx 进入失败分支。回放用内存响应替代网络传输。 [Anthropic 遥测钩子的响应检查](https://github.com/anthropics/claude-code/blob/2287e8ceed6e46623a5b913308515db683e25fbe/mods/telemetry/hooks/telemetry-of/telemetry-of.ts#L120-L155)
4. 服务器临时目录中的官方 Linux amd64 CLI 通过发布清单校验，SHA-256 为 `f390eaa0814e6a1bff74cbb47acb74cb9c89d30ced809fe0af5af00c7bfe8ebb`，`--version` 返回 `2.1.330 (Claude Code)`。 [官方发布清单](https://downloads.claude.ai/claude-code-releases/2.1.330/manifest.json)
5. Linux 首轮静态测试程序通过上传摘要校验，SHA-256 为 `04b76c8f944ce3ff8ad61301bd7e7e927b4df189c25ef2508bfef681ca5b90a0`。官方 Linux CLI 在服务器隔离测试中通过文本 SSE 和 `Read` 往返，总耗时 `0.32s`。脱敏测试也通过。 [真实 CLI 协议测试](../backend/internal/service/gateway_claude_cli_contract_test.go)
6. 修复前，`x-should-retry` 缺失导致 7 条断言失败，包括一条默认响应头允许名单断言和两种转发模式各 3 条服务断言。修复后，响应头过滤测试和两种转发模式的 14 个计数错误子用例通过，覆盖 `true`、`false`、无该头和配置显式移除该头。 [响应头过滤测试](../backend/internal/util/responseheaders/responseheaders_test.go) · [计数错误测试](../backend/internal/service/gateway_count_tokens_error_contract_test.go)
7. 2026-11-27 02:02:21 UTC，上一轮 Linux 静态测试程序通过上传摘要校验，SHA-256 为 `c0c3e71c7fd6040b3ed252004fe5c90c7e48c16c3ee5a514608099f0f8e3dafd`。该轮运行全部通过且无跳过，覆盖消息请求 ID 的 2 个子用例、真实 CLI 的文本 SSE 和 `Read` 往返（`0.31s`）、参考样本导出的 3 项测试、计数错误的 14 个子用例和计数成功响应的 4 个子用例。遥测路由结果来自本机路由包。 [真实 CLI 协议测试](../backend/internal/service/gateway_claude_cli_contract_test.go) · [计数错误测试](../backend/internal/service/gateway_count_tokens_error_contract_test.go) · [计数成功测试](../backend/internal/service/gateway_count_tokens_response_headers_test.go)
8. 上一轮本机三个包的全量回归均通过，进程退出码为 `0`。`internal/util/responseheaders` 耗时 `0.651s`，`internal/server/routes` 耗时 `11.309s`，`internal/service` 耗时 `164.780s`。

上一轮 Linux 日志和脱敏样本保存在工作区本地的 `.scratch/claude-cli-protocol/linux-2.1.330/`。 [运行日志](../.scratch/claude-cli-protocol/linux-2.1.330/final-results.log) · [文本请求样本](../.scratch/claude-cli-protocol/linux-2.1.330/text.json) · [`Read` 工具往返样本](../.scratch/claude-cli-protocol/linux-2.1.330/read_tool_roundtrip.json)

### 本轮 Messages 和 OAuth 回归

1. 修复前，服务与真实网关处理器的回归用例复现了最终错误响应的差异。修复后，通过了最终账号请求 ID、重试头、429/529 默认映射、`ForceRemove` 与显式规则优先级检查。 [服务回归](../backend/internal/service/gateway_messages_error_contract_test.go) · [网关处理器回归](../backend/internal/handler/gateway_handler_anthropic_messages_error_unit_test.go)
2. macOS 官方 CLI 收到真实 Messages 处理器返回的 429 或 529。两条响应均带 `x-should-retry: false` 和 `Retry-After`，CLI 各只发送 1 次请求，进程退出码均为 `1`，`api_error_status` 分别为 429 和 529。该用例核对请求次数和返回状态。 [CLI HTTP 错误回归](../backend/internal/handler/gateway_handler_claude_cli_error_contract_unit_test.go)
3. 单独运行的 429 正向重试用例也通过。首次响应带 `Retry-After: 1` 和 `x-should-retry: true`，从该响应写完到 CLI 的第二次请求相隔 `1.230160417s`，满足测试的 `900ms` 下限。网关和合成上游各收到两次请求。CLI 最终消费成功响应并退出 `0`，整次 CLI 调用耗时 `1.458s`。 [CLI HTTP 重试回归](../backend/internal/handler/gateway_handler_claude_cli_error_contract_unit_test.go)
4. macOS 官方 CLI 消费网关转发的 SSE `overloaded_error` 后重试一次，并成功消费第二条文本流。 [CLI SSE 回归](../backend/internal/service/gateway_claude_cli_contract_test.go)
5. macOS 官方 CLI 在非 `--bare` 模式用合成入站 Bearer 凭据访问网关，经实际 `GatewayService.Forward` 向合成 OAuth 账号上游转发并消费文本流。测试确认平台 SDK 请求头和 `metadata.user_id` 保留，出站只使用选中账号的合成 OAuth 凭据。 [CLI OAuth 回归](../backend/internal/service/gateway_claude_cli_contract_test.go)
6. 本机四包的 `-tags unit` 全量回归均通过，进程退出码为 `0`。`internal/util/responseheaders` 耗时 `0.580s`，`internal/server/routes` 耗时 `11.455s`，`internal/service` 耗时 `222.388s`，`internal/handler` 耗时 `42.465s`。 [本地回归日志](../.scratch/claude-cli-protocol/full-regression-20261127.log)

### Linux 第二轮回归与补充用例

2026-11-27 02:35:14 UTC，Linux 的 service 和 handler 测试程序经 SHA-256 校验后运行，所选用例全部通过且无跳过。service 摘要为 `6215f99e2373ce4dd5aa28c23f7bb34ed38779da5079cf0f4f8115f2256cec0b`，handler 摘要为 `60338419072a344067b536f32968879afc1fe6e67c22f632f3e7158de4973b07`。 [service 日志](../.scratch/claude-cli-protocol/linux-2.1.330-stage2/service-stage2.log) · [handler 日志](../.scratch/claude-cli-protocol/linux-2.1.330-stage2/handler-stage2.log)

- 官方 Linux CLI 通过 API Key 透传完成文本 SSE（`0.154s`）、`Read` 工具往返（`0.162s`）和 SSE 错误后恢复（`0.692s`，共两次请求）。非 `--bare` 合成 OAuth 凭据用例也通过实际 `GatewayService.Forward`（`0.201s`），验证平台 SDK 请求头、`metadata.user_id` 和入站、出站凭据隔离。 [service 日志](../.scratch/claude-cli-protocol/linux-2.1.330-stage2/service-stage2.log) · [OAuth 脱敏样本](../.scratch/claude-cli-protocol/linux-2.1.330-stage2/references-stage2/TestClaudeCLIConsumesGatewayOAuthStream.json)
- 429 和 529 的 `x-should-retry: false` 用例各发出一次请求，CLI 退出码均为 `1`，`api_error_status` 分别为 429 和 529。429 正向用例在首次响应完成后 `1.196118855s` 发出第二次请求，CLI 成功退出，耗时 `1.345s`。 [handler 日志](../.scratch/claude-cli-protocol/linux-2.1.330-stage2/handler-stage2.log)
- Mac 和 Linux 官方 CLI 的补充 `/context` 用例通过。普通转发和 API Key 透传均通过实际 `ForwardCountTokens` 发送三次 `/v1/messages/count_tokens` 请求，`/v1/messages` 请求数为零。合成上游返回固定计数，CLI 显示系统提示词 17、消息 17、合计 34 个 token。Linux 普通转发耗时 `0.140s`，透传耗时 `0.135s`，用例总耗时 `0.28s`。 [CLI 计数回归](../backend/internal/service/gateway_claude_cli_count_tokens_contract_test.go) · [Linux 计数日志](../.scratch/claude-cli-protocol/linux-2.1.330-stage2/count-tokens-stage3.log)

2026-11-27 02:45:40 UTC，Linux 计数和 Go 出站 TLS 的补充测试均通过。计数测试程序的 SHA-256 为 `836ec1f1081ded5e2f78f80076b77ec39b12568e3bdc7fe1c06e335bdfbadfe1`，TLS 测试程序为 `31a7b9563aa3a736fa7f54261e265988a347b1272c95aab1ce1c987eb49cb48b`。 [计数日志](../.scratch/claude-cli-protocol/linux-2.1.330-stage2/count-tokens-stage3.log) · [Go 出站 TLS 日志](../.scratch/claude-cli-protocol/linux-2.1.330-stage2/tls-upstream-stage3.log)

## Linux 请求参考

### 普通客户端的主动生成分支

新增 `TestClaudeOAuthMimicOutboundContract` 使用普通客户端 UA、合成 OAuth 账号、显式 Linux SDK 配置与 `sdk-cli` 入口，经过实际 `GatewayService.Forward` 和生产 HTTP 传输到回环接收端。用例已扩展为 gzip、标准 deflate、br、zstd 四种 SSE：下游先读取完整首帧，上游才释放后续帧，同时核对最终文本、token usage、唯一选中账号的 Authorization 及无入站凭据泄漏。这个服务级用例关闭身份统一、系统提示注入和账单标记生成，不经过正式鉴权路由，也不验证真实账号。[出站联动测试](../backend/internal/service/gateway_claude_mimic_contract_test.go)

当前仍不能称为完整 CLI 模拟。最初的生成分支与原生样本存在压缩声明和 beta 差异；现有显式 Linux 配置已覆盖 Sonnet 4.5 的 UA、六项 SDK 字段、协议版本、超时、压缩声明和两个端点各自的 beta 集合。未选择配置的模型保留原策略，不能将单模型样本推广到所有功能组合。原生 CLI 请求透传、普通客户端主动生成、真实上游接受是三项独立验收。

官方 Linux CLI 非 bare 合成 OAuth `/context` 补充捕获成功：三次计数请求均使用 Bearer、无 API Key，beta 为 Claude Code、OAuth、interleaved-thinking、context-management、token-counting 五项，CLI 正确显示 `17+17=34`。它与 Messages 的六项集合不同，因此配置使用两个独立对象。[Linux OAuth 计数样本](../.scratch/claude-cli-protocol/oauth-context-linux-reference.json)

### 原生 CLI 透传样本

两份脱敏样本分别记录文本 1 轮和 `Read` 2 轮。请求头白名单里的入站值与转发给合成上游的值一致。`User-Agent` 为 `claude-cli/2.1.330 (external, sdk-cli)`，`anthropic-version` 为 `2023-06-01`，`x-stainless-retry-count` 为 `0`，`x-stainless-timeout` 为 `600`。`anthropic-beta` 包含 `claude-code-20250219`、`interleaved-thinking-2025-05-14`、`context-management-2025-06-27` 和 `thinking-token-count-2026-05-13`。 [参考导出实现](../backend/internal/service/gateway_claude_cli_contract_test.go)

以下 JSON 可作为本地协议对照时 `SUB2API_CLAUDE_COMPATIBILITY_PROFILE` 的显式配置值。六个字段来自官方 Linux CLI `2.1.330` 在 `--bare` 和测试 API Key 模式下发送的 SDK 头，其中 `runtime` 和 `runtime_version` 是 CLI 自报值。合成 OAuth 回环另有独立样本，生产出站配置仍待验收。 [配置说明](claude-compatibility-profile.md) · [OAuth 脱敏样本](../.scratch/claude-cli-protocol/linux-2.1.330-stage2/references-stage2/TestClaudeCLIConsumesGatewayOAuthStream.json)

```json
{
  "lang": "js",
  "package_version": "0.128.0",
  "os": "Linux",
  "arch": "x64",
  "runtime": "node",
  "runtime_version": "v26.12.1",
  "entrypoint": "sdk-cli"
}
```

### 本轮最终本地回归

新增配置与传输修复集成后，2026-11-27 的五个受影响包全量回归通过，进程退出码为 `0`：Claude 配置 `0.332s`、TLS 配置 `0.183s`、迁移工具 `0.415s`、传输仓库 `13.321s`、网关服务 `223.973s`。服务测试启用了已校验的 macOS CLI。测试包含标准 zlib/常见 raw deflate SSE，以及上游先发送响应头时的读取前关闭和读取中关闭，确认不等待首帧才能返回响应。[完整日志](../.scratch/claude-cli-protocol/stage4-full-regression.log) · [deflate 回归](../backend/internal/repository/http_upstream_deflate_contract_test.go)

### 本轮最终 Linux 回归

2026-11-27 03:20:17 UTC，服务器隔离目录中的三个测试程序全部通过，退出码均为 `0`，无跳过。普通客户端 OAuth 主动生成分支通过实际转发和 gzip SSE（`0.01s`）；标准/内置 TLS 信任及不可信 CA 拒绝、同名 TLS 模板切换、标准 zlib/常见 raw deflate 和首帧关闭用例全部通过；两条遥测路由都返回空 `200`。本轮没有使用真实 OAuth 账号。[Linux 完整日志](../.scratch/claude-cli-protocol/linux-2.1.330-stage4/results.log)

上传后逐个核验测试程序 SHA-256，随后执行同一文件：

- service：`e181dcc0167588efa4d8f19ae79e6ecc70fdc83fc8920af5434284be051f4311`
- repository：`f4667fb5b6a991a9582bf0ca1148ca8729cfc6a15004e8dcb25b4815d2b59f9d`
- routes：`0543560fbf6ae9de9c9ed9491b9c222fae5e0beecffd24a6f0fc4beee2325df7`

同时重验服务器上的官方 CLI 摘要，仍为本文件记录的 Linux `2.1.330` 发布件。

### 逐模型 beta 与四编码补充回归

逐模型/端点 beta 配置、压缩声明与相关策略处理集成后，本机受影响三包全量回归通过：Claude 配置 `0.337s`、迁移工具 `0.389s`、网关服务 `222.145s`。最终复核又修复了重复 JSON 配置键及最终模型 StructuredOutputs 拦截的边界，并重新运行受影响定向测试，三个包分别通过于 `0.349s`、`0.401s`、`1.216s`。[全量日志](../.scratch/claude-cli-protocol/stage5-full-regression.log) · [最终定向日志](../.scratch/claude-cli-protocol/stage5-final-targeted.log)

定向证据包括：四种编码的及时 SSE 首帧与最终用量；Messages 六项和计数五项有序 beta；模型别名归一化；入站别名与最终模型过滤规则的保留；配置与允许继承的客户端 token 命中 block 时零上游请求；计数 block 的 400 错误；beta 过滤与请求体净化保持一致。重复配置键用例先复现了旧解析器静默接受，修复后同时拒绝重复外层字段、重复模型键和归一化碰撞。

2026-11-27 03:51:50 UTC，最终 Linux 配置测试与实际出站联动均通过，退出码 `0`、无跳过。四种压缩流及全部十项 beta 策略子用例通过，服务联动总耗时 `0.02s`。上传后校验的 service 测试程序 SHA-256 为 `c27c595f8a6490377e154a41a376701dfba3bb3090d8d80af9321ab2ed99f6fc`，配置测试程序为 `91d247b88fa11deec4d4ea5202d28377e2c8ad87c4d375a1c9e16fc08641ddcb`。[最终 Linux 日志](../.scratch/claude-cli-protocol/linux-2.1.330-stage5/results.log)

同轮只读复核了应用数据库指向及账号元数据：非删除 Anthropic OAuth/SetupToken 账号总数仍为 0，活跃数为 0，专用测试候选为空。未查询凭据、邮箱、代理或余额，也未改动生产。真实账号登录、刷新和上游接收仍不能验收。

## 本地复跑

在 `backend` 目录运行，使用与测试机平台匹配的官方 CLI 绝对路径。未设置 `CLAUDE_CLI_TEST_BINARY` 时，真实 CLI 用例会跳过。

```bash
rtk proxy env CLAUDE_CLI_TEST_BINARY=/absolute/path/to/claude \
  go test -tags unit ./internal/service ./internal/handler -run '^TestClaudeCLI' -count=1 -v
```

```bash
rtk go test ./internal/server/routes -run '^TestCommonRoutesAcknowledgesTelemetryBatch$' -count=1
rtk go test ./internal/repository -run '^TestClaudeTLSUpstreamContract$' -count=1 -v
rtk go test -tags unit ./internal/util/responseheaders ./internal/service ./internal/handler \
  -run '^Test(FilterHeaders|ForwardCountTokensAnthropicRetryableErrorContract|AnthropicCountTokensSuccessPreservesAllowedRequestID|AnthropicPassthroughMessagesPreservesAllowedRequestID|AnthropicMessages)' -count=1
```

可选的 `CLAUDE_CLI_REFERENCE_DIR` 将允许的请求头、公开配置字段和请求体结构写入指定目录，用于比较入站与测试上游。 [参考导出实现](../backend/internal/service/gateway_claude_cli_contract_test.go)

本机四包的全量复跑命令如下。复跑时将 CLI 路径替换为当前机器上已核验的发布件路径。

```bash
rtk proxy env GOCACHE=/private/tmp/sub6api-gocache \
  CLAUDE_CLI_TEST_BINARY=/Users/leo/.local/bin/claude.orig \
  go test -tags unit ./internal/util/responseheaders ./internal/server/routes ./internal/service ./internal/handler -count=1
```

## 验收矩阵

完成性复核：安装及隔离运行、已选版本/模型的公开协议配置、响应错误/遥测 ACK、四编码流和可选 TLS 模板已有上述证据。它们没有证明实际默认链路完整模拟：普通客户端测试关闭了身份统一、系统提示注入和账单标记生成；真实 CLI 透传用例也不覆盖这些主动生成行为。下一项必要端到端验收须使用获授权的专用 OAuth 账号，先确认官方 Linux CLI 可完成一次请求，再验证代理默认链路、上游接收、刷新和错误恢复。当前没有这样的账号，不能把模拟凭据或本地成功代作完成凭据。其他模型/功能组合及真实代理链路继续保留为未验证，不因单模型通过而视为通过。

| 协议面 | 现有证据 | 尚未验证 |
| --- | --- | --- |
| 消息 | 官方 Mac 和 Linux CLI 在 `--bare` 模式通过 API Key 透传消费合成文本 SSE | 生产路由和真实模型上游 |
| 工具消息 | 官方 Mac 和 Linux CLI 本地执行 `Read`，网关转发下一轮的 `tool_result` | 其他工具调用的消息往返 |
| 计数 | Mac 和 Linux 官方 CLI `/context` 经两种转发模式显示合成计数，服务测试覆盖计数响应和请求 ID | 真实上游计数 |
| 错误 | Mac 和 Linux 官方 CLI 验证 Messages 429、529 终止、429 按 `Retry-After: 1` 重试及 SSE 错误重试恢复 | 真实 CLI 的计数错误处理 |
| 请求头 | 已导出 Linux CLI 的白名单请求头；普通客户端主动生成分支验证显式 Linux SDK/UA、压缩声明及 Sonnet 4.5 逐端点 beta 配置 | 其他模型/功能组合、部署环境的真实出站头 |
| 遥测批次 | Mac 和 Linux 两条批次路径返回 `200` 空响应，官方客户端的响应分支通过离线回放 | 真实第一方遥测投递 |
| OAuth | Mac 和 Linux 官方 CLI 均通过实际网关服务转发合成 OAuth 请求并隔离凭据 | 真实 OAuth 登录与刷新 |
| 传输 | Mac 和 Linux 通过进程级 CA 信任测试；显式参考模板与官方 CLI 在同一接收端的已采公开 ClientHello 字段一致并成功读取 SSE | TLS 会话恢复、未采随机字段、生产代理及真实上游连接 |

- 官方 CLI 的事件批次默认发往 `api.anthropic.com/api/event_logging/v2/batch`。官方源码说明，会话使用其他 API Base 时不会发送这类事件，正常网关配置因此通常不会命中本地遥测路由。协议测试关闭了非必要流量，现有证据覆盖本地应答和离线解析。 [Anthropic 遥测目标源码](https://github.com/anthropics/claude-code/blob/2287e8ceed6e46623a5b913308515db683e25fbe/mods/telemetry/hooks/entries/ingest-url.ts#L1-L8) · [协议测试设置](../backend/internal/service/gateway_claude_cli_contract_test.go)
- 官方 CLI `2.1.330` 使用 OAuth 配置选定 `POST /api/claude_code/metrics` 的主机，默认是 `api.anthropic.com`。普通 `ANTHROPIC_BASE_URL` 设置指定消息请求的网关地址，用于网关的 API Key 无法通过官方指标目标的凭据主机检查。因此普通自定义网关无需增加该指标路由。对官方二进制的 12 项离线回放确认，指标 `POST` 接受空的 `2xx` 响应体。独立的组织设置 `GET /api/claude_code/organizations/metrics_enabled` 要求返回布尔字段 `metrics_logging_enabled`。用户自行配置的 OTLP 指标发往所选收集器。 [官方发布清单](https://downloads.claude.ai/claude-code-releases/2.1.330/manifest.json) · [OTLP 配置](https://code.claude.com/docs/en/monitoring-usage#common-configuration-variables)

## TLS 验收范围

### 同一接收端的公开握手字段对照

本轮增加同一 Python/OpenSSL listener 的四种客户端对照：官方 CLI、Go 标准传输、既有内置模板、显式参考模板。四者均完成一次 HTTPS 请求并读取合成 SSE。macOS 与 Linux 的显式参考模板均得到 `profile_fields_match: true`，所采字段差异列表为空。[macOS 结果](../.scratch/claude-cli-protocol/tls-clienthello-profile-darwin.json) · [Linux 结果](../.scratch/claude-cli-protocol/tls-clienthello-profile-linux.json)

参考模板经现有创建处理器、内存测试仓库、账号模板选择及实际 `NewHTTPUpstream.DoWithTLS` 路径验证。它补齐了旧内置模板缺少的 X25519MLKEM768 key share 与两项签名算法，并采用原生样本的扩展顺序。它是独立的可选创建请求，没有覆盖内置模板或写入生产数据库。[可导入 JSON](examples/claude-linux-2.1.330-tls.json)

一致性覆盖 legacy version、套件及顺序、compression methods、扩展顺序及长度、supported groups、signature algorithms、ALPN、supported versions、point formats、PSK modes、key share 组及长度。采集器不保留随机数、会话 ID、key share 字节或 HTTP 凭据；结果不代表逐字节相同，也不验证 TLS 会话恢复及真实上游识别。

Linux 新探针 SHA-256 为 `d19dd875d9f030fa6d5e446b4d222e7e9dd5598a736f254261e5ad567aa697d1`，模板为 `59920f489c0584e603f96220e04c4b916cf08d8e52f47722ec80a35d4e20e695`，上传后核验一致。[含摘要的执行日志](../.scratch/claude-cli-protocol/tls-clienthello-profile-linux-output.log)

### 已有信任与流式传输回归

- Mac 和 Linux 官方 CLI `2.1.330` 直连各自的回环 HTTPS 探针。加载进程级测试 CA 的一组均协商出 TLS 1.3、`TLS_AES_256_GCM_SHA384` 和 ALPN `http/1.1`，发出一次 HTTP/1.1 消息请求，消费合成 SSE 后退出 `0`。Linux 耗时 `0.291s`。缺少测试 CA 的一组收到零个消息请求，CLI 退出 `1`。Mac 服务端记录 `UNEXPECTED_EOF_WHILE_READING`，Linux 记录 `ConnectionResetError` 并耗时 `0.143s`，两平台均无超时。探针服务仅提供 `http/1.1` ALPN。 [回环 HTTPS 探针](../.scratch/claude-cli-protocol/tls_reference_probe.py) · [Linux HTTPS 结果](../.scratch/claude-cli-protocol/linux-2.1.330-stage2/tls-reference-linux.json)
- Mac 和 Linux 的 API Key 及合成 OAuth 消息回环走普通 HTTP。两平台另用实际 `NewHTTPUpstream` 做 Go 出站测试，标准传输和 Anthropic OAuth 内置 TLS 模板均可在受信任连接上收到已刷新的 SSE 首帧，随后读完流，并拒绝未知 CA。Linux 两种传输均协商出 TLS 1.3、`TLS_AES_128_GCM_SHA256`，实际 HTTP 协议均为 HTTP/1.1。标准传输未协商 ALPN，内置模板协商出 `http/1.1`。用例总耗时 `0.02s`。 [Go 出站 TLS 回归](../backend/internal/repository/claude_tls_contract_test.go) · [Linux Go 出站 TLS 日志](../.scratch/claude-cli-protocol/linux-2.1.330-stage2/tls-upstream-stage3.log)
- 前一轮官方 CLI 探针使用 Python/OpenSSL 服务，Go 出站测试使用 `crypto/tls` 服务。协商密码套件由客户端和服务端共同决定，不能仅从这两组协商结果推断客户端是否一致。本轮同一接收端的公开 ClientHello 字段对照见上节。
- TLS 模板适用于显式开启该功能的 Anthropic OAuth 或 SetupToken 账号。网关会解析已绑定或随机选择的模板，缺少可用模板时使用内置默认值。 [账号设置](../backend/internal/service/account.go#L2477) · [模板选择](../backend/internal/service/tls_fingerprint_profile_service.go#L177)
- 源码中的 HTTPS 直连、HTTP CONNECT 和 SOCKS 代理可以走自定义 TLS 拨号，HTTPS 代理由 Go 标准传输处理。本轮 TLS 回环覆盖直连，HTTP CONNECT、SOCKS 和 HTTPS 代理的握手待验证。新增模板切换回归已通过：同名模板修改密码套件后，新连接协商出新值；前后各自内容相同的模板仍复用同一 TCP 连接。[模板切换回归](../backend/internal/repository/http_upstream_tls_profile_test.go)
- 不同模板内容各占一个缓存条目，旧模板按现有空闲回收和容量限制逐出，不中断在途 SSE。达到客户端缓存上限且全部条目仍在使用时，新模板请求可能被现有容量保护拒绝；模板切换不承诺立即回收所有旧连接。
- 部署宿主机是 Debian Linux x86_64，应用容器为 Alpine 3.21.10。服务器临时目录中的验收 CLI 独立于应用容器。对 `sub6api-app-1` 的只读检查显示，容器环境未设置 `SUB2API_CLAUDE_CLI_VERSION` 或 `SUB2API_CLAUDE_COMPATIBILITY_PROFILE`。实际生效的管理端配置、出站请求头和 TLS 模板仍待在真实连接上核验。 [Claude 出站兼容配置](claude-compatibility-profile.md)
