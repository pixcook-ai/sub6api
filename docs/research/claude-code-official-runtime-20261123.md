# 官方 Claude Code 运行态抓包（2026-11-23）

## 已完成的证据

实际执行从 Anthropic 官方 npm scope 下载、完整性核验通过的未修改
Claude Code darwin-arm64 2.1.330。默认 attribution 和显式关闭两组均实际连接
受控本地 HTTP 接收端，分别收到一次 Messages 请求，CLI 都成功解析合法 SSE，
退出码 0、结果 CAPTURE_OK、stderr 为空。本次使用 synthetic OAuth token，
不使用真实账号，也不转发到付费或认证上游。

持久化夹具的首次抓包时间：2026-11-23 00:27:28 +08:00
（manifest 的 UTC 为 2026-11-22T16:27:28Z）。版本是本次固定验证的 2.1.330，
没有声称它是最新版本。

## 来源与未修改校验

- npm 主包元数据：https://registry.npmjs.org/@anthropic-ai%2fclaude-code/2.1.330
- 实际平台包元数据：https://registry.npmjs.org/@anthropic-ai%2fclaude-code-darwin-arm64/2.1.330
- 实际平台包 tarball：https://registry.npmjs.org/@anthropic-ai/claude-code-darwin-arm64/-/claude-code-darwin-arm64-2.1.330.tgz
- tarball 为 164285735 字节，SHA-256：b8c91d706567b670e4f2b6112199c5db3125ca237ac214e6143e7094bd0cb33b。
- npm dist.integrity 精确匹配：sha512-rgzAjrRJJsIdZXGF9CCFFbURxKuJNobRr+krV3dNmqNJWIws74rLD0CABe4+nXlx/9WwRECTrveprWB96mubqw==。

实际执行的 package/claude 为 363855808 字节，SHA-256：
f7c5093b9dd6560d08af28bc27b123fb55091deae3180ac39df199522da86815。
脚本再次对已核验 tarball 内该成员逐字节流式计算哈希，确认提取后执行物一致。
没有修改其源代码、二进制或请求构造路径；没有调用系统安装的 clawgod launcher。
未额外验证 npm registry 发布签名，manifest 对此记录 false；来源保证为 HTTPS
官方 npm scope 元数据加 dist.integrity，不扩大为完整供应链审计。

包和元数据保留在任务 scratch 的 official-runtime 目录；大包不属于提交产物。

## 两组实际观测

| 项目 | attribution 默认 | attribution 显式关闭 |
|---|---|---|
| CLAUDE_CODE_ATTRIBUTION_HEADER | 未设置 | 0 |
| CLI exit code / 输出 | 0 / CAPTURE_OK | 0 / CAPTURE_OK |
| 接收端请求数 | 1 | 1 |
| 请求方法与路径 | POST /v1/messages?beta=true | POST /v1/messages?beta=true |
| 认证观测 | Bearer；没有 X-Api-Key | Bearer；没有 X-Api-Key |
| User-Agent | claude-cli/2.1.330 (external, sdk-cli) | 相同 |
| system 块数 | 3 | 2 |
| attribution block | 1 | 0 |
| attribution 文本 | x-anthropic-billing-header: cc_version=2.1.330.db5; cc_entrypoint=sdk-cli; | 无 |
| cc_gateway / cch | 本次均未出现 | 本次均未出现 |
| wiggly / additional-protection | 本次均未出现 | 本次均未出现 |
| 模型 / tools 数 | claude-sonnet-5-5 / 11 | 相同 |

两组实际 SDK 头均为 x-stainless-package-version=0.128.0、os=MacOS、arch=arm64、
runtime=node、runtime-version=v26.12.1、retry-count=0、timeout=600。
这是此官方 native 工件实际发送的兼容头；不代表宿主系统安装或执行该 Node 版本。
不据此自动覆盖 sub6api 已有账号画像。

这些请求保留了官方构造的 tools、thinking、context_management、output_config、
metadata 和 system 布局。metadata.user_id 实际为 JSON 对象字符串；持久化前将
device/account/session 身份替换为固定 synthetic 值，没有把真实设备 ID 保存到夹具。

## 隔离、运行方式与复现

可复用脚本：scripts/claude-oauth-compare/capture_official.py。
详细使用说明：scripts/claude-oauth-compare/README.md。

    rtk proxy python3 scripts/claude-oauth-compare/capture_official.py \
      --version 2.1.330 \
      --artifact-dir .scratch/claude-oauth-compatibility-20261123/official-runtime \
      --fixtures-dir backend/internal/service/testdata/claude-official

真实 CLI 参数：--print --output-format json --max-turns 1 --no-session-persistence
--disable-slash-commands；输入为 Reply with the single word CAPTURE_OK.。
--print 模式实际入口为 sdk-cli，不能把此结果扩展成互动终端 cli 入口的完全等价。

