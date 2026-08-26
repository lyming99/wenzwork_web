# Device Agent 正式发布与灰度手册

## 1. 发布前硬门禁

GitHub Release 工作流只在以下条件全部满足时生成 Agent 包：

- 在使用平台签名证书前，重新执行锁定依赖下的 OpenAPI、Web、Go 与跨端契约完整门禁；
- tag 所指 commit 必须已有一次成功的 push 触发主 CI，覆盖 PostgreSQL/Redis 集成测试、race、ShellCheck、跨目标构建和 secret scan；
- Linux/Windows/macOS 双架构均可构建；
- Bash/PowerShell 安装生命周期契约通过；Linux 私有 mount namespace 还必须证明数据根挂载点和数据树内子挂载均失败关闭，并在真实 `ENOSPC` 后证明身份状态原子回滚、释放空间后可恢复；
- Windows Agent 与 verifier 完成 Authenticode 时间戳签名并由 `signtool`、PowerShell 双重验证；
- macOS 两架构在原生 Runner 上以启用 Hardened Runtime 的 Developer ID 签名，通过 Apple `notarytool`，审计公证日志并由 Gatekeeper 复核；
- 六个 Agent 归档都有目标绑定 Manifest，归档与 Manifest 进入 Ed25519 签名的 `SHA256SUMS`；
- 发布输出扫描不到私钥材料；任何原生已签名二进制缺失都会失败，不能回退到交叉编译的未签名文件。

Flutter 位于独立 `wenzwork` 仓库，Agent Release 工作流不能从本仓库源码重跑它。发布审批必须记录与该 Agent 契约哈希一致的 Flutter commit、`flutter analyze`、`flutter test` 和目标平台构建 CI 链接；缺少该独立证据时可以生成候选包，但不得进入实体设备灰度或标记 GA。

Release 环境必须配置：

| 名称 | 类型 | 用途 |
| --- | --- | --- |
| `RELAY_RELEASE_SIGNING_PRIVATE_KEY` | Secret | Ed25519 外层归档/Manifest 签名（当前为通用 Release key） |
| `RELAY_RELEASE_SIGNING_KEY_ID` | Variable | Manifest 固定 Key ID |
| `WINDOWS_CODE_SIGNING_CERTIFICATE_BASE64` | Secret | Authenticode PFX |
| `WINDOWS_CODE_SIGNING_CERTIFICATE_PASSWORD` | Secret | PFX 导入密码 |
| `WINDOWS_CODE_SIGNING_TIMESTAMP_URL` | Variable | RFC 3161 时间戳服务 |
| `APPLE_DEVELOPER_ID_APPLICATION` | Secret | Developer ID Application 身份 |
| `APPLE_SIGNING_CERTIFICATE_P12_BASE64` | Secret | macOS 签名证书 |
| `APPLE_SIGNING_CERTIFICATE_PASSWORD` | Secret | P12 密码 |
| `APPLE_NOTARY_KEY_P8_BASE64` | Secret | App Store Connect Notary API 私钥 |
| `APPLE_NOTARY_KEY_ID` / `APPLE_NOTARY_ISSUER_ID` | Secret | 公证 API 标识 |

缺少任一凭据是发布失败，不是允许人工上传未签名包的理由。

## 2. 发布资产复核

每次 Release 至少应有六个 `wenzwork-device-agent-...tar.gz`、六份目标 Manifest 及签名、`SHA256SUMS`、`SHA256SUMS.sig`、Release 公钥。复核人需在干净主机：

1. 验证 `SHA256SUMS.sig`；
2. 执行 `sha256sum -c` 或 verifier 等价命令；
3. 随机抽取三平台包，确认没有链接/特殊文件、私钥、源码工作路径或其他目标二进制；
4. 分别验证 Authenticode、Developer ID/Gatekeeper 和 Manifest 目标；
5. 核对 `VERSION`、Release tag、commit、Key ID 与变更单一致；
6. 保存 CI run、签名证书指纹、公证 submission ID 和校验输出，禁止保存证书私钥或密码。

## 3. 兼容包与回退面

- 至少保留当前版和上一稳定版的六目标签名包、公钥链与安装器。
- v1/v2 四种客户端/Agent 组合必须继续通过权威 fixture；不得以“客户端会同时升级”为前提删除降级路径。
- 每台升级设备都保留升级前完整数据快照。二进制回退必须恢复对应快照。
- 高风险能力独立关闭：`-terminal.interactive,-tasks.v2,-workflow.v2,-ai.workspaceTools`。发生 AI v2 专项问题时可另加 `-ai.v2`；项目与文件只读不随之关闭。
- Scope/项目策略撤销与本地 kill switch 应在下一轮操作前生效；需要终止活动 PTY/任务时同时撤销对应项目策略。

## 4. 四阶段灰度

每一阶段都必须有负责人、开始/结束时间、目标设备清单、上一稳定包、feature flag 回退和停止条件。

| 阶段 | 建议范围 | 最短观察 | 重点 |
| --- | ---: | ---: | --- |
| 内部设备 | 3 平台各至少 2 台 | 48 小时 | 安装/升级/休眠/网络切换/回滚全场景 |
| 测试用户 | 明确同意的测试群 | 72 小时 | 真实项目兼容、权限说明、支持工单 |
| 小比例设备 | 1%，按 OS/架构分层 | 7 天 | 崩溃率、回滚率、v1 失败率、不可恢复数据 |
| 扩大与全量 | 10% → 25% → 50% → 100% | 每级至少 48 小时 | 容量、长稳、尾延迟与安全事件 |

不得只按总设备数抽样而遗漏某个平台、架构、旧客户端或旧 Agent 组合。

## 5. 自动停止条件

任一条件触发即冻结扩量，先关闭相关高风险 flag；若涉及身份、文件写入或数据一致性，回退整个 Release：

- 任意高危权限绕过、正文/密钥进入控制面或遥测；
- 任意不可恢复数据、跨项目访问、半文件发布或任务重复执行；
- 安装/升级回滚失败，或签名/公证验证异常；
- 新版本崩溃率、连接恢复失败率或稳定错误率超过变更单阈值；
- v1 客户端失败率显著上升，或某平台没有足够实体设备证据。

队列深度、耗时与字节数可用于阈值；项目名、路径、命令、Prompt、回复或文件摘要禁止成为监控标签。

## 6. 发布完成条件

只有四阶段都有签字记录、三平台核心场景与长稳测试通过、灰度窗口无高危安全事件或不可恢复数据问题，才可标记 GA。CI 绿色、包已生成或“未收到投诉”均不能单独证明正式发布完成。
