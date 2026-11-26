# Sub6API CI/CD

目标为现有独立实例 `https://sub6api.pixcook.ai`，服务器目录 `/opt/sub6api`。现有站点、数据库、Redis、账号授权菜单和数据卷保持独立。

## 流程

1. PR、`codex/**` 和 main 推送运行 CI：部署脚本、Go 单元与集成测试、前端 lint/typecheck/关键测试、Go lint。
2. main 或手工运行 `Deploy Sub6API` 先执行相同 CI，及生产依赖安全扫描，再构建 Linux AMD64 嵌入前端二进制、资源与 SHA256 清单。部署仅允许 main。
3. 使用 `sub6api-production` environment 的 SSH 凭据上传本次运行的固定 SHA artifact。服务器验证校验和与已部署迁移、运行时基线，成功备份后只重建应用层。
4. 等待容器健康、验证实际 PID1 二进制 SHA256 与公网 HTTPS `/health`，保存部署证据。失败恢复旧 compose/应用镜像。

## 一次性服务器配置

- Environment secrets：`SUB6API_DEPLOY_SSH_KEY`、`SUB6API_DEPLOY_KNOWN_HOSTS`。主机指纹必须经已信任连接核对，不能在 CI 中临时无条件接受扫描值。
- Environment variables：`SUB6API_DEPLOY_HOST`、`SUB6API_DEPLOY_USER`。当前使用独立密钥和 forced-command receiver，只接受 `upload <sha>` / `deploy <sha>`，拒绝 shell、转发和非预期压缩包；部署脚本需要 Docker、备份 service 与 `/opt/sub6api` 写权限，不能仅授予无特权 shell 后声称部署可用。
- 初次记录 `/opt/sub6api/MIGRATIONS_SHA256`：在核实服务器实际部署提交后，对该提交执行 `git ls-tree -r <commit> backend/migrations | sha256sum | cut -d' ' -f1`。与 artifact 不同则停止自动部署，先处理迁移与恢复计划。
- 使用现有 `compose.json`、`sub6api-backup.service` 及 `sub6api-app-1`。脚本不会新建数据库或重置安装状态。

## 回滚边界

每个 release 保留 `compose.before.json`（0600）、原镜像 ID、artifact、校验和；成功后记录 `logs/deployment-<sha>.json`。回滚仅替换 app，数据库未发生 schema 变化是自动回滚的前提。跨 schema 发布被明确拒绝，不宣称二进制回滚可逆转迁移。

使用旧镜像作为固定运行时基底、仅覆盖已验证二进制和 resources，避免每次远端重新编译及依赖漂移。Dockerfile 与运行时依赖有变化时，需单独更新基底并验证，不能仅依靠此部署方式。

`Main release` 不再随 main 推送直接发布；显式 tag/manual 发布先执行 CI。它仍用于多平台公开发行，依赖原有 Docker Hub 凭据。生产部署流水线不依赖 Docker Hub。

## 验收

健康与二进制校验证明应用版本就绪；仍需管理员登录、静态资源、未认证拒绝和现有授权菜单回归。真实模型请求须有已配置账号与明确验收输入；不得将 `/health` 通过表述为模型推理通过。

Go lint 以基座提交 `a3eaf63afc672ffedf7844e6cfc5cb6750d52a3e` 为既有问题基线，阻断新增问题；既有 84 项问题没有在本次发布中全部修复。全量单元、集成测试与安全扫描仍阻断发布。

`scripts/release/ssh-receiver.py` 安装至 `/usr/local/sbin/sub6api-ci-receiver`，对应 authorized_keys 行带 `restrict,command="/usr/local/sbin/sub6api-ci-receiver"`。此部署密钥可以发布应用代码，按生产部署凭据保护。

运行时基线由 `bash scripts/release/runtime-baseline.sh <verified-commit>` 生成，保存为 `/opt/sub6api/RUNTIME_SHA256`。覆盖运行时 Dockerfile 阶段、基础镜像版本和 entrypoint；编译器修补更新不要求更换运行时。

## 发布清单：批量邮箱----密码----2FA 授权与自动重授权

目标：`https://sub6api.pixcook.ai`，服务器目录 `/opt/sub6api`。本次发布含迁移 251 和运行时变更，不能走普通自动部署。

### 为什么不能直接部署

1. `deploy-sub6api.sh` 比对 `MIGRATIONS_SHA256`，新增迁移 251 会使其不一致，脚本拒绝执行。
2. 脚本同时比对 `RUNTIME_SHA256`，两份 Dockerfile 已改动（内置 Node、jsdom、Python curl_cffi），同样会被拒绝。
3. 脚本以服务器现有镜像为基底，只覆盖 `sub2api` 与 `resources`。新镜像里不会有 Node、curl_cffi、jsdom 和 `/app/tools/tosub2-login`，批量登录会返回 `BATCH_LOGIN_RUNTIME_UNAVAILABLE`，自动重授权会失败。
4. 部署只允许 main，推送 main 会直接触发生产部署。

