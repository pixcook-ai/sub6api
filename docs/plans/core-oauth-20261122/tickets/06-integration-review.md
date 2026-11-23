# F：验收完整核心路径并完成 Spec / Standards 独立审查

追踪：[pixcook-ai/sub6api#8](https://github.com/pixcook-ai/sub6api/issues/8)；父规格：[#2](https://github.com/pixcook-ai/sub6api/issues/2)。

## What to build

在最终集成版本从 Sub6 管理员授权到官方 Python/JavaScript SDK 使用同一 Sub6 入口完成可追溯验收，并通过两轴独审，明确真实官方授权和计费尚待满足的门槛。

## Acceptance criteria

- [ ] 从管理员登录/授权管理 → 核心 profile → gateway → 受控 BPS → 官方 Python/JavaScript SDK 验证同一入口，toSub2 完全停机仍可运行。
- [ ] 覆盖 JSON/SSE、工具往返、身份/资源隔离、重启、人工停调、并发/迟到 401、双向往返切换及旧会话/在途绑定。
- [ ] 验证核心单一刷新权威、RT 旋转先持久化后发布 AT、未知结果闭锁、invalid_grant 恢复及显式旧所有权交接。
- [ ] 单独记录真实官方 OAuth、RT 轮换、BPS 模型/文件理解和计费证据；缺真实官方授权时明确 blocked/待验，不关闭父规格或相关整票。
- [ ] 针对最终集成 diff 完成 Spec 与 Standards 独立审查，修复后重验受影响范围，记录失败、跳过和未验证范围。
- [ ] 证据与规格/Issues 互相引用且不含凭据、私有日志或真实账号秘密；清晰区分本地可运行、受控集成与真实验收。
- [ ] 保留所有外来 dirty；本轮无 push、PR、部署、真实号池启用或隐式凭据迁移。

## Blocked by

- A：官方协议与独立 provider。
- B：核心授权与单写者刷新。
- C：原生授权管理。
- D：双向新会话切换与 AT 交付。
- E：显式所有权交接与可选管理兼容。

## Status

ready-for-agent；最终代码、真实 OAuth/RT/BPS/计费均须各自证据，局部通过不关闭整项任务。
