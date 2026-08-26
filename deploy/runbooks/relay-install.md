# Relay 主机安装 Runbook

适用范围是以下六个独立构建和签名的目标。控制面不保存 SSH、WinRM 或远程桌面凭据，也不会登录宿主机执行安装。

| 宿主机 | Release 目标 | 原生服务 | 安装脚本 |
| --- | --- | --- | --- |
| Ubuntu 22.04/24.04、Debian 12，x86_64 | `linux/amd64` | systemd | [`deploy/relay/install.sh`](../relay/install.sh) |
| Ubuntu 22.04/24.04、Debian 12，aarch64 | `linux/arm64` | systemd | [`deploy/relay/install.sh`](../relay/install.sh) |
| Windows 10/11、Server 2019/2022，AMD64 | `windows/amd64` | `WenzWorkRelay` Windows Service | [`deploy/relay/windows/Install.ps1`](../relay/windows/Install.ps1) |
| Windows 11、Server 2022 on ARM，ARM64 | `windows/arm64` | `WenzWorkRelay` Windows Service | [`deploy/relay/windows/Install.ps1`](../relay/windows/Install.ps1) |
| macOS 13+，Intel | `darwin/amd64` | `com.wenzwork.relay` LaunchDaemon | [`deploy/relay/darwin/install.sh`](../relay/darwin/install.sh) |
| macOS 13+，Apple silicon | `darwin/arm64` | `com.wenzwork.relay` LaunchDaemon | [`deploy/relay/darwin/install.sh`](../relay/darwin/install.sh) |

## 安装前

1. 在“中继主机”页面选择宿主平台和架构，创建主机，并只选择状态为 `published` 且目标完全匹配的 Relay Release。
2. 保存只显示一次的 Access Key，并复制管理端生成的平台原生一键安装命令。命令本身不包含 Access Key。
3. 核对时间同步、安装盘至少 512 MiB 可用空间、出站 HTTPS、管理页指定的 Relay WS 监听端口和外部 Nginx/LB 规划。
4. Access Key 只允许通过隐藏输入、标准输入或权限受限文件提供；不得放入 URL、命令参数、工单或日志。

在线命令会先校验 bootstrap 脚本和目标架构 `relayctl` 验证器的 SHA-256；验证器再通过固定 Ed25519 公钥认证 `SHA256SUMS`、校验归档摘要。只有外层验证通过后才解压，并继续检查 Manifest 的平台、架构、协议版本和逐文件摘要。没有 `--skip-verify` 或等价绕过路径。

## Linux / systemd

以 root 运行管理页命令。交互式安装会提示工作目录（默认 `/opt/wenzwork-relay`）、管理端 HTTPS 地址和隐藏 Access Key。配置保存在 `/etc/wenzwork-relay`，运行状态保存在 `/var/lib/wenzwork-relay`；自定义目录只承载并排 Release 和 `current` 链接。

自动化可以提供权限为 `0600` 的环境文件：

```dotenv
RELAY_ACCESS_KEY=relay_<43-character-secret>
RELAY_MANAGEMENT_URL=https://control.example.com
```

`RELAY_MANAGEMENT_URL` 可由部署者使用 HTTP 或 HTTPS；HTTP 会明文传输 Relay Access Key 和管理请求，只应在明确接受该风险的可信网络中使用。

```bash
sudo bash install.sh \
  --install-root /srv/wenzwork/relay \
  --relay-env-file ./relay.env \
  --artifact-url https://downloads.example.com/relay/VERSION/wenzwork-relay-VERSION-linux-ARCH.tar.gz \
  --checksums-url https://downloads.example.com/relay/VERSION/SHA256SUMS \
  --checksums-signature-url https://downloads.example.com/relay/VERSION/SHA256SUMS.sig
```

安装后执行：

```bash
sudo systemctl status wenzwork-relay.service --no-pager
install_root=$(awk -F= '$1 == "RELAY_INSTALL_ROOT" {print $2}' /etc/wenzwork-relay/install.conf)
sudo "$install_root/current/scripts/healthcheck.sh" --live
sudo relayctl status
```

