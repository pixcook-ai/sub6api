# Sub6API CI/CD

目标为现有独立实例 `https://sub6api.pixcook.ai`，服务器目录 `/opt/sub6api`。现有站点、数据库、Redis、账号授权菜单和数据卷保持独立。

## 流程

1. PR、`codex/**` 和 main 推送运行 CI：部署脚本、Go 单元与集成测试、前端 lint/typecheck/关键测试、Go lint。
2. main 或手工运行 `Deploy Sub6API` 先执行相同 CI，及生产依赖安全扫描，再构建 Linux AMD64 嵌入前端二进制、资源与 SHA256 清单。部署仅允许 main。
3. 使用 `sub6api-production` environment 的 SSH 凭据上传本次运行的固定 SHA artifact。服务器验证校验和与已部署迁移、运行时基线，成功备份后只重建应用层。
4. 等待容器健康、验证实际 PID1 二进制 SHA256 与公网 HTTPS `/health`，保存部署证据。失败恢复旧 compose/应用镜像。

## 一次性服务器配置

- Environment secrets：`SUB6API_DEPLOY_SSH_KEY`、`SUB6API_DEPLOY_KNOWN_HOSTS`。主机指纹必须经已信任连接核对，不能在 CI 中临时无条件接受扫描值。
- Environment variables：`SUB6API_DEPLOY_HOST`、`SUB6API_DEPLOY_USER`。建议使用专用受限部署身份；部署脚本需要 Docker、备份 service 与 `/opt/sub6api` 写权限，不能仅授予无特权 shell 后声称部署可用。
- 初次记录 `/opt/sub6api/MIGRATIONS_SHA256`：在核实服务器实际部署提交后，对该提交执行 `git ls-tree -r <commit> backend/migrations | sha256sum | cut -d' ' -f1`。与 artifact 不同则停止自动部署，先处理迁移与恢复计划。
- 使用现有 `compose.json`、`sub6api-backup.service` 及 `sub6api-app-1`。脚本不会新建数据库或重置安装状态。

## 回滚边界

每个 release 保留 `compose.before.json`（0600）、原镜像 ID、artifact、校验和；成功后记录 `logs/deployment-<sha>.json`。回滚仅替换 app，数据库未发生 schema 变化是自动回滚的前提。跨 schema 发布被明确拒绝，不宣称二进制回滚可逆转迁移。

使用旧镜像作为固定运行时基底、仅覆盖已验证二进制和 resources，避免每次远端重新编译及依赖漂移。Dockerfile 与运行时依赖有变化时，需单独更新基底并验证，不能仅依靠此部署方式。

`Main release` 不再随 main 推送直接发布；显式 tag/manual 发布先执行 CI。它仍用于多平台公开发行，依赖原有 Docker Hub 凭据。生产部署流水线不依赖 Docker Hub。

## 验收

健康与二进制校验证明应用版本就绪；仍需管理员登录、静态资源、未认证拒绝和现有授权菜单回归。真实模型请求须有已配置账号与明确验收输入；不得将 `/health` 通过表述为模型推理通过。

Go lint 以基座提交 `a3eaf63afc672ffedf7844e6cfc5cb6750d52a3e` 为既有问题基线，阻断新增问题；既有 84 项问题没有在本次发布中全部修复。全量单元、集成测试与安全扫描仍阻断发布。

运行时基线由 `bash scripts/release/runtime-baseline.sh <verified-commit>` 生成，保存为 `/opt/sub6api/RUNTIME_SHA256`。覆盖运行时 Dockerfile 阶段、基础镜像版本和 entrypoint；编译器修补更新不要求更换运行时。
