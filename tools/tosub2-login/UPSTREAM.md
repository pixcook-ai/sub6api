# tosub2-login 来源记录

本目录是从 toSub2 精简引入的纯协议登录运行时，供 sub6api 批量「邮箱----密码----2FA」授权使用。

- 来源仓库：poxiao33/toSub2（本地副本 `../toSub2`）
- 来源提交：`dae89f4fbe2918f092d368e0f49fc6ccf7cf6c38`
- 许可证：MIT（见 `LICENSE`）
- 引入方式：`git show <commit>:<path>`；仅 src/protocol-login.mjs 在失败分支增加了 [reason] 分类输出（tls_error、proxy_error、risk_control），其余未修改

## 文件清单

- `src/protocol-login.mjs`、`src/tls-transport.mjs`、`src/bps-oauth.mjs`、`src/codex-oauth.mjs`
- `src/tls_transport.py`
- `src/cloudflare-ctf/`：`cf_runtime.cjs`、`cloudflare_solver.py`、`sentinel_client.py`、`sentinel_dynamic.py`、`sentinel_runtime.cjs`、`turnstile_runtime.cjs`
- `requirements.txt`：`curl_cffi==0.15.0`
- `package.json` / `package-lock.json`：仅 `jsdom@26.1.0`（本仓库新增）

## 不引入的部分

控制台、授权档案、受管部署、SMS/邮箱验证码服务、`sentinel-sdk.js`（仅控制台使用，登录路径无引用）、`__pycache__`。

## 不固定到旧提交的原因

参考仓库运行包固定的 `8548397e…` 不含 `bps-oauth.mjs` 与 `codex-oauth.mjs`，缺少 `--oauth-provider bps_excel --token-ipc` 接口，因此基于上述较新提交。

## 升级

对照来源仓库该提交之后的 `src/` 变更，逐文件 `git show` 覆盖，并更新本文件的提交号。
