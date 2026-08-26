# macOS Relay 安装脚本

支持 macOS 13+ 的 `darwin/amd64`（Intel）与 `darwin/arm64`（Apple silicon）。服务由 LaunchDaemon `com.wenzwork.relay` 管理，默认程序目录为 `/usr/local/lib/wenzwork-relay`，凭据文件为 `/Library/Application Support/WenzWork/Relay/relay.env`。

推荐复制管理页生成的一键命令。命令会通过管理端 HTTPS 下载目标架构的受信 `relayctl` 引导验证器，先验证 `SHA256SUMS` 的 Ed25519 签名和归档摘要，再解压并验证 Manifest、平台、架构和逐文件摘要。

手工安装必须以 root 运行，并显式提供可信验证器及其 SHA-256：

```bash
sudo ./install.sh \
  --management-url https://control.example.com \
  --package-file ./wenzwork-relay-VERSION-darwin-ARCH.tar.gz \
  --checksums-file ./SHA256SUMS \
  --checksums-signature-file ./SHA256SUMS.sig \
  --signing-key-file ./release-signing-public-key.pem \
  --verifier-file ./trusted-relayctl \
  --verifier-sha256 VERIFIER_SHA256 \
  --access-key-stdin
```

升级前先在管理端排空节点并移出外部负载均衡，再运行当前版本的 `scripts/upgrade.sh`。新版本未通过 Ready 检查时脚本会恢复旧版本链接、环境文件和 LaunchDaemon 配置。`uninstall.sh` 默认只移除服务；只有同时使用 `--purge --confirm` 才删除程序和配置。
