# A：核对 Excel 官方协议并建立独立 BPS provider 行为

追踪：[pixcook-ai/sub6api#3](https://github.com/pixcook-ai/sub6api/issues/3)；父规格：[#2](https://github.com/pixcook-ai/sub6api/issues/2)。

## What to build

在既有公开模块接缝中验证 Excel 官方授权合同，并让 BPS provider 以独立身份发起、校验和交换本次授权；既有 Codex 授权继续可用，为核心管理提供可验证的协议基础。

## Acceptance criteria

- [ ] 明确 Excel 与 Codex 独立 client_id、精确官方 redirect URI、授权/交换/刷新合同和身份提取规则；未确认的官方行为显式保留合同门。
- [ ] 首版支持提交本次官方回调 URL 完成 PKCE 事务，无需提供 OAuth 文件；不将 Excel 强改为 localhost，不通过 client_id 转换 token。
- [ ] provider 拒绝错误 origin、state、PKCE、过期/重放事务和身份不符；错误输出为固定安全信息，无 code/token/verifier/cookie 泄漏。
- [ ] 公开模块行为 RED → GREEN，独立 fixture 证明协议与 Codex 回归边界；fixture 不表述为真实官方 OAuth 已完成。
- [ ] 交付可供核心授权服务消费的合同、限制和复现证据；真实官方回交能力待实际授权验证。

## Blocked by

无，可立即开始。

## Status

ready-for-agent；六阶段已批准，代码与真实 OAuth 仍待相应证据。不得仅凭合成测试关闭真实验收门。
