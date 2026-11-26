# BPS 功能对照与架构适配

日期：2026-11-26。开发目标为 `sub6api`；`../ranxi2001-sub2api` 仅作只读参考。

## 身份与额度边界

参考仓库主要在普通 OpenAI 账号上增加 BPS 路由开关，保留原 Codex 凭证，并单独保存 Excel grant。当前仓库将 BPS 建为独立平台 `openai_bps`，不能根据相同邮箱或平台按钮推断两个 grant 可互换。

参考 `backend/internal/service/account_usage_service.go` 的 `getOpenAIUsage` 明确要求：配额查询使用原 quota OAuth，不使用独立 BPS grant。因此参考界面的 Codex 点数、重置次数、邀请和隐私操作，不能通过扩大当前平台白名单直接移植。

当前只读 `/backend-api/wham/usage` 返回的是该身份的 ChatGPT/Codex 账号配额，不是 BPS 模型独立额度的保证。本站请求、Token、账号费用和用户费用是本地日志统计。两类数据分别显示；缺少官方周期时显示 `—`，不能显示伪造的 `0%`，也不能用 BPS 本地费用除以该配额百分比估算总额度。

## 参考仓库包含的能力

| 能力 | 参考实现 | 当前适配决定 |
|---|---|---|
| 配额与本地窗口 | Codex 原授权查询、TTL、5H/7D 与日志统计 | 保留当前只读查询和 5H/7D/30D 按真实周期识别；30D 存在也保留短周期本地统计 |
| 令牌续期 | Excel 固定客户端、轮换、锁、CAS、401 后恢复 | 接入现有共享刷新 API 的手动/批量和后台刷新；仅官方 Excel + RT，增加 CAS 和原代理保护；不增加推理 401 重试 |
| 凭证状态 | pending、expired、revoked、认证失败等 | 保留当前按凭证版本匹配的状态；续期只清理由本次凭证产生的诊断，不解除管理员停用或停调度 |
| 点数/重置/邀请/隐私 | 主要依赖保留的原 Codex 授权 | 独立 Excel grant 的端点权限尚未验证，本轮不开放写操作，不把缺失值当零 |
| 连接与工具测试 | nonce 文本、echo 工具、工具结果续接三阶段 | 适配到现有 BPS 生产 adapter，新增工具闭环测试模式；明确三次请求、逐阶段终态和精确结果验证 |
| 错误诊断 | 实际 HTTP 与流内错误区分，失败保留 usage | 补齐 HTTP 200 内白名单错误、取消终态、客户端取消原因；不新增重试或换号 |
| 模型发现 | 同一 access 接口取得 ID、label、efforts，过滤 policy | 已有按凭证/workspace/代理隔离的 ID 发现；补未配置模型过滤。更完整 label/efforts 展示不冒充上游全部强度已支持 |
| 工具转换与修复 | 工具目录、原始 item 恢复、确定性语法修复、受限模型再生成修复 | 当前已有基础工具和确定性语法修复；不新增会改变请求次数、计费或工具执行次数的再生成重试 |
| 压缩 | 原生 compaction window 与图片检查点 | 当前已有真实摘要与 Redis 不透明引用，保持既有会话及压缩契约，不直接替换格式 |
| 图片与附件 | 上传、签名 HTTPS relay、缓存、SSRF/像素/存储/数量上限 | 需要完整上传/relay/资源准入与真实客户端闭环；当前文本平台不能仅放开 input_image 校验 |
| 搜索 | hosted 工具转回 native Codex，或显式省略不支持工具 | 参考也不是 BPS 原生搜索；独立 BPS 没有原 Codex fallback，保持明确能力限制 |
| 403 恢复/转组 | 关闭 BPS 开关、保留 Codex；认领后真实 nonce 探测再 CAS 恢复 | 当前没有同账号原生通道，不复制开关语义；保留模型级拒绝冷却，普通 403 不误判整账号失效 |
| 降智切协议 | 原生 Codex 降智后开 BPS，恢复后撤回自己的改动 | 需要显式双身份关联和规则所有权，不能直接打开独立 BPS 账号的管理员调度开关 |
| 代理预热/代理组 | Mihomo 池、会话租约、资源预算和 worker 生命周期 | 属于独立代理基础设施，保持当前账号指定代理；代理不可用时不得直连 |

## 本轮验收边界

沿用本会话既有的管理 API、生产 adapter 的 HTTP/SSE、账号用量组件和真实本地账户页面验收边界。每项新增行为先复现失败，再验证修复；凭证并发覆盖和 SQL 原子写入另做仓储验证。共享刷新服务和其他平台的既有回归属于受影响范围。

真实页面、离线协议 fixture、模拟 OAuth、真实 OAuth 续期分别记录；fixture 通过不等于上游权限或续期已通过。保留参考仓库原状、当前工作区其他登录/IQ改动和管理员账号设置。无数据库结构变更；不发布远程部署或创建外部 Issue。

## 主要参考路径

