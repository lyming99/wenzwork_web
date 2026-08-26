# WenzWork Relay Windows 脚本

本目录提供不依赖第三方 Service Wrapper 的 Windows Relay 生命周期脚本。服务固定注册为 `WenzWorkRelay`，以 `NT AUTHORITY\LocalService` 运行，支持 Windows `amd64`（主机报告 `AMD64`）和 `arm64`（主机报告 `ARM64`）。脚本需要 Windows PowerShell 5.1 或更高版本；安装、启停、升级和卸载必须从“以管理员身份运行”的 PowerShell 执行。

## 发布包与信任链

Windows Release 必须包含：

```text
bin/wenzwork-relay-server.exe
bin/relayctl.exe
scripts/windows/Install.ps1
scripts/windows/Upgrade.ps1
scripts/windows/Start.ps1
scripts/windows/Stop.ps1
scripts/windows/Healthcheck.ps1
scripts/windows/Uninstall.ps1
scripts/windows/lib/RelayCommon.psm1
VERSION
release-manifest.json
```

从归档安装或升级时，脚本严格按以下顺序处理：

1. 对从管理端 HTTPS bootstrap 获得的受信 `relayctl.exe` 校验 `-VerifierSha256`。
2. 调用受信 verifier 的 `release verify-bundle`，用固定 Release 公钥验证 `SHA256SUMS.sig`，并核对所选归档 SHA-256。
3. 拒绝归档中的绝对路径、父目录穿越、链接、特殊文件、重复 ZIP 路径和过大的 ZIP 内容，然后解压到随机临时目录。
4. 独立读取 `release-manifest.json`，要求 `platform=windows` 且架构与当前主机完全匹配；再由同一受信 verifier 校验版本、协议和所有文件摘要。
5. 将完整版本先写入 `releases/.stage.*`，复验后在同一卷内改名为 `releases/<version>`。

不要用归档内尚未验证的 `relayctl.exe` 验证该归档本身。`-PackageDirectory` 仅用于已经在外部完成同一套 `verify-bundle` 校验并安全解压的目录；日常安装应优先使用签名归档。

## 安装

安装目录必须是本机盘上的绝对路径，可以包含空格，但不能是盘符根目录、Windows 根目录、Program Files 根目录、ProgramData 根目录、用户目录或重解析点。交互运行且未显式传入 `-InstallRoot` 时，脚本会显示 `Relay work/install directory [%ProgramFiles%\WenzWork\Relay]`，可以输入自定义绝对路径，或直接回车采用显示的默认值。显式传入空的 `-InstallRoot` 会被拒绝，不会被当作“未传参数”。

从管理端下载签名归档并安装：

```powershell
.\Install.ps1 `
  -InstallRoot 'D:\WenzWork\Relay' `
  -ManagementUrl 'https://management.example.com' `
  -ArtifactUrl 'https://management.example.com/downloads/wenzwork-relay-VERSION-windows-amd64.tar.gz' `
  -ChecksumsUrl 'https://management.example.com/downloads/SHA256SUMS' `
  -ChecksumsSignatureUrl 'https://management.example.com/downloads/SHA256SUMS.sig' `
  -SigningKeyFile 'C:\Bootstrap\release-signing-public-key.pem' `
  -VerifierFile 'C:\Bootstrap\relayctl.exe' `
  -VerifierSha256 '<管理端 HTTPS bootstrap 显示的 64 位十六进制摘要>'
```

命令不会接收明文 `-AccessKey` 参数。管理端生成的一键交互命令不传 `-AccessKeyStdin`，会使用 `Read-Host -AsSecureString` 明确提示并隐藏输入；无人值守环境可以从已重定向的标准输入或受限文件读取：

```powershell
Get-Content -LiteralPath 'C:\Secure\relay-access-key.txt' |
  .\Install.ps1 <其余参数> -AccessKeyStdin

.\Install.ps1 <其余参数> -AccessKeyFile 'C:\Secure\relay-access-key.txt'
```

