# Claude Code 网关兼容性：sub6api 缺口与补充方案

状态：Anthropic OAuth 只读透明诊断已实现并通过本地定向验收；保留初次调研证据，旧 default/omit 与关闭 attribution 的建议不交付。
日期：2026-11-23（UTC+08:00）。
入口：[MACOS DO 主题 280](https://macos.do/t/topic/280)。
源码基线：sub6api HEAD a3eaf63af，分支 codex/sub6-core-oauth-20261122；分析包含当前工作区已有修改，不代表这些能力均已提交或部署。

现行约束：[SPEC](../../.scratch/claude-oauth-compatibility-20261123/SPEC.md) 优先于本报告的历史建议。仅面向 Anthropic OAuth 增加只读透明诊断及两种 opaque 头的日志脱敏：保留真实来源，不新增身份改写，不移除/篡改 cc_gateway 或遥测归因，不伪造 first-party/政府版身份，不生成 opaque hash；既有出站行为保持基线。`transparent` 是本次观察模式，不表示现有网关全字节透传，也不表示完整性已经验证。

## 1. 建议解决什么问题

将需求定义为“Anthropic OAuth 请求兼容性与归因诊断”：管理员读取归因标记的存在、位置、基线改写状态与兼容性未知项；不展示敏感原值，不提供归因省略或身份伪造控制。

现有请求头、身份隔离和 billing 逻辑作为明确记录的基线保留；本次只观察，不新增出站选择策略。不把“去掉一个标记”定义为避免账号封禁的验收标准。

初次调研阶段未修改业务源码、账号、客户端配置或部署；随后已按 SPEC 完成服务观察、管理员只读 API、账号编辑页诊断及两种 opaque 头的显式调试日志脱敏。当前源码的后端定向验收为 104 个 pass 事件（46 个顶层测试）、0 fail，官方请求比较 12 个场景全部通过；前端 151 项测试、ESLint 与完整类型检查通过。详见[官方运行与比较证据](claude-code-official-runtime-20261123.md)及[逐项交付审计](../../.scratch/claude-oauth-compatibility-20261123/COMPLETION-AUDIT.md)。

真实官方 Claude Code 2.1.330 在隔离配置、合成凭证和 loopback 接收端下完成捕获；比较保留并解释既有补归因、beta 与 body 净化差异，未验证真实账号认证、付费上游接受、完整 TLS/字节等价或浏览器渲染验收。全部改动尚未提交或部署，已有无关未提交变更保留；当前停止产品代码修改和大型测试排队，优先让出资源给 Core OAuth。

## 2. 帖子与官方资料的证据边界

1. 主帖讨论 cc_gateway 和 x-claude-code-wiggly-dove，并推测与中转识别和封号有关。相关版本、字段用途与执行条件需要按客户端样本核实；封号因果未被该讨论证明。[帖子](https://macos.do/t/topic/280)
2. x-anthropic-billing-header 这个名字在此指 system 中的归因文本块，不能当成普通 HTTP 头处理。官方要求它保持为首个独立 system 块；需要重排时，提供客户端环境变量 CLAUDE_CODE_ATTRIBUTION_HEADER=0。这是客户端兼容设置，不是服务端响应头，也不提供防封承诺。[官方协议](https://code.claude.com/docs/en/llm-gateway-protocol#system-prompt-attribution-block)
3. Claude Code 产品遥测与 Messages 模型请求分开处理。网关只能控制经过自己的流量，无法替客户端关闭直接外联；退出可选遥测不能推出请求归因随之消失。[官方 data usage](https://code.claude.com/docs/en/data-usage#telemetry-services)
4. 官方已发布专门的网关协议兼容指南，因此应按 provider、认证方式、能力 beta 和客户端版本建立兼容策略。官方关于头透传的要求不能直接替代现有能力协商与 sanitize 判断。[官方请求头要求](https://code.claude.com/docs/en/llm-gateway-protocol#request-headers)

外部资料与版本限定详见 [一手资料核验](claude-code-gateway-primary-sources-20261123.md)。
本地客户端工件的来源与版本限制详见 [客户端证据](claude-code-client-evidence-20261123.md)。
后续已验真官方 CLI 的隔离运行与本地假上游证据，单独见 [官方运行证据](claude-code-official-runtime-20261123.md)；不能把后续证据追溯归入早期第三方备份工件。

本地补充核验：本机 claude 命令实际经 clawgod 包装器启动；其持有的备份工件声明版本 2.1.330。该工件可静态追到 cc_gateway 的构造及 wiggly 的 feature-gate/字段映射/头写入链，含可选 gitOrigin 字段，但备份来源未与官方原包认证，当前模块也有第三方修改，且本次没有观测真实发送。已观察到的头名识别 hash 不能证明 body/header 签名或服务端校验关系。因此可以据此设计版本化采样，不能据此推导出可通用重签的算法或风控效果。

## 3. sub6api 已有能力与实际缺口

| 项目 | 初次调研基线行为 | 本次透明诊断边界 |
| --- | --- | --- |
| billing 版本与短后缀 | OAuth 路径根据最终 UA 同步 cc_version；短后缀按原始首条 user 文本和版本计算 | 仅观察既有变换；不改算法，不把 3 位后缀当作 wiggly 或通用 body 签名 |
| system 归因块 | 原生 CC 缺块时可补块；mimic 构造归因文本，包含 cch=00000 | 区分客户端来源与网关生成；新增归因字段结构化识别，保留未知字段 |
| cc_gateway | billing 同步只替换版本相关片段，原有其他文本保留；所核对路径没有针对 cc_gateway 的解析策略 | 只解析归因块，不在普通用户消息里搜索替换；先提供诊断 |
| wiggly HTTP 头 | allowedHeaders 与 claudeCodeOptionalHeaders 的当前显式集合均不含该名称 | 显式记录“客户端携带/现有白名单未转发”，样本验证前不直接放行或自行生成 |
| 其他 opaque 头 | 两张集合已含 x-anthropic-additional-protection；不能凭名称断言它等同 wiggly | 分别识别、版本限定，验证是否与被重写内容相关；不可一概删掉所有 Claude 头 |
| 账号身份与会话 | 已有账号级 fingerprint、metadata.user_id 重写、父子 session 关联映射 | 明示这些既有变换，不新增身份改写或伪造 |
| messages/count_tokens | 已有两条请求构造与对应 billing 同步 | 诊断覆盖两条入口及已接入桥接/重试路径，不改变既有转发策略 |
| SDK 配置与 TLS | 已有 compatibility profile、账号 TLS 配置与显式缓存迁移工具 | 配置值来自已验证样本；HTTP 头一致不代表 TLS 或完整客户端一致 |
| 客户端非必要流量配置 | UseKeyModal 的 shell 与 settings.json 已默认输出 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 | 保留既有配置；不新增 attribution 关闭选项，不改生成器的默认或归因行为 |
| 调试日志 | debugLogGatewaySnapshot 可写入完整头/body；safeHeaderValueForLog 目前只对 authorization/x-api-key 脱敏，且仅配置调试输出时进入该路径 | 新增诊断时一并对 opaque 元数据做显式脱敏，避免“出站未转发但调试日志留原值” |

源码依据：

- [gateway_billing_block.go](../../backend/internal/service/gateway_billing_block.go)：23–48 的短后缀算法、79–108 的 user 文本提取、111–125 的 mimic 归因块。
- [gateway_billing_header.go](../../backend/internal/service/gateway_billing_header.go)：21–24 保存改写前文本；36–90 选择版本来源并同步；106–139 补齐缺失块。
- [gateway_upstream_request.go](../../backend/internal/service/gateway_upstream_request.go)：21–245，身份改写、billing 同步、beta/body sanitize、白名单、最终请求构造。
- [gateway_count_tokens.go](../../backend/internal/service/gateway_count_tokens.go)：548–717，对应计数请求构造；与 messages 的对称性需要一起验收。
- [gateway_service.go](../../backend/internal/service/gateway_service.go)：429–500，两张请求头集合。
- 同文件 283–291 的 safeHeaderValueForLog、1719–1766 的 debugLogGatewaySnapshot：调试输出与现有脱敏边界；不据此声称线上开启了调试。
- [identity_service.go](../../backend/internal/service/identity_service.go)：304–332 应用账号指纹；366–436 重写身份并保留 metadata 扩展。
- [现有兼容配置说明](../claude-compatibility-profile.md)：显式配置、缓存迁移与 TLS 验证边界。

补充：某些注释仍使用 CCH 签名措辞，但上述构造路径与 billing helper 不足以证明存在适配新版 opaque 元数据的通用签名算法。不能据注释宣称已实现帖子所说的完整性校验。

## 4. 当前已确定的最小实现边界

### 4.1 只读透明诊断

引入共享的安全观察机制，供 Anthropic OAuth 的 messages 与 count_tokens 使用；不接管非目标账号，不改变既有转发策略：

1. 在现有改写前后提取受控事实：客户端版本、归因块存在与位置、cc_gateway/cch 与两种 opaque 头是否存在、body/身份/beta 是否按基线改写。区分客户端提供和网关生成，保留真实来源与未知归因字段。
2. 仅存放有界、进程内的各账号最新观察：上限 1024 个账号、15 分钟 TTL、并发安全与返回副本；mode/attribution_mode 均为 transparent，integrity_status 始终为 unverified。不保存 opaque 原值、设备标识、git remote 或完整请求正文。
3. GET /api/v1/admin/accounts/:id/claude-compatibility 复用管理员鉴权、正数 ID 与账号存在校验；非 Anthropic OAuth 返回 400。只读返回 account_id、attribution_mode 和 observation（无有效观察时为 null），不主动探测或改变账号。
4. 前端仅展示诊断并提供刷新、空/错误状态与账号切换竞态保护；不提供 suppression 开关。显式调试日志对 wiggly/additional-protection 原值脱敏。归因或 opaque 头存在不触发封号、下架、刷新凭证。

客户端现有锚点：[UseKeyModal.vue](../../frontend/src/components/keys/UseKeyModal.vue) 初查时 877–926 的 generateAnthropicFiles，覆盖 unix/cmd/powershell 和 settings.json。已有 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 维持基线；本次不添加 attribution-off 配置，不借此删除或改写归因。

### 4.2 已撤回的旧建议与保留的研究边界

此前讨论过账号 default/omit 策略、删除归因块及客户端 attribution-off 选项。**这些建议已由新 SPEC 撤回，不属于交付；本任务新增的对应设置、删除逻辑与生成配置须撤回。** 官方存在相关配置的资料事实仍保留，但不等于用户授权采用。

1. 官方网关协议覆盖 API Key 与调用方 claude.ai OAuth 透传，但不能据此推导允许替换订阅身份或任意混用账号身份。本次限定 Anthropic OAuth，其他账号只做排除验证。
2. 不移除/篡改 cc_gateway、遥测归因或未知归因字段，不伪造 first-party/政府版身份；不生成 opaque hash，不把 cch=00000 或短后缀冒充其算法。
3. 保留原有 provider 路由、代理、身份及能力净化基线；对差异明确标为既有变换，不能以 transparent 名称宣称无变换或上游完整等价。
4. 如果后续发现 opaque 元数据存在绑定关系，只记录证据与未知项；改变转发/拒绝/回退行为属于另一个需明确规格的任务，不能在当前只读诊断中悄悄加入。

当前范围已由 SPEC 固定，不再将“是否只做 OAuth”或“是否采用 omit”作为待选交付分支。

## 5. 验收设计

1. 已验真且未修改的官方 CLI 使用隔离配置、合成凭证访问 loopback，随后将脱敏捕获通过实际 GatewayService Forward 发送到本地接收端；解释基线变换，不宣称真实上游/TLS 完整等价。
2. 诊断覆盖 messages/count_tokens、native/mimic、缺失/未知/畸形归因及无关账号排除；新代码保留来源与未知字段，不新增身份或归因变换。
3. 验证观察缓存 TTL、上限、并发与副本隔离，诊断与两种 opaque 头调试日志不暴露敏感原值；integrity_status 不因字段齐全变为 verified。
4. 管理 API 验证鉴权、ID、账号存在与严格类型范围；前端覆盖刷新、空/错误、账号切换竞态且没有 suppression 选项；原有客户端配置不被任务改写。
5. 运行定向 service/handler/frontend 测试、受影响回归与类型检查，按最终任务 diff 做 Standards/Spec 两轴审查；不执行生产部署或真实账号更改。

初查只读核对过 gateway_billing_header_test.go 与 gateway_cli_version_runtime_test.go。后续实际测试、CLI 捕获与网关比较结果应以各阶段证据及最终交付记录为准；本文所列验收项不等于已通过。不进行真实上游推理或生产验收。

## 6. 当前阶段与证据责任

流程已从 ask-matt → research 进入按 SPEC 分工实施与验收。根代理负责集成与最终状态；本报告仅记录事实、已撤回建议和现行边界，不代替实现或验收报告。

外部资料代理曾独立审查官方协议适用范围，以及客户端省略 attribution 后旧服务端仍可能补回的边界。该历史兼容结论保留，但不再衍生 omit 控制。三份初期研究与后续官方运行证据分层保存；禁止把第三方备份冒充官方工件、把本地捕获冒充真实上游或防封验证。

图谱证据为 Verify：project Users-leo-Projects-pixcook-ai-sub6api，generation 2026-11-21T21:14:57Z。已对引用后端路径做 coverage；identity_service.go、constants.go 等变更路径回读当前文件，未跟踪的兼容配置文档也回读。初次宽查询未穷尽全仓符号，结论限于所列路径，不宣称完成全仓安全审计。
