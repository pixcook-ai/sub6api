# C：管理员在 Sub6 原生完成 Excel 授权和本次回调交换

追踪：[pixcook-ai/sub6api#5](https://github.com/pixcook-ai/sub6api/issues/5)；父规格：[#2](https://github.com/pixcook-ai/sub6api/issues/2)。

## What to build

管理员在 Sub6 原生授权管理界面创建/选择核心 profile，发起 Excel 或 Codex 独立授权，提交本次官方回调 URL 完成交换，查看可恢复的状态并取消或重试。全流程无需安装或启动 toSub2。

## Acceptance criteria

- [ ] 使用核心能力与安全 profile API 展示可用性、provider、身份、到期与重新授权状态；无密钥/失败时给出可操作的安全提示。
- [ ] 发起授权、官方页面登录、本次回调 URL 提交、完成/取消/过期均有明确状态；授权交换保留真实 origin/PKCE/state/发起者校验。
- [ ] Codex 与 Excel 作为两份独立授权显示，不转换客户端身份；目标缺授权先授权，不删除原授权。
- [ ] 回调 URL 仅用于本次交换，不写浏览器持久存储、日志或公开错误；profile DTO 不暴露秘密。
- [ ] 使用公开 API 接缝验证成功、重复点击、过期、错误发起者和重新授权保留旧 grant；toSub2 停机路径可演示。
- [ ] 真实官方登录由用户完成并单独留存安全证据；缺真实授权时保持对应验收待验。

## Blocked by

- A：官方协议合同。
- B：核心授权 API、状态机与持久化。

## Status

ready-for-agent；界面/合成 API 通过不等于真实 OAuth 已验收。