Access Key 文件不能是链接，且 ACL 不得允许 Everyone、Authenticated Users 或 Users 读取。脚本不会把 Key 写入 URL、命令行或日志。最终环境文件位于 `<InstallRoot>\config\relay.env`，使用无 BOM UTF-8 原子写入；安装根 ACL 只保留 SYSTEM、Administrators 和 `NT SERVICE\WenzWorkRelay`。SCM 的每服务环境仅保存 `RELAY_ENV_FILE` 路径，不保存 Key 本身。确认安装成功后应安全删除输入 Key 文件。

`-AccessKeyStdin` 只接受管道或重定向输入；若标准输入仍连接交互控制台，脚本会立即拒绝，避免明文回显或等待误操作。

无人值守安装应传入 `-NonInteractive`。此模式不会显示工作路径、管理地址或 Access Key 提示：未传 `-InstallRoot` 时使用 `%ProgramFiles%\WenzWork\Relay`，未传 `-ManagementUrl` 时使用 `https://wenzwork.com`，并且必须显式选择 `-AccessKeyStdin` 或 `-AccessKeyFile`，否则立即失败。例如：

```powershell
.\Install.ps1 <签名归档与 verifier 参数> `
  -NonInteractive `
  -InstallRoot 'D:\WenzWork\Relay' `
  -ManagementUrl 'https://management.example.com' `
  -AccessKeyFile 'C:\Secure\relay-access-key.txt'
```

## 启停与健康检查

```powershell
.\Start.ps1
.\Healthcheck.ps1 -Mode ready -WaitSeconds 30
.\Stop.ps1
```

健康检查只允许访问回环 HTTP 地址，默认使用 `http://127.0.0.1:19090/health/ready`。若管理端为该节点配置了不同的本地健康端口，请显式传入 `-BaseUrl` 或 `-HealthBaseUrl`。

## 升级与自动回滚

升级前必须先在管理端 Drain 节点并将它从外部负载均衡摘除。升级默认复用当前已验证 Release 中的 `bin\relayctl.exe` 作为 verifier，因此不需要再次传入 `-VerifierFile`；若显式换用 bootstrap verifier，仍必须同时传 `-VerifierSha256`。

```powershell
.\Upgrade.ps1 `
  -InstallRoot 'D:\WenzWork\Relay' `
  -PackageFile 'D:\Packages\wenzwork-relay-VERSION-windows-amd64.tar.gz' `
  -ChecksumsFile 'D:\Packages\SHA256SUMS' `
  -ChecksumsSignatureFile 'D:\Packages\SHA256SUMS.sig' `
  -SigningKeyFile 'D:\WenzWork\Relay\releases\CURRENT_VERSION\release-signing-public-key.pem' `
  -ConfirmDrained
```

脚本在旧服务仍运行时完成下载、外层签名校验、解压、Manifest 校验和版本目录 staging。之后停止服务，通过一次原生 `sc.exe config ... binPath=` 操作把 SCM ImagePath 切到完整的新版本目录，原子更新 `RELAY_VERSION`，启动并等待 Readiness。任一步失败都会恢复旧 ImagePath 和旧环境文件、重启旧版本并再次检查 Readiness；回滚也失败时节点必须继续留在负载均衡之外。

## 卸载

默认卸载服务与所有 Release，但保留包含 Access Key 的 `config` 目录，便于人工恢复：

```powershell
.\Uninstall.ps1 -InstallRoot 'D:\WenzWork\Relay'
```

永久删除安装根目录、Access Key 和配置必须显式二次确认：

```powershell
.\Uninstall.ps1 `
  -InstallRoot 'D:\WenzWork\Relay' `
  -ConfirmUninstall `
  -Purge `
  -ConfirmPurge DELETE_RELAY_DATA
```

`-Purge` 删除不可恢复。执行前应先在管理端吊销 Key、Drain 节点并确认不再承载流量。

## 自包含测试

测试不安装或修改 Windows Service，只验证 PowerShell 语法、架构映射、安装路径与 URL 边界、Manifest 目标匹配、环境原子更新、verifier 摘要固定及脚本静态安全契约：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\RelayScripts.Tests.ps1
```

真实发布前还必须分别在 Windows amd64 和 Windows arm64 主机执行安装、重启、升级成功、健康失败自动回滚和卸载演练。
