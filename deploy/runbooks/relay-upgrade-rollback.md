# Relay 逐节点升级与回滚 Runbook

## 门禁

- 目标 Release 已发布，Manifest、Key ID、SHA256SUMS 和离线签名完整。
- 同一组至少另一台主机健康且容量足够。
- 目标主机已排空并从外部 LB 摘除。

## 管理端一键升级

在主机详情的“安装 / 升级”区域选择 Release 并复制命令。命令会下载并校验 `upgrade.sh`，脚本再通过 URL 下载安装包、摘要和签名。脚本从 `/etc/wenzwork-relay/install.conf` 读取安装目录，并复用现有 `/etc/wenzwork-relay` 配置、Access Key 以及 `/var/lib/wenzwork-relay` 状态。

交互运行时输入 `UPGRADE` 确认已排空；自动化环境使用 `--confirm-drained`。也可直接运行：

```bash
sudo /opt/wenzwork-relay/current/scripts/upgrade.sh \
  --artifact-url https://downloads.example.com/relay/<version>/wenzwork-relay.tar.gz \
  --checksums-url https://downloads.example.com/relay/<version>/SHA256SUMS \
  --checksums-signature-url https://downloads.example.com/relay/<version>/SHA256SUMS.sig \
  --confirm-drained
```

离线升级继续支持 `--package-file`、`--checksums-file`、`--checksums-signature-file` 与 `--signing-key-file`。

脚本在解压前校验签名和 SHA-256，将版本并排安装到 `<install-root>/releases/<version>`，原子切换 `current`，更新环境中的 `RELAY_VERSION`，启动并等待 Readiness。配置、身份和状态不会被 Release 覆盖。

## 自动和人工回滚

Readiness 失败时，脚本恢复旧 `current`、旧 systemd Unit 和旧环境文件，再启动上一版本。若自动回滚也失败：

1. 保持主机在 LB 外，不要连续重启。
2. 从 `/etc/wenzwork-relay/install.conf` 读取安装根目录，确认 `current` 只指向其中的 `releases/<approved-version>`。
3. 执行旧版本健康检查并收集白名单诊断包。
4. 在管理端确认心跳、版本和路由状态；二进制回滚不会回滚管理事实版本。
5. 记录恢复时间和失败原因，满足发布回滚规则后再加入 LB。

禁止同时升级同一组的全部主机。
