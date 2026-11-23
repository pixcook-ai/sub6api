# Claude Code 客户端本地工件证据（2026-11-23）

交付边界更新：本报告保留早期静态证据，当前实现以 [透明诊断 SPEC](../../.scratch/claude-oauth-compatibility-20261123/SPEC.md) 为准。旧 default/omit、归因删除与客户端 attribution-off 建议不交付；仅做 Anthropic OAuth 的只读 transparent 观察与 opaque 日志脱敏，integrity_status=unverified。保留真实来源，不伪造 first-party/政府版身份，不移除/篡改 cc_gateway 或遥测归因，不生成 opaque hash，也不新增身份改写；既有行为须明确作为基线解释。

## 结论

本报告的主要证据是**本地第三方包装器 clawgod 持有的备份工件，不是已验真的官方原包**。版本、build time、git SHA 均只是工件声明。该早期静态核验阶段没有下载其他版本、执行 Claude Code、读取登录凭证或发起真实推理。后续已验真官方 CLI 的隔离运行、合成凭证和本地假上游结果另见 [官方运行证据](claude-code-official-runtime-20261123.md)；不得将后续工件真实性归给本报告的第三方备份，也不得将本地发送当成真实上游推理。

1. 本机 claude 入口是 clawgod launcher，以 Bun 执行 cli.cjs。源版本文件和备份声明 2.1.330，包装器版本文件声明 2.3.1；不能当成帖子 2.1.30x 的同版本证据。
2. 备份确有 cc_gateway 构造逻辑，位置是 system 文本中的 x-anthropic-billing-header attribution。条件包含 provider、网关来源、URL/host verdict；不能简化为任何非白名单域名必定附带。
3. 备份确有 wiggly 的采集、feature gate、序列化、HTTP header 注入链；可选 gitOrigin 映射到 jy。这不是只有字符串命中，但仍不能证明该功能实际启用或已发送。
4. 在已读 wiggly 路径中，明确看到的 SHA-256 比较用于识别 HTTP header 名称；没有因此证明它是请求 body 或所有 HTTP headers 的签名。完整 body/header/原生层/服务端校验关系未知，不能据此开发自行重签。
5. 当前解包源已被第三方补丁修改，wiggly gate 中多出 clawgod 条件。必须分开表述备份源、当前 patched 源和未经真实性核验的 native 工件。

## 工件身份

| 对象 | 静态观测 | 限制 |
|---|---|---|
| /Users/leo/.local/bin/claude | 1168 字节 shell launcher，调用 Bun 与 /Users/leo/.clawgod/cli.cjs | 未执行 |
| /Users/leo/.clawgod/.source-version | 2.1.330 | 工件声明 |
| /Users/leo/.clawgod/.clawgod-version | 2.3.1 | 工件声明 |
| /Users/leo/.clawgod/source-backup.json | format=1、version=2.1.330、3532 个文本条目 | 第三方持有备份，不代表官方来源已验真 |
| /Users/leo/.local/bin/claude.orig | Mach-O arm64、363855808 字节 | 未完成它与备份内嵌模块的一致性校验，未核实签名来源 |

备份常量声明 BUILD_TIME 为 2026-11-20T17:34:37Z，GIT_SHA 为 8c664bec500172ded713624f9ecac0eb7e714829；不是本次核实的官方发布时间。

SHA-256：

- source-backup.json：d83730b59c5042b0024f4325cff779f10baafce3a3d7fd563cec3a6c5113c035。
- claude.orig：f7c5093b9dd6560d08af28bc27b123fb55091deae3180ac39df199522da86815。

## 调用链证据

以下偏移均是 source-backup.json 的 files[逻辑路径] 字符串中的 **0-based Unicode 字符偏移**，不是二进制地址。用逻辑路径、符号或锚点可重定位。

### cc_gateway

逻辑路径 bunfs/chunk-865at33z.js，锚点 cc_gateway 偏移约 919071，minified 源第 129 行。该备份条目与当前 /Users/leo/.clawgod/bunfs/chunk-865at33z.js 内容一致，SHA-256：65d57de145075b3889bcc0427cf0df65a3c8eaa66624fc4d18451190614a733f。

构造顺序：从 gateway provider 获取 route；计算 gateway 来源标志；取 route URL 或 ANTHROPIC_BASE_URL；对 URL 和 hostVerdict 做判断；满足条件才生成 cc_gateway=1 或 cc_gateway=env；最后拼入 x-anthropic-billing-header attribution 文本。此处未展开全部 helper 的语义及白名单集合，因此不将帖子旧白名单用作无版本限制的实现规格。

### wiggly

逻辑路径 bunfs/chunk-gq1dr8pm.js。备份条目 SHA-256：005253b33569c6ffdf381cbecb82fe8bdb93bd81c2205b0b6b47b339fa3432ed；当前文件 SHA-256：27121d56b331dc93ca274c8ecc7869243a95e8da53f9543f706ba369ebe6dedc。两者不一致。

