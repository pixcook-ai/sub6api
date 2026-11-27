# Claude 请求兼容配置

`SUB2API_CLAUDE_COMPATIBILITY_PROFILE` 为完整 JSON 对象，在进程启动时读取一次。未设置时保留现有默认值；设置后缺少必填字段、存在未知字段、空值、控制字符或超长值会阻止启动，避免只应用半套配置。它描述出站 SDK 请求头及可选的 CLI 执行入口，不表示服务器真实运行环境，也不提供账号安全保证。

必填字段为 `lang`、`package_version`、`os`、`arch`、`runtime`、`runtime_version`。可选字段 `entrypoint` 仅接受 `cli` 或 `sdk-cli`，省略时保持原有 `cli` 行为。值应来自已核实的目标客户端版本与运行环境，不能从主机操作系统推断。以下是**原有默认值的配置格式示例**，不是新推荐的客户端配置：

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

## Linux 无交互模式参考

以下网关配置取自官方 Linux amd64 CLI `2.1.330` 在 `-p` 合成 OAuth 回环中发送的请求头。显式选择 `sdk-cli` 会改变默认 UA 的执行入口后缀；普通客户端的 OAuth 模拟分支在该模式下不额外添加 `x-stainless-helper-method: stream`。既有六字段配置保持兼容。真实客户端透传的头及已有账号缓存继续遵循原有规则。

```json
{
  "lang": "js",
  "package_version": "0.128.0",
  "os": "Linux",
  "arch": "x64",
  "runtime": "node",
  "runtime_version": "v26.12.1",
  "entrypoint": "sdk-cli"
}
```

对照时还需通过上述版本配置选择 `2.1.330`；SDK 配置不会绕过管理端版本优先级。这是固定版本和执行模式下的公开请求配置，不会随 CLI 发布自动更新。`sdk-cli` 模拟路径声明 `gzip, deflate, br, zstd`，四种响应编码都经过实际流式转发验证。该选项不表示完整请求体或真实上游识别结果已与 CLI 相同。参见[协议验收](claude-cli-protocol-acceptance.md)。

## 按模型和接口选择 beta

可选的 `model_betas` 和 `count_tokens_model_betas` 分别配置 Messages 与计数接口的完整 beta 基线，格式为模型 ID 到有序 token 数组的对象。两个对象独立，省略或空对象均沿用原有策略。只在 OAuth 模拟分支、最终上游模型匹配时启用；真实 CLI 透传和 API Key 路径不受影响。

[Linux 2.1.330 完整参考配置](examples/claude-linux-2.1.330-profile.json) 包含上述 SDK 字段及 Sonnet 4.5 两个接口的独立实测集合。计数样本来自官方 Linux CLI 的非 bare 合成 OAuth `/context`，没有将 API Key 样本当作 OAuth。其他模型未配置时继续使用原策略；应先取得对应模型、版本及功能条件的证据再增加配置，不能将这个单模型样本推广到所有模型。

每个对象最多 64 个模型，每个模型 2–32 个唯一 beta，必须包含 `claude-code-20250219` 和 `oauth-2025-04-20`。模型 ID 最长 256 字节，token 最长 128 字节，仅接受小写 ASCII 字母、数字和 `-_.`。已知短模型 ID 会归一化；不同键归一后冲突会拒绝。新增的两个 beta 字段及其内部模型键均禁止重复，不会静默合并或覆盖。显式 `null`、错误类型和空数组会拒绝。

管理员的 block/filter 规则仍生效：生成的配置 beta 在发送前检查，命中 block 不会请求上游，计数接口返回 `400 invalid_request_error`。过滤后的集合继续决定请求体字段是否保留。例如不选择 `thinking-binding-controls` 或 `mid-conversation-output-config` 时，既有净化逻辑会删除 `thinking.block_binding` 或消息内 `output_config`。Messages 保留显式请求的 structured-outputs 兼容 token，计数接口继续合并入站 beta；选定配置不会默认追加其他 token。高级请求需要对应能力配置，不能只按请求头外观选择集合。

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

TLS 模板仍通过现有账号编辑页独立配置。[Linux CLI 2.1.330 TLS 参考模板](examples/claude-linux-2.1.330-tls.json) 是 `POST /api/v1/admin/tls-fingerprint-profiles` 的创建请求格式。它须显式导入后由目标账号选择，不会替换内置模板或自动更新生产配置。SDK 头中的 Node 版本不能用于推断 TLS 参数。

模板在 macOS 和 Linux 上均通过真实创建处理器、内存测试仓库、测试账号模板选择和实际 HTTP 传输验证。与官方 CLI 使用同一个 TLS 接收端时，所采公开 ClientHello 字段逐项一致，包括套件及扩展顺序、分组、签名算法、ALPN、版本、point formats、PSK modes、key share 组及长度，且成功读取 SSE。证据不包含随机数、会话 ID 或密钥字节，不证明会话恢复、所有代理路径或真实上游识别结果相同。[验收记录](claude-cli-protocol-acceptance.md)

离线测试覆盖配置校验、默认请求头同源、缓存迁移预览、TTL/身份/未知字段保留及并发拒绝。未执行生产迁移、部署或真实上游请求；不生成遥测。