必要环境变量使用完全隔离的字典，未继承用户模型/账号/插件配置：

- HOME、CLAUDE_CONFIG_DIR、TMPDIR、cwd 均为临时目录；用户 ~/.claude 没有改写。
- CLAUDE_CODE_OAUTH_TOKEN 是明显标记的 synthetic 安全测试值；fixture 不保存其值。
  接收端观察到 Bearer，证明本轮走了 token 构造，但没有验证 OAuth 登录、scope 或真实账号。
- ANTHROPIC_BASE_URL 为 127.0.0.1 动态端口；HTTP_PROXY、HTTPS_PROXY、ALL_PROXY
  同样指向该接收端，NO_PROXY=127.0.0.1,localhost。接收端拒绝任何外部代理转发。
- 两组统一 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1、DISABLE_ERROR_REPORTING=1、
  DISABLE_TELEMETRY=1、CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1、CI=1。
- 每轮请求返回合法 message_start/content_block/message_delta/message_stop SSE。
  服务端在 finally 中 shutdown/server_close，线程 join；CLI 结束后没有任务接收端残留。

## 相对路径命令复跑（2026-11-23）

集成复跑首先暴露真实失败：README 的相对 artifact-dir 路径虽然能完成
acquire 完整性核验，但随后在 subprocess.run 的隔离 cwd 下找不到相对的
native/package/claude，抛出 FileNotFoundError。脚本现仅将 acquire 找到的
executable 路径执行 resolve()，其工件核验与请求构造流程不变。

修正后从仓库根目录按 README 的相对路径方式实际复跑，输出写入
.scratch/claude-oauth-compatibility-20261123/official-runtime-recheck，
没有覆盖 backend/internal/service/testdata/claude-official。
manifest 的本次 captured_at 为 2026-11-23T03:17:17Z
（2026-11-23 11:17:17 +08:00）。default/off 两次均 exit_code=0、
request_count=1、messages_requests=1、completed=true，stdout 结果为
CAPTURE_OK，stderr 为空，均实际收到 POST /v1/messages?beta=true。

复跑重新计算 tarball 和 executable SHA-256，分别仍为
b8c91d706567b670e4f2b6112199c5db3125ca237ac214e6143e7094bd0cb33b 和
f7c5093b9dd6560d08af28bc27b123fb55091deae3180ac39df199522da86815，
与原 manifest 完全一致。原 testdata 的两份 fixture 及 manifest 三文件
SHA-256 均与复跑前相同。

这条复跑命令的 GREEN 仅证明相对路径启动和真实官方 CLI 双捕获成功；
后续网关 Go 比较的独立验收见下文，不扩大到真实 Anthropic 上游验收。

## 夹具与比较边界

持久产物为 backend/internal/service/testdata/claude-official 下：

1. claude-2.1.330-attribution-default.json。
2. claude-2.1.330-attribution-off.json。
3. manifest.json，包含来源、工件哈希、每份 fixture 哈希、两组 completed=true 和脱敏说明。

夹具中的 requests[0].headers/body 是接收端实际抓到、再脱敏后的请求。
authorization 已隐藏；metadata 使用固定 synthetic 身份；用户 home 和临时路径已替换。
原始 Content-Length 被保留为线缆观察值，脱敏后 JSON 长度可以变化。对比应排除 Host、
transport 长度、临时 ID，以及 sub6api 有意的账户身份/会话映射，逐项解释差异。

官方客户端输入侧抓包与网关输出侧比对是两项独立证据；后者由根代理在
当前 root 工作树重新编译并集成执行，结果如下。

## 网关比较的已知归一化与 RED 记录

初次诊断运行的 12 个比较子场景中，10 个 native/body-recognized 场景在
messages JSONEq 失败，两个 off + no-UA mimic 场景通过。RED 日志保留在
.scratch/claude-oauth-compatibility-20261123/backend-service-final-testbinary-20261123.jsonl。
该二进制来自 .recovered 工作树；根代理确认本任务 Claude Go 文件一致，但仍有
16 个 Core/OpenAI/仓储文件不同，因此本轮仅用作差异诊断，不能作为当前 root
全树的最终验收。

实际发现的差异是 messages[1].output_config 被移除。夹具在这条有正文的
system 消息内带有 effort=medium，却没有请求
mid-conversation-output-config-2026-07-01 beta。当前 root 的既有
gateway_request.go:1051–1054、1099–1173 按最终 beta 清除此消息级字段，
保留 role/content/其余字段；顶层 output_config 不受影响。
gateway_mid_conversation_output_config_test.go:34–41、78–91 已记录并断言
同一规范。上述两个既有文件相对 HEAD 没有修改，本任务 review.patch 也没有
改动该净化逻辑，不能把此差异归因于新诊断 hook。

