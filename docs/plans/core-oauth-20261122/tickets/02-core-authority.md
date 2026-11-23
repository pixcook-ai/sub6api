# B：通过核心 API 独立保存授权并以单写者完成刷新恢复

追踪：[pixcook-ai/sub6api#4](https://github.com/pixcook-ai/sub6api/issues/4)；父规格：[#2](https://github.com/pixcook-ai/sub6api/issues/2)。

## What to build

管理员从 Sub6 核心 API 创建和管理两类 profile，完成授权后在重启、到期或真实上游 401 时由唯一核心权威恢复凭据；停止 toSub2 后依然可运行。

## Acceptance criteria

- [ ] 既有管理员认证/RBAC 下提供能力、profile 和发起/完成/取消/刷新 API；同一发起者校验有效，DTO 无秘密且保持既有成功 envelope。
- [ ] 使用稳定密钥加密持久化授权、attempt 元数据和刷新操作标记；复用私有 settings/CAS，不新增迁移。无稳定密钥明确 unavailable 且既有服务可启动。
- [ ] 重启恢复已保存授权，未完成的进程内 PKCE 尝试明确过期；重新授权期间保留原可用 grant。
- [ ] 跨实例先 CAS 取得刷新权；旋转 RT 先持久化后发布 AT。存储失败和已经发出但结果未知时闭锁，不盲重试旧 RT；invalid_grant 明确要求重新授权。
- [ ] 只有 upstream_oauth 401 按 identity epoch 与实际失败 revision 恢复；并发/迟到 401 不重复刷新，入口 401、403、429 不触发刷新。
- [ ] CredentialSnapshot 交付身份、revision 与 AT 的一致快照，无 RT/ID token；人工停调和有副作用/已输出请求边界保持。
- [ ] 公开 API、持久化重启、多实例竞争与错误路径有 RED → GREEN 证据；真实 OAuth/RT 轮换单独标为待验，不用 mock 关闭门槛。

## Blocked by

- A：官方 Excel 协议与独立 provider 合同。

## Status

ready-for-agent；按 A 合同接线，核心 API 与真实授权验收分开记录。
