# Claude Code 网关：外部一手资料核验

核验日期：2026-11-23（Asia/Shanghai）。本报告只核验公开资料；未操作生产、账号、客户端配置或业务源码。由主代理另行核验本地 CLI 与 sub6api。

交付边界更新：以 [透明诊断 SPEC](../../.scratch/claude-oauth-compatibility-20261123/SPEC.md) 为准，仅面向 Anthropic OAuth 增加 mode/attribution_mode=transparent、integrity_status=unverified 的只读观察和两种 opaque 头日志脱敏。此前 default/omit、归因删除与客户端 attribution-off 的交付建议已撤回；本报告保留官方配置事实，不将其作为启用建议或授权。不伪造 first-party/政府版身份，不移除/篡改 cc_gateway 或遥测归因，不生成 opaque hash，不新增身份改写。transparent 不等于既有网关无变换。

## 1. 帖子说了什么，证据到哪里

| 说法 | 资料性质与结论 |
| --- | --- |
| 非白名单 BASE_URL 会产生 `cc_gateway`，灰度 `tengu_wiggly_dove` 会产生 `x-claude-code-wiggly-dove` | 作者的逆向观测声明；正文没有附带可复现完整程序、精确包摘要和原始请求记录，本报告未独立复现。 |
| 该结构含 git remote、校验值，并导致封号 | 字段描述与风控因果必须分开：正文将二者连接，但没有对照实验或官方说明能证明封号因果。 |
| 改 HOST/DNS/CA 可解决 | 回帖明确是猜测，作者也说未试；不是可验收的功能规格。 |

来源：[主帖，2026-10-18 起](https://macos.do/t/topic/280)。帖子标题限定为 2.1.30x，不能外推所有版本或所有灰度账号；配图附件未在本次做独立 OCR 或真实性检验。

关联[原帖，2026-09-29](https://macos.do/t/topic/210)聚焦 2.1.283 的 `cc_version` 短后缀与 JavaScript UTF-16/Go UTF-8 差异，并指向固定提交的网关代码；这是具体兼容性缺陷线索，不证明另一结构的编码、哈希或封号机制。`cc_version` 短后缀、`cch`、不透明请求头是不同对象，不能按同一算法处理。

## 2. 官方已经公开的兼容边界

### 网关与认证

官方明确支持通过 `ANTHROPIC_BASE_URL` 连接第三方网关。网关凭证会替代已保存的订阅凭证；只配置 BASE_URL 则不必然替换订阅。适用范围是客户端连接方式与接口兼容，不是对某个共享账号模式或内部风控的认可。[Other LLM gateways](https://code.claude.com/docs/en/llm-gateway)

### attribution 不是 HTTP header

官方描述的是 **system 首个独立块**：含客户端版本和会话派生指纹。直连 Anthropic 时，原样且位于首位才被端点剥离；移动、合并、重排会影响模型输入/缓存，合并时甚至可能一起丢掉系统内容。若网关必须重排 system，官方提供**客户端环境变量** `CLAUDE_CODE_ATTRIBUTION_HEADER=0` 省略该块；不是服务端返回头，也不是隐私或账号安全保证。官方还列出直连 auto-mode classifier 的例外。[Gateway compatibility：attribution](https://code.claude.com/docs/en/llm-gateway-protocol#system-prompt-attribution-block)

上段是公开协议事实；**当前任务不采用该关闭选项**，也不新增服务端归因省略策略。现有客户端非必要流量配置保持基线；退出产品遥测与省略模型请求归因不是同一操作。

### 头与 provider 能力

Messages 模式要求保留 `anthropic-version`/`anthropic-beta`；使用调用方 claude.ai 登录向 Anthropic 认证时还须保留 Authorization。session/agent/hint 头可供网关消费；新增 Anthropic 头和 body 字段应按开放集合考虑。非 Anthropic provider 需要协议桥接。该原则提示审查现有白名单与 beta/body sanitize 的取舍，**不能据此直接删除现有能力过滤**，更不证明未知头可在改写 body 后继续有效。[Gateway compatibility：request headers](https://code.claude.com/docs/en/llm-gateway-protocol#request-headers)

## 3. 客户端遥测与模型请求必须分开

官方把产品 metrics/error reporting 与模型 API 请求分开：文档称 metrics 不含代码、prompt、路径；可用 `DISABLE_TELEMETRY=1`、`DISABLE_ERROR_REPORTING=1` 分别退出。`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` 覆盖更多流量，但并非所有外联开关。该声明的对象是文档所述产品遥测，不能据此证明模型请求内不存在其他归因字段，也不能证明 git remote 不会经用户上下文进入模型请求。[Data usage](https://code.claude.com/docs/en/data-usage#telemetry-services)

`CLAUDE_CODE_ATTRIBUTION_HEADER=0` 控制 system 块；`DISABLE_TELEMETRY`/非必要流量变量有非空即启用的语义（`0` 也可能启用退出行为），并影响 feature flags。应在实际客户端版本下校验，而不是由 sub6api 服务端假设已经生效。[Environment variables](https://code.claude.com/docs/en/env-vars)

## 4. 版本证据与未验证项

官方 changelog 的 **2.1.181** 条目确认修复 custom BASE_URL/Foundry 的逐请求 attestation token 造成缓存失效；**2.1.299** 又记录 Claude apps gateway attribution 字段及 Desktop Code tab 的关闭配置。这表明结构确实随版本演进，旧版本算法不应当作永恒契约。[官方 CHANGELOG](https://raw.githubusercontent.com/anthropics/claude-code/main/CHANGELOG.md)

2026-11-23 的本次官方文档检索未获得能解释 `cc_gateway`/`wiggly` 内部校验的条目；对已打开 changelog 的同名查找也未命中。**这只记录本次检索边界，不证明机制不存在。** 官方资料没有给出本次所需的 wiggly 编码、关联字段、哈希覆盖范围或封号因果。本次外部资料核验未做启用/关闭灰度的客户端对照、真实上游请求、账号封禁实验或图片真实性核验。后续已验真官方 CLI 的隔离运行另见 [官方运行证据](claude-code-official-runtime-20261123.md)，不追溯视为本轮官方文档证明。

## 5. 研究推论与现行交付约束

1. 只观察既有 Anthropic OAuth 转发，不新增出站变换；保留真实来源、cc_gateway、遥测归因与未知字段，不伪造任何 first-party/政府版身份或 opaque 签名。
2. 官方客户端控制项作为事实保留；旧 default/omit、归因删除与 attribution-off 建议不交付，不新增或改变客户端配置生成。服务端不能替用户关闭绕过网关的外联。
3. 使用可追溯官方工件、隔离配置与合成凭证取得本地版本样本，再比较网关最终 HTTP 请求；解释基线变换，不将本地观察等同真实上游或完整 TLS 行为。
4. `wiggly` 完整性关系保持 unverified；诊断与显式调试日志不得留 opaque 原值。不把“防封”作为验收，不将 HOST/DNS/CA 劫持纳入实现。
