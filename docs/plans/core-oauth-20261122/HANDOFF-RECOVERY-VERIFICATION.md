# OAuth 交接恢复本地验证（2026-11-24）

本记录覆盖本轮实现增量，基线为 Sub6 `29f5ebe70`、toSub2 `cde7e44`。对应 Sub6 Issue #7 / `docs/plans/core-oauth-20261122/tickets/05-explicit-handoff.md`；完整交接任务仍未完成。

## 行为

- Sub6 加密 profile 保存 pending 接收预约及原管理员/操作号/来源身份/载荷摘要。状态闭锁授权、刷新、401 恢复和凭据快照；过期不自动解锁。匹配请求可读回原回执，不能延长有效期。
- 内部 `AbandonHandoff` 用同一管理员和操作号持久放弃预约，删除暂存载荷，保留 abandoned 回执。CAS 失败不能解锁，旧操作不能重新激活；原生登录可重新建立新身份代际。尚无接收/放弃 HTTP 入口，不认证或激活传入载荷。
- toSub2 内部协调器先持久写操作号与身份摘要，再本地停写、核对 BPS worker 发布撤销及回执。丢失响应后读回；远端每次重新核对，身份变化闭锁。Codex 撤销适配仍不完整且闭锁。
- 控制台通过协调器串行执行本地准备和取消。取消只接受原 operation_id，协调器已有远端交接记录时拒绝；先持久取消，再持久释放同操作号发布围栏。写入失败或目录同步失败闭锁，原请求重试/重启可恢复。
- 协调日志不保存 AT/RT；不同 profile 更新串行合并，写入结果未知后禁止继续覆盖。取消与撤销互斥；已取消操作保留终态回执，当前不提供第二次交接预约入口。

## 验证

- Sub6：`go test -tags=unit ./internal/handler/admin ./internal/service ./internal/repository ./internal/server/routes -run 'CoreOAuth|CoreRuntimeShell|ManagedDeployment|BPSFiles' -count=1`：330 项通过。使用共享 Go 排他锁，无大型构建并发。
- toSub2：`npm run check:oauth-handoff` 通过，包含 profile 服务、发布围栏、协调器及受鉴权 HTTP 控制台。覆盖错误 ID、重启读回、取消写入/目录同步失败、并发操作、两个 profile 日志保存、远端回执身份变化及 token 不落日志。
- toSub2：`node test/oauth-registry.mjs`、`node test/sub6-publication.mjs`、`node test/managed-sub6-publication.mjs`、console server 语法及两仓 diff 检查通过。
- 回归：`npm run check:oauth-runtime`、`npm run check:admin-entry`、`node test/console-production-smoke.mjs` 均通过；不重新构建前端，生产 smoke 使用已有静态产物，仅验证服务入口。

## 验收边界

本轮没有真实 OAuth、真实 RT 传输/轮换、接收激活、完整 Codex 撤销枚举/读回或真实 BPS 文档/计费验证。全部 `transfer_supported` 为 false，未开放远端撤销 HTTP 操作，未关闭 Issue #7。没有 push、部署、迁移或重启用户实例。

知识图谱传输断连，精确源码回退检查。独立代理多次收到不可读任务或返回无关历史结果，未形成有效独立审查；主代理完成跨仓格式核对和手工差异检查，不宣称独立审查通过。未改其他仓库及原有缓存/验收目录。
