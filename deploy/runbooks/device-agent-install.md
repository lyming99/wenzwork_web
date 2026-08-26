# Device Agent 三平台安装手册

本文适用于正式 Release 中的 `wenzwork-device-agent-<version>-<platform>-<architecture>.tar.gz`。源码目录中的手工 `go build` 只用于开发，不是生产安装路径。

## 1. 安装前提与信任链

正式安装没有 `skip verify` 路径，必须依次通过：

1. 从已认证的管理 HTTPS 通道取得 bootstrap 脚本、固定 Release Ed25519 公钥，以及与目标平台同架构的 `relayctl` 验证器；验证器名称沿用 Relay 工具，但其 Release 校验子命令与产品无关。
2. 用独立公布的 SHA-256 校验 bootstrap 验证器。Windows 与 macOS 安装器强制要求该摘要；Linux 使用本机 OpenSSL 直接验证 `SHA256SUMS.sig`。
3. 验证 Ed25519 签名的 `SHA256SUMS` 和所选归档摘要，再进行拒绝链接、特殊文件及路径穿越的安全解压。
4. 验证包内公钥与受信公钥完全一致，并验证 `release-manifest.json` 的版本、平台、架构、协议区间及逐文件摘要。
5. Windows 安装器要求两个可执行文件都有受信 Authenticode 签名；macOS 安装器要求 Developer ID 签名通过 `codesign`，并通过 Gatekeeper/公证策略 `spctl`。

任何一步失败都不得注册或切换服务。不要用归档内尚未验证的 `relayctl` 验证归档本身。

## 2. 固定目录和服务身份

| 平台 | 版本目录 | 配置 | 业务数据 | 服务身份 |
| --- | --- | --- | --- | --- |
| Linux | `/opt/wenzwork-device-agent/releases/<version>` | `/etc/wenzwork-device-agent` | `/var/lib/wenzwork-device-agent` | `wenzwork-agent` |
| macOS | `/usr/local/lib/wenzwork-device-agent/releases/<version>` | `/Library/Application Support/WenzWork/DeviceAgent/config` | `/Library/Application Support/WenzWork/DeviceAgent/data` | `_wenzworkagent` |
| Windows | `%ProgramFiles%\WenzWork\DeviceAgent\releases\<version>` | `%ProgramData%\WenzWork\DeviceAgent\config` | `%ProgramData%\WenzWork\DeviceAgent\data` | `LocalSystem` + 独立服务 SID |

Linux/macOS 安装器要求数据根与其父目录位于同一文件系统，并拒绝数据树内的子挂载、符号链接、硬链接和特殊文件；Windows 安装器拒绝数据根中的 Junction、符号链接、硬链接、其他 reparse point，以及祖先 reparse point。该约束保证升级失败时可以在同一卷先完整暂存快照，再用目录重命名原子换位。不要只把表中的数据根目录单独挂载成一个卷。

服务命令只传 `serve --env-file <受保护文件>`，不会把 Device Access Key 放进 SCM、launchd、systemd 或进程参数。Linux/macOS 服务包固定使用加密文件 SecretStore，使身份、业务库和 AI 密钥能进入同一个停机快照；Windows 默认使用同机 DPAPI，密文文件仍位于受管数据目录。

服务账户默认不能访问用户源码目录。登记外部项目之前，应只给服务账户授予该项目需要的最小读写权限；不要把服务改成日常管理员账户。

## 3. 准备环境文件

从目标平台的 `device-agent.env.example` 复制，不要直接修改归档内模板。必填项为 Control Plane URL、一次性 `device_...` Access Key、固定状态路径和 SecretStore 模式。状态路径必须保持安装器给出的受管路径，否则升级无法证明备份完整。

Linux/macOS：

```bash
install -m 0600 device-agent.env.example /tmp/wenzwork-device-agent.env
# 仅在本机安全编辑器中替换 Access Key 和 Control URL。
```