| 符号/偏移 | 备份直接观测 |
|---|---|
| Op / 272731 | 头名 x-claude-code-wiggly-dove |
| nS / 274928 | gate 名 tengu_wiggly_dove |
| ah / 277746 | Ss() 为真、b(nS,false) 为真、再通过 gS；未展开全部 helper 或其运行值 |
| pS / 276288 | 采集 querySource、客户端版本、入口、权限模式/旁路能力、autoMode、classifier、computerUse、容器、沙箱 posture、平台、hostEnvironment、gitOrigin 等 |
| vS / 279843 | 映射为短字段；gitOrigin 非 undefined 时加入 key=Vp()，而 Vp() 返回 jy |
| AS / 280840 附近 | Tp 序列化，限制成员数/键长/值长，超限字段按规则丢弃并告警 |
| fms / 281257 | 未通过 ah 就返回；否则 AS(vS(await pS(...))) |
| 请求构造 / 295685 | 使用本轮 querySource、权限、toolNames、classifier 标志调用 fms |
| Qi(Fn,...) / 304955、305969 等 | 将值合入不同 provider 分支的 defaultHeaders；不证明分支实际走到或请求已上网发送 |

fS（276074）按 oa→dr→Mi→dft 获取候选 gitOrigin，拒绝非可打印 ASCII，截去 ?/# 后内容，限制到最多 126 字符，空值不发送。未展开这条 helper 链的全部 git 配置读取行为，不能声称会上传本地所有 git remote。

当前文件的 ah 中增加 globalThis.__clawgodPatches 对 wiggly-dove 的前置条件。这是第三方修改存在的证据；本次不修改、不执行、不判定补丁实际启用。

## Hash 与字段绑定

| 位置 | 能证明什么 | 未证明什么 |
|---|---|---|
| Xo(e)，chunk-gq1dr8pm.js，约 274780 | 对 e.toLowerCase() 做头名直接比较或 SHA-256 与常量比较，用于识别 HTTP header 名称 | 不是已证明的 body 完整性签名；不能推出改头就封号 |
| fms→pS→vS→AS | wiggly 字段由上述客户端状态构造；这些函数签名没有完整请求 body 参数；Qi 与已有 HTTP headers 合并 | 其他模块、Bun 原生层、服务端是否额外绑定 body/headers/环境，未知 |
| mDi(e,n)，chunk-4mh8nmh5.js，约 1812 | cc_version 后缀 SHA-256 输入为固定盐 59cf53e54c78 + 首条非 meta user 文本 JS 下标 4/7/20（空位为 0）+版本，取前三位 hex | 不是整个请求的强认证，也不是已证明的 wiggly 签名 |
| cch=00000，chunk-865at33z.js，918251 | placeholder 在 firstParty 的特定条件或 vertex 分支加入 | 未完成最终发送字节 CCH 替换/计算链验证；与 body、headers、原生层的完整关系未知 |

chunk-4mh8nmh5.js 备份与当前文本一致，SHA-256：55c54786357d612ded23a7e24c3369475de090e8b3e393bd418ccc51652c7a54。

当前只交付字段识别、脱敏诊断、版本差异观测与本地最终出站比较；不新增转发或归因省略策略。未知签名不自行重签，不伪造设备/隐私状态，不移除或改写 cc_gateway/遥测归因，也不把字段存在或完整性未知转换成账号安全结论。mode=transparent 只表示观察模式，不表示原有身份、billing、beta 变换消失。

## 范围与限制

- 对工作区 .scratch、.verification、文档及几个相关仓库浅层做了按名有界查找，未找到明确命名的 2.1.283 提取包；不是全盘不存在的证明。本机另有 2.1.322/327/328 native 文件，本次未重复分析。
- REA 6.9.0 对当前 bunfs 的三个选定文件做静态分析；目录 relevant files=3536，仅两个文件完成语法解析，大模块 chunk-865at33z.js 达到 30000ms 单文件超时；语义节点/unknown 上限也被命中。结果为 partial，不能作为完整图谱或缺失证明。
- REA partial Evidence：ev_a8c643b789f29b1610e6dc32c79cd890777dacb95b67720d27986e58cf84635f；summary projection：ev_c7cd4417e01d9f7ae22bed2319b8ff77ff7f5874d9171544db597fe5141ad3b4。它们是本次分析会话引用；本报告保留关键源锚点、哈希、观察与限制，未额外导出工件。
- REA 当前目录身份哈希：31b79718d77b6f4912230f2d11556c8e457aee7dd484ecbdd2690534e39832f2；仅标识当前第三方解包目录，不认证 Anthropic 来源。
- 未证明 wiggly hash/校验与篡改暴露或封号的因果；未证明禁用产品遥测会禁用模型请求中的字段；本报告未验证生产账号、生产网关或实际风控结果。后续本地受控比较的范围与结果由独立运行报告记录，不改变上述证据等级。