## Windows / Windows Service

在管理员 PowerShell 5.1 或更高版本中运行管理页命令。脚本自动把 `AMD64` 映射为 `amd64`、把 `ARM64` 映射为 `arm64`，拒绝 32 位系统、网络路径和 reparse-point 安装目录。默认目录为 `%ProgramFiles%\WenzWork\Relay`；凭据文件只允许 `SYSTEM`、`Administrators` 与服务 SID 读取，SCM 环境只保存 `RELAY_ENV_FILE` 路径。

具体参数和离线示例见 [`deploy/relay/windows/README.md`](../relay/windows/README.md)。常用检查：

```powershell
Get-Service WenzWorkRelay
& "$env:ProgramFiles\WenzWork\Relay\current\scripts\Healthcheck.ps1" -Mode live
```

升级前排空节点，再运行当前 Release 的 `Upgrade.ps1`。脚本通过 SCM `ImagePath` 单步切换版本；启动或 Ready 检查失败会恢复旧 `ImagePath`、环境文件并复检旧版本。

## macOS / launchd

以 root 在 macOS 13 或更高版本运行管理页命令。脚本自动把 Intel `x86_64` 映射为 `amd64`、Apple silicon `arm64` 映射为 `arm64`。默认目录为 `/usr/local/lib/wenzwork-relay`，root-only 凭据位于 `/Library/Application Support/WenzWork/Relay/relay.env`。

具体参数和离线示例见 [`deploy/relay/darwin/README.md`](../relay/darwin/README.md)。常用检查：

```bash
sudo launchctl print system/com.wenzwork.relay
sudo /usr/local/lib/wenzwork-relay/current/scripts/healthcheck.sh --live
```

升级前排空节点，再运行当前 Release 的 `scripts/upgrade.sh`。脚本原子切换 `current`，重载 LaunchDaemon；启动或 Ready 检查失败会恢复旧链接、环境文件和 plist。

## 离线安装

通过受控介质带入同一目标的 `.tar.gz`、`SHA256SUMS`、`SHA256SUMS.sig`、已核对 Key ID 的 `release-signing-public-key.pem`，以及从管理端 HTTPS 取得并独立核对 SHA-256 的同平台 `relayctl` 验证器。分别使用 Bash 的 `--package-file/--verifier-file` 或 PowerShell 的 `-PackageFile/-VerifierFile` 参数；平台与架构不匹配时必须失败。

## 激活与失败处理

Relay 会凭 Access Key 拉取管理端配置，以明文 WS 启动指定监听端口，注册运行实例并上报心跳。使用 WSS 时，证书与私钥只配置在 Nginx/LB，外部 WSS 链接保存到管理端。完成 LB、DNS、监听端口、外部 WS/WSS 与 mTLS 四项检查后，才在管理端启用主机。

- Access Key 无效或已吊销：在管理端更换 Key，再通过平台权限保护的新环境文件安装；旧 Key 不可恢复。
- 签名或摘要失败：隔离下载文件，记录 Release、Key ID、SHA-256 和来源；禁止继续解压。
- 安装目录校验失败：使用本机专用、规范化绝对路径；不要使用系统根目录、网络路径、符号链接或 reparse point。
- 管理端不可达：保持主机未激活，检查 DNS、TLS 链和出站防火墙。

工单可以记录 Installation ID、Release 目标、主机资产 ID、安装目录、操作人和外部检查结果；不得记录 Access Key。

## 实机验收边界

CI 会解析三平台脚本、运行 Windows 自包含断言、执行 Linux/macOS 静态脚本测试，并交叉编译六个目标。CI 环境不能代替真实 SCM、launchd、Gatekeeper、主机重启和 ARM 硬件行为；首次生产发布及脚本/服务模板变更后，必须分别在 Windows amd64/arm64、macOS Intel/Apple silicon 和 Linux amd64/arm64 测试主机完成安装、重启自启、升级成功、升级回滚、卸载与重新安装演练，并保存验收记录。
