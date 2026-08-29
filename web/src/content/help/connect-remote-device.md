---
title: 接入一台远程设备
description: 生成 Device Access Key 和一键安装脚本，让 Windows、Linux 或 macOS 设备安全上线。
category: 远程设备
order: 30
updatedAt: 2026-08-28
---

# 接入一台远程设备

Device Agent 默认主动连接 Host 与分配到的 Relay，不需要在工作设备上开放固定入站端口。也可以显式开启 IP 直连监听器，让工作台绕过 Relay；两种模式使用同一套端到端加密 Link 与远程功能。一份 Access Key 只用于设备交换短期凭证，不会发送给 Relay。

设备接入要求账户具有当前有效的 Pro 会员。每个账号默认最多保留 10 台已接入设备，管理员可以在会员管理后台即时调整该上限；吊销 Access Key 或关闭远程访问不会释放名额，永久删除不再使用的设备后才可重新接入。

## 创建 Access Key

1. 登录 WenzWork，进入“账户中心 → 远程设备”。
2. 打开“设备接入 Access Key”，填写容易辨认的用途名称。
3. 点击“生成 Access Key”。Key 明文只在创建、轮换或同一请求安全重试时显示。

不要关闭结果区域。页面会使用刚生成的 Key 创建一键脚本。

## 下载 Device Agent 脚本

1. 在“远程设备一键安装”中选择目标系统：Windows、Linux 或 macOS。
2. 选择 x64/AMD64 或 ARM64。Windows 常见 Intel/AMD 电脑选择 x64；Apple silicon 和 aarch64 设备选择 ARM64。
3. 点击“下载一键安装脚本”。若按钮不可用，说明当前正式版还没有对应目标的 Device Agent 包。

脚本会下载与正式版本、平台和架构完全匹配的归档，校验 SHA-256，写入当前 Host 地址和一次性设备 Key，然后初始化并后台启动 Agent。

## 在目标设备运行

只把脚本传到准备接入的可信设备。Linux/macOS 先授予当前用户执行权限；Windows 在 PowerShell 中运行。脚本含明文 Access Key，安装成功后立即删除，不要上传到同步盘或作为附件发送。

若 Host 使用自签名 HTTPS，不要关闭 TLS 校验；应把签发 CA 安装到系统信任链，或按部署文档配置专用 CA 文件。HTTP 会明文传输 Access Key 和控制流量，只能用于部署者认可的可信网络。

## 确认设备在线

返回“远程设备”页，确认设备卡片出现并显示“在线”。打开设备后，核对设备名、Agent 版本、项目列表和允许能力。如果设备没有出现：

- 检查目标机 Agent 日志和系统时间；
- 确认 Host 地址可从目标机访问；
- 确认 Access Key 未被吊销或轮换；
- 确认账户 Pro 会员仍在有效期内，且已接入设备数没有达到后台设置的上限；
- 确认下载脚本的平台、架构与目标机一致；
- 使用 HTTPS 时检查证书主机名和 CA 信任。

设备丢失或脚本误泄露时，立即在页面吊销对应 Key；需要继续接入时创建或轮换新 Key，不要复用已暴露的脚本。

## 可选：开启 IP 直连

在受保护的 Device Agent `.env` 中设置以下三项，并把 IP 换成控制端能够访问、且确实属于该设备的单播地址：

```dotenv
WENZWORK_DEVICE_DIRECT_ENABLED=true
WENZWORK_DEVICE_DIRECT_IP=192.168.1.50
WENZWORK_DEVICE_DIRECT_PORT=9443
# 可选：为原生桌面直连单独设置；省略时复用 WENZWORK_DEVICE_ACCESS_KEY
# WENZWORK_DEVICE_DIRECT_ACCESS_KEY=device_replace_with_a_43_character_urlsafe_access_key
```

重启 Agent，确认防火墙允许该 TCP 端口。Agent 成功绑定后会向 Host 上报端点并每 15 秒刷新心跳。随后在“远程设备 → 修改设备”中开启“IP 直连模式”；若端点没有有效心跳，管理端会拒绝开启。

直连端口只接受 remote/v2 二进制 Carrier。网页模式使用 Host 签发的短期 DeviceConnectionGrant；原生桌面可用 Access Key 通过 `POST /v2/direct/device-links` 换取由 Agent 身份签发、有效期 5 分钟且绑定控制器 Ed25519 持钥证明的本地 Grant。Access Key 只允许放在 `Authorization: Bearer` 请求头，不得写入 URL 或日志；两类 Grant 使用独立验签域和连接 registry，不互相回退或顶替，业务内容仍由 Link 层端到端加密。未配置证书时入口使用 `ws://`，HTTPS 管理页会受浏览器混合内容策略限制。要使用 WSS，同时配置 `WENZWORK_DEVICE_DIRECT_TLS_CERT_FILE` 和 `WENZWORK_DEVICE_DIRECT_TLS_KEY_FILE`；证书必须把直连 IP 写入 IP 类型的 subjectAltName，并受控制端浏览器信任。

下一步阅读[使用远程项目工作区](/help/remote-workspace)。