- 参考：`backend/internal/service/openai_excel_oauth.go`、`openai_excel_oauth_routing.go`、`account_usage_service.go`、`account_test_service_bps_probe.go`、`openai_excel_bps_recovery.go`、`openai_excel_bps_model_access.go`、`basispoints/upstream_failure.go`、`basispoints/attachments.go`、`basispoints/route.go`。
- 当前：`backend/internal/service/openai_bps_*.go`、`account_usage_service_bps.go`、`oauth_refresh_api.go`、`token_refresh_service.go`、`frontend/src/components/account/AccountUsageCell.vue`、`AccountTestModal.vue`、`frontend/src/components/admin/account/AccountActionMenu.vue`。

这是围绕本次 BPS 对齐的定向比较，不是参考仓库全功能审计。


## 本地最终验收记录

- 运行入口：`http://127.0.0.1:59090/admin/accounts`，ego lite 原任务空间 62。仅恢复用户明确授权的原账号 #15/#17/#18；其余 27 个账号仍软删除。原代理 #48、active 状态及 schedulable 设置保留。
- BPS #17/#18 实际显示 5H、7D、30D；官方短周期缺失显示 `—`，30D 当前返回 0%。普通 OAuth #15 的月度配额实际显示 30D，不再当作 7D。BPS 页面保留本地统计与 ChatGPT/Codex 配额的来源说明。
- BPS #18 通过页面完成一次真实官方 Excel RT 续期，凭证及 RT 指纹变化，到期时间从 `2026-12-06T09:37:59Z` 更新为 `2026-12-06T22:57:51+08:00`；#17 凭证未改变。未输出任何令牌值。
- BPS #18 从实际管理弹窗选择工具闭环模式，以 `gpt-5.6-luna` 连续完成三阶段真实请求：精确 nonce 文本、唯一合法 echo 调用、工具结果续接的精确 nonce；三次均返回完成终态，最终 `test_complete success=true`。HTTP 200 本身不作为通过依据。三次耗时分别为 3456/3326/2575 ms，输入/输出 Token 分别为 22387/28、22749/121、22894/28。
- 实际账号页导入的是 `components/admin/account/AccountTestModal.vue`；真实验收发现另一路径改动不足后，补齐该实际入口及公开交互回归，两个组件均保留相应模式。
- 最终隔离目录由当前提交 `cfca59ff85dbe7b094f652c04281a59ffff7e744` 加本轮任务补丁生成；未混入其他未提交登录流。最终运行二进制 SHA256 为 `382c1497cd5607cbaf2f22482d5eaa5de59bd4cac88ce510a327575031d82228`。仅替换并重启同一 `sub6api-dev` 容器，没有重建容器或重启数据库/Redis。
- 305 个源迁移与现有数据库记录核对：无待执行迁移、无 checksum 冲突。数据库另有 9 条历史记录，未更改。

验证：最终前端相关 6 文件共 110 项通过，i18n 3 项、typecheck 和 production build 通过。后端相关 service/admin/repository/handler 最终定向回归通过，完整 handler/admin/repository unit 包复验也通过；隔离 PostgreSQL 的 3 项 CAS 集成测试明确通过（暂停及 extra 保留、并发重授权优先、outbox 失败回滚）。本轮运行过完整后端 unit；发现的既有 Composite 模型目录 2 项失败已做窄修并定向复验。完整前端为 2789 通过、3 失败，3 项均为未修改的 SubscriptionPlanCard，已在原基线单独复现相同失败，未作为本轮通过项。

证据保存于 `/private/tmp/sub6api-bps-parity-20261126/`：`current-backend-final.log`、`current-handler-repository-unit.log`、`cas-integration.log`、`migration-check-final.safe.json`、`current-frontend-tests.log`、`current-frontend-build.log`、`subscription-baseline-cfca59ff.log`、`runtime-acceptance.safe.json`、`tool-probe-real.txt` 及两张真实页面截图。敏感备份另存私有临时目录，不提交。

未以真实上游验收后台定时触发、批量续期和失败分支；这些范围有本地回归与仓储集成证据。点数/重置/邀请/隐私、图片附件、搜索回退及双授权自动恢复维持上述边界，本轮未宣称与普通 OAuth 完全等价。未远程部署、推送或创建 PR。

## 红框统计行复核（2026-11-27）

按用户“和参考仓库保持一致”的要求，继续复用 `UsageProgressBar.vue` 原有统计行。与参考仓库逐字比较该模板一致：请求数或 Token 大于零才显示请求数、Token、A 账号费用；`user_cost` 非空时显示 U 用户费用。全零或统计缺失时隐藏，不另加零值常显或未知金额占位。

本地 59090 的 BPS #17/#18 用量 API 均返回 5H/7D/30D 的真实 `window_stats` 对象，其中请求数、Token、账号费用和用户费用均为 0，因此当前页面没有统计行符合参考规则。管理页连接/工具测试不等同于通过业务网关产生的计费用量记录。

组件回归覆盖全零隐藏，以及主动刷新到 `4000` 请求、`778500000` Token、账号费用 `1302.97`、用户费用 `1100.25` 后，复用原组件呈现 `4.0K req`、`778.5M`、`A $1302.97`、`U $1100.25`。这些非零值仅为渲染回归输入，并非真实账号消费。

参考的预计总费用仅用于普通 OAuth 7D，以当前窗口费用除以已用比例估算。当前独立 BPS 身份仍遵循前述额度边界，不拿 ChatGPT/Codex 百分比推算 BPS 总费用。本轮未变更生产组件、数据库或本地运行服务。
