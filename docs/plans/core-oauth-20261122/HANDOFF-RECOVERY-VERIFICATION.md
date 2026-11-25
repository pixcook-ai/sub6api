# OAuth 交接本地验证记录（2026-11-24）

对应 Issue #7 / [显式交接规格](tickets/05-explicit-handoff.md)。本文件记录当前集成增量的证据边界，不把此前工作树的测试数或 synthetic 结果当作最终版本的真实迁移证明。协议与恢复步骤见 [PROTECTED-HANDOFF.md](PROTECTED-HANDOFF.md)。

## 当前实现

- Codex 永久账号 publisher fence、全部 controller key 撤销、暂停调度及 scheduler outbox 在独立事务内一起提交，提供管理员 POST / GET 围栏入口。
- BPS 独立 Files service key 可读取只读 publication-fence proof，接收端用同一 Sub6 已保存的账号/config 实时核对。
- Sub6 管理入口提供 invitation、受保护 accept、receipt 读回及 pending abandon；grant 与 accepted 回执以一次 CAS 接收，accepted 不可 abandon。
- toSub2 通过 profile 进程锁、registry freeze 和在途排空建立来源停写，持久保存接收前的 sealed envelope，丢响应后先读回；accepted 后保存回执并清除 envelope，清理旧 vault grant，永久保留来源 fence。
- 当前仅支持有绑定账号的单目标，完整 transfer 在永久撤销前拒绝不满足范围的请求。接收后不自动安装 runtime、调整分组或选择调度。
- 2026-11-25 取消恢复增量：已有 transfer 的 abort 要求 target，在协调器锁内持久 abort_pending、确认并保存 receiver abandoned 回执后才恢复源；无 transfer 保留旧 operation_id-only body。远端撤销开始后禁止源恢复。新增 Node 实测结果见下表。
- 2026-11-25 coordinator 审查修复：无 bound 账号来源在本地 fencing/远端调用前拒绝；对应协调器回归通过。

## 已确认的本地证据

| 范围 | 已确认结果 | 证明边界 |
| --- | --- | --- |
| Codex PostgreSQL 18 围栏 SQL | `python3 internal/repository/account_repo_oauth_publication_fence_postgres.py` 四组验证通过：围栏后撤销全部 key 且阻断注册/install、revision 变化时拒绝暂停、outbox 错误回滚、controller 注册和围栏双序锁竞争。隔离 Postgres 18 容器已删除。 | 数据库事务与竞争顺序，不证明真实 OAuth 或真实账号接收。 |
| Sub6 final Go regression | `go test -tags=unit ./internal/handler/admin ./internal/service ./internal/repository ./internal/server/routes -run 'ManagedOAuth|OAuthController|OAuthPublicationFence|CoreOAuth|CoreRuntimeShell|ManagedDeployment|BPSFiles' -count=1 -timeout=150s`；522 tests passed, 0 failed, 0 skipped。See `.verification/oauth-handoff-20261124/final-go-summary.json`. | Targeted four-package regression against current Sub6 source; not the entire Go repository. |
| BPS final Go regression | `go test ./internal/proxy ./internal/auth ./internal/managedfiles -count=1 -timeout=150s`；862 tests passed, 0 failed, 0 skipped. Also reran the two managed-activation regressions and `TestHTTPClientMetadataSnapshots -count=10`. See `.verification/oauth-handoff-20261124/bps-final-fixed-summary.json`, `managed-regression.jsonl`, and `metadata-concurrency-10.jsonl`. | Local gateway package tests; not a real BPS service or billing acceptance. |
| Node handoff regression | `cd toSub2 && npm run check:oauth-handoff` passed after cancellation recovery was added; includes 10 source/coordinator restart, lost response and concurrency recovery scenarios plus HTTP console smoke and production crypto tests. Final rerun log: `.verification/oauth-handoff-20261124/node-handoff-final.log`. | Synthetic grants and receiver; no real token or publisher migration. |
| Node to Go protected handoff HTTP integration | `go test -tags=unit ./internal/handler/admin -run '^TestCoreOAuthTransferProductionClientHTTPIntegration$' -count=1 -timeout=90s -v` passed for `{codex,bps_excel}` after adding the production source `ProfileService`. It exercises source vault/registry prepare, invitation, permanent remote fence, protected accept, lost accept and receipt responses, client/source restart, accepted readback, source vault deletion, and rejection of source refresh/reprepare; a fresh receiver service independently refreshes the imported grant. Final run output is in `.verification/oauth-handoff-20261124/http-transfer-source-final.log`. | Synthetic credentials, AES-GCM in-memory source vault and encrypted in-memory receiver repository, loopback proof backend. This does not prove physical DB durability or real OAuth/BPS migration. |
| Current source official SDK integration | `sdk-e2e-current.json`: 61/61 steps passed with Python OpenAI SDK 3.55.1 and JavaScript SDK 7.60.0 through Sub6/admin/router and real managed BPS worker. Covers Excel/Codex switching, JSON/SSE/tools, Files and `input_file`, isolation, 401 refresh, restart, same-identity reauthorization, and old sessions. Current binary hashes/source manifests are in `build-sub6-result.json`, `build-bps-worker-result.json`, and `final-source-sha256.json`. | Isolated local DB/services and simulated OAuth, BPS, and Codex. Does not include ownership handoff or stopping toSub2, and does not prove real upstream, document understanding, billing or deployment. |
| Source SHA and build | Sub6 fixture server built with `go test -tags=coreoauthfixture -c -o <evidence>/bin/server-final.test ./cmd/server` from current source; aggregate source SHA-256 `93dcb345ea56d4dab55cc35c80166f67d9a3756fa5e8cd9a9c4cf68b5c9dfe2a`, binary SHA-256 `496845d9c64152c91428a1459ce51e0182d5f02f980c3a08bebcff33bf33f7f9`. BPS fixture worker built from current source; aggregate SHA-256 `47d5251ae689cf19b4d49b4322083fecd4ef89bc12ae77dd247a1f6dc7d3c075`, binary SHA-256 `0c066218c8bae71ca5ea9b3351ea37fd4f82bf8dc88a8f03e29225804b0827aa`. | Aggregate hashes cover Go source files, go.mod/go.sum (and Sub6 model-pricing JSON) in each respective repository. Source hash was unchanged during each build. |