比较测试对 native/body-recognized 场景只从期望 messages 显式删除
1.output_config，并断言入站和最终
出站 beta 均缺该 token、原字段精确为 effort=medium、顶层 output_config
仍完整保留。其余 messages 字段继续整体 JSONEq，不调用生产 sanitizer
构造期望，也不泛化忽略未知差异。当前 root 重新编译后的结果为 GREEN，
详情及其覆盖范围见下一节。

count_tokens 子场景使用同一官方 Messages 抓包改换入口，并非官方 CLI
实际发出的 count_tokens 抓包；其额外已知归一化是移除 stream/max_tokens。
本比较不启用 identityService，也不证明 TLS、真实 OAuth 账号认证、所有
SDK/UA/beta 头或全部顶层字段完全等价。

## 当前 root 的真实 GREEN 与比较矩阵

根代理随后在 /Users/leo/Projects/pixcook-ai/sub6api/backend 实际重新编译
并执行 -tags=unit 定向回归，GOMOD 核对为该目录的 go.mod。命令记录在
.scratch/claude-oauth-compatibility-20261123/backend-current-root-command-20261123.json，
其中保存 11 个任务 Go 源码的 SHA-256；报告更新时逐项重算均匹配。
实际日志为同目录 backend-current-root-final-tests-20261123.jsonl，
结果汇总为 backend-current-root-final-result-20261123.json。

service、admin handler、routes 三个包 exit_code=0；104 个测试 pass 事件
包含 46 个顶层用例及其子用例，0 fail。routes 本次仅匹配既有的
TestGatewayRoutesOpenAICountTokensPathIsRegistered；不能据此声称 Claude
诊断端点的完整 HTTP 路由验收。之前 .recovered 的 RED 仅保留为诊断记录，
本节当前 root 新编译、新运行的结果才是此轮最终后端定向验收。

TestGatewayService_ClaudeOfficialRuntimeLocalHTTP 的 12 个子场景全部
通过。下表从本次真实 t.Log 逐项摘录；native 指保留官方抓包 UA，
no-UA/body 指删除 UA 后仍被 billing block + metadata 识别，
no-UA/mimic 指删除 UA 且原夹具关闭 attribution 后进入既有 mimic 分支。

| endpoint | 输入夹具/变体 | 客户端条件 | mimic | attribution 块数 | body_changed | beta_changed |
|---|---|---|---|---|---|---|
| messages | default | native | false | 1→1 | true | false |
| messages | default | no-UA/body | false | 1→1 | true | false |
| messages | off | native | false | 0→1 | true | false |
| messages | off | no-UA/mimic | true | 0→1 | true | true |
| count_tokens | default | native | false | 1→1 | true | true |
| count_tokens | default | no-UA/body | false | 1→1 | true | true |
| count_tokens | off | native | false | 0→1 | true | true |
| count_tokens | off | no-UA/mimic | true | 0→1 | true | true |
| messages | synthetic_source_fields | native | false | 1→1 | true | false |
| messages | synthetic_source_fields | no-UA/body | false | 1→1 | true | false |
| count_tokens | synthetic_source_fields | native | false | 1→1 | true | true |
| count_tokens | synthetic_source_fields | no-UA/body | false | 1→1 | true | true |

所有场景 mode=transparent、integrity_status=unverified，且
metadata_changed=false 的断言通过。本测试没有 identityService，因此这
只证明该配置下未发生 metadata 改写，不能替代启用账号画像时的单独验收。

GREEN 证明比较断言在明确列出的基线归一化边界内成立，不是请求字节
完全相等：消息级 output_config 缺 beta 时剥离；off 输入会被既有 billing
逻辑从 0 补到 1 块；mimic 仍执行既有 system/header 变换；count_tokens
另有生成字段净化和 beta 协商。body_changed 全为 true，与上述观察一致；
beta_changed 只报告最终 header 是否变化，不证明全部 beta 的语义等价。

四个 synthetic_source_fields 场景只是在官方 default 夹具的内存副本中
加入 cc_gateway、cch、future_source 与未知 metadata 字段；测试实际
断言这些来源值/未知 metadata 保留。它们不是官方 CLI 实际发送过这些
字段的证据，也不验证或生成 opaque 完整性签名。

## 不支持的结论

官方 CLI 两组原始抓包没有 wiggly、additional-protection、cc_gateway 或 cch，
不代表它们全局不存在、
不代表关闭 NONESSENTIAL_TRAFFIC 必然关闭这些字段，更不代表灰度风控已解决。
测试环境是自定义 loopback URL、synthetic token、隔离空配置；它不复现真实账号的
feature flag、上游鉴权、官方域名分支、产品遥测路径或服务器判定。

没有真实账号认证推理，也没有证明上游接受重写后的请求或不会风控。本轮未观察到的
opaque 完整性状态继续记为 unverified；不据此推导重签算法或伪造运行环境的策略。
