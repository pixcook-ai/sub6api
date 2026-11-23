# Sub6 完整核心双 OAuth：规格与任务

日期：2026-11-22（Asia/Shanghai）。六阶段规格、切片与公开 API 测试接缝已获批准；文档与追踪发布完成不表示产品实现或真实 OAuth 已验收。

- [规格](spec.md)；[GitHub 父规格 #2](https://github.com/pixcook-ai/sub6api/issues/2)。
- [核心授权 API 合同](API-CONTRACT.md) 与 [新会话切换 API 合同](SWITCH-API-CONTRACT.md)。这两份合同是本次集成合同的安全快照；实施中变更须由主代理统一核对。
- [GitHub 回读核验](tracking-verification.json)：六个原生 sub-issues、11 条原生 blocking edges，全部 open / ready-for-agent。

| 顺序 | GitHub 任务 | Blocked by | 本地草案 |
| --- | --- | --- | --- |
| 01 | [A：核对 Excel 官方协议并建立独立 BPS provider 行为](https://github.com/pixcook-ai/sub6api/issues/3) | 无 | [本地任务](tickets/01-official-protocol.md) |
| 02 | [B：通过核心 API 独立保存授权并以单写者完成刷新恢复](https://github.com/pixcook-ai/sub6api/issues/4) | #3 | [本地任务](tickets/02-core-authority.md) |
| 03 | [C：管理员在 Sub6 原生完成 Excel 授权和本次回调交换](https://github.com/pixcook-ai/sub6api/issues/5) | #3、#4 | [本地任务](tickets/03-native-authorization.md) |
| 04 | [D：同一入口双向切换新会话并向 worker 交付核心 AT](https://github.com/pixcook-ai/sub6api/issues/6) | #4 | [本地任务](tickets/04-runtime-switch.md) |
| 05 | [E：显式交接旧受管所有权并保持 toSub2 可选兼容](https://github.com/pixcook-ai/sub6api/issues/7) | #4、#6 | [本地任务](tickets/05-explicit-handoff.md) |
| 06 | [F：验收完整核心路径并完成 Spec / Standards 独立审查](https://github.com/pixcook-ai/sub6api/issues/8) | #3、#4、#5、#6、#7 | [本地任务](tickets/06-integration-review.md) |

## 状态与验收边界

所有 Issue 当前记录实施范围，验收项保持未勾选；代码实施、受控集成、真实官方 OAuth/RT 旋转、BPS 模型/文件理解和计费分别需要对应证据。缺少用户官方授权时记录 blocked/待验，父规格与相关整票保持打开。

Sub6 是完整核心，toSub2 可选；旧 toSub2 管理员嵌入任务 #1 与本任务不等价，未修改也未关闭。旧受管凭据交接必须显式围栏并维持单写者。

本目录仅包含已批准规格、合同快照、任务与追踪证据。文档任务未运行 Go、构建或安装，未改产品代码，未执行 commit、push、PR、部署、真实号池启用或真实凭据迁移。

## Git 交付注意

仓库既有忽略规则 docs/* 会忽略本目录；本次保留该配置并未暂存。文件已实存，GitHub 规格和任务已发布。后续获准提交时，仅对本目录显式 force-add，或另行批准精确忽略例外。