Sub6 与 BPS SDK runner 使用当前工作树构建的源码二进制，构建前后源码 SHA-256 一致；不沿用历史 SDK 报告作为当前证据。此前 Node transfer client 结果不覆盖新增取消恢复补丁，本表所列 Node handoff regression 是该补丁后的最终结果。SDK runner 于 2026-11-25T01:04:17Z 启动、01:04:37Z 结束；run_id 为 `1b2d72acf8529b30a517fe8e`。隔离 PostgreSQL/Redis、Sub6 与 worker 进程均由 runner 验证停止/删除，只清理 runner 自有临时根目录。

## 最终集成待补证据

- 尚无物理 PostgreSQL-backed protected-transfer end-to-end；跨语言 HTTP 交接测试使用 encrypted in-memory repository。Codex publisher fence 的真实 PostgreSQL SQL concurrency tests 独立通过。
- 尚无覆盖 source-toSub2 transfer、永久撤销、停掉来源进程、Sub6 独立 refresh/publish 与后续 SDK 调用的单一端到端用例。当前 SDK E2E覆盖核心运行时行为，HTTP integration 覆盖 transfer 接收和重启刷新，但二者尚未串成一次工作流。
- 已有独立双轴只读审查未发现确定 P0/P1/P2；Spec 复核另发现并修复“无绑定可直接 reconcile”的 P2，新增回归通过。审查不覆盖 profile-service/protection 模块的完整源码质量，也不代替持续维护。

## 未验证范围

没有运行真实 OAuth、真实凭据迁移、真实 RT 传输/轮换、真实 BPS 文档/计费、真实管理员恢复或真实调度切换。未知外部 vault / publisher 的全局清单完整性不在现有 proof 的证明范围内。不得用本地通过关闭 Issue #7 的真实验收门。

本轮文档更新未执行 push、部署、迁移、用户实例重启或真实凭据读取。知识图谱传输不可用，合同核对使用精确源码回退，不宣称图谱覆盖或真实验收通过。
