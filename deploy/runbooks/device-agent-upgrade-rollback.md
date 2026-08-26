# Device Agent 升级、数据备份与回滚手册

## 1. 原子边界

升级事务的业务边界不是单个 SQLite 文件，而是以下同一设备身份的数据集：

- 主身份状态 `agent-state.json`；
- BusinessStore `agent-state.json.business.sqlite` 及停机时可能存在的 SQLite sidecar；
- 加密控制状态 `agent-state.json.remote-control.enc`；
- Unix 加密 SecretStore `agent-state.json.secrets.enc` 或 Windows DPAPI 文件 `agent-state.json.secrets.dpapi`；
- 任务运行时私有目录和受管数据目录内的其他版本化状态；
- 受保护的 `agent.env`，其中包含接入配置与 feature flag。

安装器必须先停止服务，再复制完整数据根和环境文件。用户项目正文不属于 Agent 业务快照，升级器不会复制项目目录；BusinessStore 中只保存其注册关系。项目本身应使用组织既有备份策略。

## 2. 自动升级顺序

三平台脚本执行相同状态机：

1. 使用当前已验证 Release 中的 verifier 验证新归档外层签名、摘要、Manifest、目标平台和平台代码签名；
2. 把新 Release 写入全新的不可变版本目录，再次逐文件验证；
3. 停止 Agent，等待进程树退出；
4. 创建完整数据与配置快照并写入 `BACKUP-METADATA`；
5. 原子切换 systemd/launchd/SCM 到新版本；
6. 启动服务，验证服务状态、二进制版本和稳定窗口；
7. 成功时保留本次快照，并把历史快照限制为最近 5 份；
8. 任一步失败时停止新版本，保留失败后的数据副本，恢复旧快照和旧二进制，再次健康检查。

如果旧版本恢复也失败，脚本会停止宣称成功并输出备份位置。此时不要反复启动新旧版本写同一数据集。

## 3. 升级命令

Linux：

```bash
sudo /opt/wenzwork-device-agent/current/scripts/upgrade.sh \
  --package-file ./wenzwork-device-agent-VERSION-linux-ARCH.tar.gz \
  --checksums-file ./SHA256SUMS \
  --checksums-signature-file ./SHA256SUMS.sig \
  --confirm-upgrade
```

Windows（管理员 PowerShell）：

```powershell
& "$env:ProgramFiles\WenzWork\DeviceAgent\releases\CURRENT\scripts\Upgrade.ps1" `
  -PackageFile .\wenzwork-device-agent-VERSION-windows-ARCH.tar.gz `
  -ChecksumsFile .\SHA256SUMS `
  -ChecksumsSignatureFile .\SHA256SUMS.sig `
  -ConfirmUpgrade
```

macOS：

```bash
sudo /usr/local/lib/wenzwork-device-agent/current/scripts/upgrade.sh \
  --package-file ./wenzwork-device-agent-VERSION-darwin-ARCH.tar.gz \
  --checksums-file ./SHA256SUMS \
  --checksums-signature-file ./SHA256SUMS.sig \
  --confirm-upgrade
```

升级默认信任当前 Release 固定的公钥和 verifier。只有进行正式密钥轮换时才传入新的 bootstrap verifier；轮换必须同时有独立 SHA-256 和双人审批记录。

## 4. 备份位置与恢复限制

| 平台 | 自动备份位置 |
| --- | --- |
| Linux | `/var/backups/wenzwork-device-agent/<UTC>-<old-version>` |
| macOS | `/Library/Application Support/WenzWork/DeviceAgent/backups/<UTC>-<old-version>` |
| Windows | `%ProgramData%\WenzWork\DeviceAgent\backups\<UTC>-<old-version>` |

备份与原状态包含设备私钥、刷新凭据或 AI 密钥密文，仍按敏感资产处理。Windows DPAPI 备份只能在同一机器、同一服务安全上下文恢复；不要把它当成跨设备迁移格式。Unix 正式服务使用身份派生的加密文件 SecretStore，必须和主身份状态一起恢复。

自动备份和恢复只接受由普通目录与单链接普通文件组成的单文件系统数据树。Linux/macOS 遇到符号链接、硬链接、数据树内的子挂载或特殊文件会拒绝；Windows 遇到 Junction、符号链接、硬链接或其他 reparse point 会拒绝。恢复前后都会重新扫描快照，并且只从安装器管理的备份根恢复。不要在 Agent 数据根或备份根中放置指向项目目录、共享盘或其他系统目录的链接。

自动恢复会先在数据根所在卷完整暂存快照、校验环境文件并恢复 ACL/所有者，随后才用同卷重命名切换活动数据。暂存复制遭遇磁盘写入失败时，现有活动数据不会被移走；切换或环境文件原子替换失败时，脚本会优先把原数据放回，并把失败原因作为回滚失败上报。

Linux/macOS 的数据根本身也不能是独立文件系统挂载点，否则同级回滚暂存目录不在同一文件系统，安装或升级会在修改服务前失败关闭。需要独立数据卷时，应在隔离主机或 mount namespace 中让数据根及其同级回滚暂存路径一起位于该卷；不能只把默认数据根目录单独挂载。

BusinessStore migration 在启动事务内前向执行，不提供直接对生产库运行的“降版本 SQL”。二进制回滚必须配套恢复升级前完整快照；只切换旧二进制而继续使用已迁移数据库不构成受支持回滚。

## 5. 手工恢复

只有自动回滚也失败时才手工恢复，并先保存现场：

1. 确认服务已停止且没有 `wenzwork-device-agent` 子进程；
2. 核对备份目录包含 `BACKUP-METADATA`、`data` 和 `config/agent.env`；
3. 把当前数据根重命名到带 UTC 时间的 `failed` 目录，不要覆盖或删除；
4. 复制备份数据与环境文件，并恢复平台 ACL/所有者；
5. 把服务 ImagePath/版本链接切回备份记录对应的旧 Release；
6. 启动并执行健康检查、身份指纹检查和只读文件 RPC；
7. 在确认恢复完成前，不重新开放任务、PTY 或 AI 工具。

手工命令因平台 ACL 和组织路径不同，不在本手册提供可直接粘贴的递归删除命令。恢复执行人必须把解析后的绝对目标记录到变更单并由第二人复核。

## 6. 卸载语义

默认卸载只删除服务注册和版本目录，明确保留配置、身份、SecretStore、BusinessStore、备份和项目目录：

```bash
sudo ./uninstall.sh --confirm
```

```powershell
.\Uninstall.ps1 -ConfirmUninstall
```

只有明确执行永久退役，才可使用 `--purge --confirm-purge DELETE_DEVICE_AGENT_DATA` 或 `-Purge -ConfirmPurge DELETE_DEVICE_AGENT_DATA`。Purge 不会删除受管目录之外的用户项目，但会不可恢复地删除 Agent 身份、AI 密钥、任务/会话数据库和安装器备份。
