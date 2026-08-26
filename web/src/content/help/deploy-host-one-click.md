---
title: 使用一键脚本部署 Host
description: 在下载页生成匹配系统和架构的 Host 脚本，完成安装、首次登录与安全收尾。
category: 安装与部署
order: 20
updatedAt: 2026-08-23
---

# 使用一键脚本部署 Host

Host 提供网站、账户、管理后台和远程控制面。下载页生成的一键脚本会选择正式发布包，核对 SHA-256，写入初始配置并后台启动服务。

## 部署前准备

- 准备 64 位 Linux、Windows 或 macOS 主机，并确认处理器是 AMD64/x64 还是 ARM64；
- 若由 Host 自动准备 PostgreSQL 与 Redis，目标机需要可用的 Docker；
- 决定首次访问地址、监听端口和初始管理员邮箱；
- 公网生产环境还需要自行准备域名、DNS、TLS 证书、反向代理和防火墙规则。

## 生成脚本

1. 打开[软件下载](/download)，选择“Web / 服务端”。
2. 在“一键安装脚本”中选择“Host 服务端”、目标系统和处理器架构。
3. 填写监听范围、Host 端口、首次访问地址和初始管理员邮箱。
4. 使用页面生成的高强度密码，或填写一个 8–128 字符且不含换行的密码。
5. 点击“下载 Bash 一键安装脚本”或“下载 PowerShell 一键安装脚本”。

密码只在当前浏览器中写入下载文件，不会提交给版本目录 API。页面会在生成后再次显示首次登录凭据；请立即保存，且不要把脚本提交到 Git、聊天记录或公开工单。

## 在目标机运行

Linux 或 macOS：

```bash
chmod 700 ./wenzwork-host-install-*.sh
./wenzwork-host-install-*.sh
```

Windows PowerShell：

```powershell
Set-ExecutionPolicy -Scope Process Bypass
$installer = Get-ChildItem .\wenzwork-host-install-*.ps1 | Select-Object -First 1
& $installer.FullName
```

脚本会拒绝摘要不一致或包含危险路径、链接和特殊文件的归档。默认安装目录位于脚本旁；不要在已有同名安装目录上直接覆盖。安装成功后删除含凭据的脚本。

## 首次登录与初始化

打开生成脚本时填写的首次访问地址，用保存的管理员邮箱和密码登录。首次登录会进入系统初始化页；按页面完成正式站点地址、数据库、Redis、邮件、允许来源、注册开关和 Release 仓库配置。

安全 Cookie 与管理员 MFA 是独立开关。只有正式 HTTPS 已可用时才开启安全 Cookie；开启后应始终从配置的 HTTPS 地址登录。初始化完成并按页面提示重启 Host 后，管理员明文密码会从 `.env` 清除。

## 验证结果

- 首页、登录页和 `/api/v1/health/ready` 可访问；
- 能登录账户中心，管理员可进入系统设置；
- PostgreSQL、Redis 和 Host 进程保持运行；
- 目标机重启后，所选后台服务方式能恢复 Host；
- 安装脚本已经删除，`.env` 与 `cache/host-secrets/` 已纳入加密备份。

需要连接工作设备时，继续阅读[接入远程设备](/help/connect-remote-device)。