### 本地计算的基线（仅供对照）

| 项目 | 变更前（HEAD~1） | 变更后（f92150779） |
|---|---|---|
| RUNTIME_SHA256 | 36ee679dbf84f41062671400d95048b88c3083ca46f9da03e91bb253607dc412 | fa47f9511f24bafd6d1af59f8c012248f71d7e80ff1ec2906d6205a684990806 |
| MIGRATIONS_SHA256 | 823246b620db3e3be2bb0b5429dcf6e1566d77317edad67409289868ecb40760 | 1c9c7ca17251f6c49453c13e5b11e60eb4202f3e0e91cbf4e3f3cc27b5a9d5e4 |

变更前一列只是本仓库上一个提交的值。服务器当前基线必须读取 `/opt/sub6api/MIGRATIONS_SHA256`、`/opt/sub6api/RUNTIME_SHA256` 与 `SOURCE_COMMIT`，并对该提交按 `docs/SUB6API_CICD.md` 的方法复算，确认一致后再继续。若服务器的值与本表不同，说明线上与本仓库历史存在差异，先停止并核实。

### 发布步骤（由有服务器权限的人执行）

1. 核实服务器实际部署提交与上述基线，保存 `compose.json` 与当前镜像 ID。
2. 手动触发 `sub6api-backup.service`，确认备份成功。迁移 251 只新增 `openai_login_credentials` 表，不改既有表，但仍应先备份。
3. 在构建机用主 Dockerfile 构建新的运行时基底镜像（含 `tosub2-runtime` 阶段产物），验证：`node --version`、`/opt/tosub2-venv/bin/python -c 'import curl_cffi'`、`node /app/tools/tosub2-login/run-login.mjs` 缺凭据时返回 `missing_credentials`。注意本机验证的是 aarch64，生产为 amd64，需在 amd64 上重新验证 curl_cffi 的 musllinux wheel。
4. 将新基底镜像加载到服务器，更新 `/opt/sub6api/RUNTIME_SHA256` 为 `fa47f951…`，更新 `/opt/sub6api/MIGRATIONS_SHA256` 为 `1c9c7ca1…`，并调整 `deploy-sub6api.sh` 使其以新基底镜像（而非旧镜像）构建应用层。
5. 确认 `totp.encryption_key`（32 字节，64 位十六进制）已配置。缺失时，保存凭据与自动重授权会失败。
6. 评审后将分支合并到 main，触发 `Deploy Sub6API`。迁移 251 在应用启动时自动执行。


运行时基线现在额外覆盖 `tools/tosub2-login` 的文件清单（`git ls-tree`），登录运行时变化会要求重新核对基底镜像。上表“变更后”的 RUNTIME_SHA256 以提交本次脚本改动之后的 HEAD 为准；最终值以 `bash scripts/release/runtime-baseline.sh <verified-commit>` 对实际发布提交的计算结果为准。

### 运行时基底镜像（RUNTIME_BASE_IMAGE）

`deploy-sub6api.sh` 现在支持服务器上的 `/opt/sub6api/RUNTIME_BASE_IMAGE`：文件内容为已加载并验证过的基底镜像引用，格式必须是 `sub6api:runtime-<字符>`。存在该文件时，脚本直接以它为基底构建应用层，并拒绝不存在或格式非法的引用；不存在时保持原行为（以当前运行镜像为基底）。首次发布步骤：在 amd64 上构建含 Node、jsdom、curl_cffi 和 `/app/tools/tosub2-login` 的基底镜像，`docker load` 到服务器并验证，写入 `RUNTIME_BASE_IMAGE`，再更新两个基线。

### 部署后验收

- `/health` 返回 ok，管理员登录正常，既有账号授权菜单无回归。
- 添加账号 → OpenAI → 授权方式末尾出现「邮箱----密码----2FA 批量授权」。
- 用测试账号做一次真实批量授权，分别验证 BPS 与 Codex 目标，以及勾选保存凭据后账号编辑页的提示。
- 在测试账号上使 refresh 失效，观察自动重授权成功，且失败时按 15 分钟起翻倍冷却、连续 3 次后停止。

### 回滚

- 应用层：恢复 `compose.before.json` 与原镜像。
- 迁移 251 新增独立表，回滚应用后表保留不影响旧版本；如需清理，仅允许在确认无依赖后手动删除该表。

### 未验证

真实 OpenAI 登录与自动重授权、amd64 镜像构建、`deploy/Dockerfile` 的完整构建、登录包装脚本 9 分钟超时分支。
