# D：同一入口双向切换新会话并向 worker 交付核心 AT

追踪：[pixcook-ai/sub6api#6](https://github.com/pixcook-ai/sub6api/issues/6)；父规格：[#2](https://github.com/pixcook-ai/sub6api/issues/2)。

## What to build

管理员从核心授权选择 Codex 或 Excel 为新会话提供服务，保持客户端 base_url/API key 和既有会话不变。worker 只消费核心发布的 AT 快照，所有刷新仍由 Sub6 完成。

## Acceptance criteria

- [ ] 复用 managed deployment/session 的路由与不可变绑定；仅新会话采用新 provider，旧 session、在途请求、response/file 资源保留原身份/工作区/提供方绑定。
- [ ] 原生操作要求显式模型、至少一种能力、schedule_new_sessions=true；GET 缺上下文返回 context_required 和稳定原因，POST 严格验证对象。
- [ ] 切换前验证 binding 来源、主体、工作区、授权、人工停调、组、模型、能力和 scheduler 策略；失败不写路由，不自动跨 provider 或恢复停调。
- [ ] route revision CAS 处理竞争；重复选择当前 provider 在 CAS/预检通过后为 revision 不变的幂等操作。
- [ ] runtime 使用独立核心所有权合同，拒绝外部 managed marker、含 RT 账号、provider/platform 不符和损坏身份/版本/到期；按一致快照安装 AT。
- [ ] worker 不持有 RT/ID token、不自主刷新；核心未知刷新状态不发布 AT，不重放副作用或已输出请求。
- [ ] JSON/SSE、工具往返、双向往返切换和身份/资源隔离有公开 API 与受控 BPS 证据；真实 BPS 能力/计费另列待验。

## Blocked by

- B：核心授权与一致凭据快照。

## Status

ready-for-agent；代码与受控验证待主代理集成，不以健康检查替代用户路径。