Windows（管理员 PowerShell）：

```powershell
Copy-Item .\device-agent.env.example $env:TEMP\wenzwork-device-agent.env
icacls $env:TEMP\wenzwork-device-agent.env /inheritance:r /grant:r 'SYSTEM:(F)' 'Administrators:(F)'
# 仅在本机安全编辑器中替换 Access Key 和 Control URL。
```

HTTP 只允许精确回环主机；生产必须使用 HTTPS。环境文件只允许已审核的 `WENZWORK_*` 键，未知键、重复键、错误状态路径或无效 Access Key 会失败关闭。

## 4. Linux 安装

支持 systemd 主机。把以下占位符替换为同一 Release 的文件：

```bash
sudo ./install.sh \
  --package-file ./wenzwork-device-agent-VERSION-linux-ARCH.tar.gz \
  --checksums-file ./SHA256SUMS \
  --checksums-signature-file ./SHA256SUMS.sig \
  --signing-key-file ./release-signing-public-key.pem \
  --agent-env-file /tmp/wenzwork-device-agent.env
```

验证：

```bash
sudo systemctl status wenzwork-device-agent.service
sudo /opt/wenzwork-device-agent/current/scripts/healthcheck.sh --wait 30
```

## 5. Windows 安装

从管理端取得与本机架构一致的 bootstrap `relayctl.exe`，在独立通道核对其 SHA-256：

```powershell
.\Install.ps1 `
  -PackageFile .\wenzwork-device-agent-VERSION-windows-ARCH.tar.gz `
  -ChecksumsFile .\SHA256SUMS `
  -ChecksumsSignatureFile .\SHA256SUMS.sig `
  -SigningKeyFile .\release-signing-public-key.pem `
  -VerifierFile .\relayctl.exe `
  -VerifierSha256 '<64-hex-bootstrap-digest>' `
  -AgentEnvironmentFile $env:TEMP\wenzwork-device-agent.env
```

验证：

```powershell
Get-Service WenzWorkDeviceAgent
.\Healthcheck.ps1 -WaitSeconds 30
```

SCM 注册的是 Agent 的原生 `service` 入口。不要自行改成普通 `serve` 命令；普通控制台进程不满足 Windows Service Control Manager 协议。

## 6. macOS 安装

初装必须提供已独立核对摘要的 bootstrap verifier：

```bash
sudo ./install.sh \
  --package-file ./wenzwork-device-agent-VERSION-darwin-ARCH.tar.gz \
  --checksums-file ./SHA256SUMS \
  --checksums-signature-file ./SHA256SUMS.sig \
  --signing-key-file ./release-signing-public-key.pem \
  --verifier-file ./relayctl \
  --verifier-sha256 '<64-hex-bootstrap-digest>' \
  --agent-env-file /tmp/wenzwork-device-agent.env
```

验证：

```bash
sudo launchctl print system/com.wenzwork.device-agent
sudo /usr/local/lib/wenzwork-device-agent/current/scripts/healthcheck.sh --wait 30
```

安装前会同时执行 Developer ID 签名和 Gatekeeper 公证判断；离线环境不能绕过这一步，应先通过受控联网主机完成验证与安装介质审批。

## 7. 安装后检查

- `current/VERSION` 与 `wenzwork-device-agent version` 必须完全相等。
- 服务连续运行至少经过健康检查稳定窗口，且没有快速重启。
- 状态、BusinessStore、控制状态和 SecretStore 只能由服务身份与管理员访问。
- Web 的设备公钥指纹与本机 `identity` 输出一致。
- 先只开启项目/文件只读，再逐项开启写文件、PTY、任务和 AI 工具；高风险能力必须同时具备独立 Scope、项目策略和 feature flag。

升级、完整数据快照、失败回滚和保数据卸载见[《Device Agent 升级与回滚手册》](./device-agent-upgrade-rollback.md)。
