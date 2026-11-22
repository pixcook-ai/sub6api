# Claude 请求兼容配置

`SUB2API_CLAUDE_COMPATIBILITY_PROFILE` 为完整 JSON 对象，在进程启动时读取一次。未设置时保留现有默认值；设置后缺字段、未知字段、空值、控制字符或超长值会阻止启动，避免只应用半套配置。它只描述出站 SDK 请求头，不表示服务器真实运行环境，也不提供账号安全保证。

字段为 `lang`、`package_version`、`os`、`arch`、`runtime`、`runtime_version`。值应来自已核实的目标客户端版本与运行环境，不能从主机操作系统推断。以下是**原有默认值的配置格式示例**，不是新推荐的客户端配置：

```json
{
  "lang": "js",
  "package_version": "0.112.1",
  "os": "Windows",
  "arch": "x64",
  "runtime": "node",
  "runtime_version": "v22.19.0"
}
```

在部署环境中把 JSON 内容设置为上述环境变量，重启应用进程。新账号的默认身份与 OAuth 模拟路径共用该配置。真实 Claude Code 请求仍保留已有的按账号缓存及客户端版本升级逻辑；本配置不会自动改写已有账号，也不会强制覆盖真实客户端提交的身份。

CLI 版本继续使用现有后台设置：管理员指定值 → 自动同步值 → `SUB2API_CLAUDE_CLI_VERSION` → 内置版本。SDK profile 不改写版本、billing 计算、设备 ID、账号 UUID 或会话 ID。

## 显式迁移已有账号

工具仅处理明确指定的 `fingerprint:<账号ID>` Redis 缓存。默认预览，只输出账号 ID 与需变更字段名称，不输出身份值或 Redis 地址。

1. 将已核实的完整配置保存为绝对路径的 JSON 文件，并用安全的环境配置方式设置 `SUB2API_PROFILE_MIGRATION_REDIS_URL`。Redis URL 可包含认证信息，不要放进命令行或共享日志。
2. 暂停目标账号的新流量并等待在途请求结束。在 `backend/` 下预览：

   ```bash
   rtk go run ./cmd/claude-profile-migrate \
     --profile-file /absolute/path/profile.json --account-ids 101,102
   ```

3. 审核结果后，用相同参数增加 `--apply`。应用进程须使用相同 profile 配置；迁移成功后恢复目标账号流量。

工具只改六个 `Stainless*` 字段，保留设备 `ClientID`、`UserAgent`、`UpdatedAt`、未知 JSON 字段与缓存剩余 TTL。缓存缺失、身份字段损坏或无有效期时拒绝迁移，不创建新身份。写入使用原值比较和原子 `SET KEEPTTL`；并发变化时拒绝覆盖。多账号逐个应用，部分失败不会回滚已成功账号，应根据输出处理失败项。连接错误可能发生在 Redis 已接受写入之后，须先再次预览确认状态。

暂停流量是必要边界：CAS 只保护迁移写入前的并发变化，不能阻止已在途应用请求稍后写回旧缓存。迁移改变的是缓存，并不锁定未来真实客户端携带更高 UA 版本时的正常升级。

## TLS 与验证边界

TLS 模板仍通过现有账号编辑页独立配置。本次不自动选择或随机生成 TLS 特征，也不根据 SDK 头中的 Node 版本推断 TLS 参数。需要在受控本地接收端验证 ClientHello 及实际 HTTP 请求；现有内置 TLS 模板注释为 Node.js 24.x，而原有 HTTP profile 声明 Node v22.19.0，二者尚未由本次修改证明匹配。

离线测试覆盖配置校验、默认请求头同源、缓存迁移预览、TTL/身份/未知字段保留及并发拒绝。未执行生产迁移、部署或真实上游请求；不生成遥测。
