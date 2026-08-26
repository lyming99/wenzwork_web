---
title: 使用一键脚本部署 Relay
description: 准备 Relay Access Key，生成跨平台脚本并验证中继节点注册和心跳。
category: 安装与部署
order: 50
updatedAt: 2026-08-23
---

# 使用一键脚本部署 Relay

Relay 负责转发远程连接的加密数据，不读取项目正文。只有需要自建远程数据面或扩展容量时才部署；单纯使用本机工作区不需要 Relay。

## 先在 Host 创建安装记录

使用具备中继管理权限的管理员账户进入 Host 管理后台，在“中继节点”中创建目标节点安装记录。平台和架构必须与目标机一致，并配置两个互不绑定的网络值：Relay 本地 WS 监听端口（默认 `8443`）和客户端实际使用的 `ws://` 或 `wss://` 完整访问链接。保存只显示一次的 Relay Access Key；若 Key 已丢失，应轮换 Access Key，不要猜测或复用日志中的片段。

## 生成一键脚本

1. 打开[软件下载](/download)，选择“Web / 服务端”。
2. 在“一键安装脚本”中选择“Relay 中继服务”。
3. 选择 Linux、Windows 或 macOS，以及 x64/AMD64 或 ARM64。
4. 填写可从 Relay 目标机访问的 Host 管理地址和刚创建的 Relay Access Key。
5. 下载页面生成的 Bash 或 PowerShell 脚本。

页面填写的 Access Key 只在当前浏览器中写入下载文件，不会提交给版本目录 API。脚本会选择匹配的正式包、核对 SHA-256、写入注册配置并后台启动 Relay。

## 网络与安全

Relay 进程始终在管理页指定的端口提供明文 WS，不读取 WSS 证书或私钥。生产环境应由 Nginx 等反向代理监听公网 HTTPS/WSS、保存证书与私钥，并把 WebSocket Upgrade 转发到 Relay 的 WS 端口；随后只需把外部 `wss://.../v2/connect` 链接填回 Host。反向代理端口与 Relay 监听端口可以不同，例如公网 `443` 转发到本机 `8443`。

以下是监听端口为 `8443` 时的最小 Nginx 片段；证书路径、域名和网络访问控制由部署者维护：

```nginx
server {
    listen 443 ssl;
    server_name relay.example.com;
    ssl_certificate /etc/nginx/tls/relay.crt;
    ssl_certificate_key /etc/nginx/tls/relay.key;

    location = /v2/connect {
        proxy_pass http://127.0.0.1:8443;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
    }
}
```

管理地址生产环境也应使用 HTTPS。若明确选择 HTTP，Access Key 与管理通信会以明文经过网络，只能在受控可信网络中使用。脚本不会自动配置 DNS、防火墙、TLS 证书、反向代理或负载均衡。公网 WSS 地址、Nginx 证书主机名和防火墙规则必须与 Host 中的访问链接一致；Relay 的 WS 端口只应向反向代理或受信任内网开放。不要把 Access Key 放入 URL、命令历史、工单或普通日志；安装完成后删除脚本。

## 验证注册

- 目标机上的 Relay 服务或后台进程保持运行；
- Host 管理后台显示安装记录已注册，节点与运行实例持续心跳；
- 节点报告的平台、架构和版本与所选 Release 一致；
- 健康地址可按管理员配置访问；
- 用两台已授权设备建立一次远程连接，确认流量走目标 Relay 且项目内容仍为加密数据。

升级或迁移前先在管理后台执行排空，确认没有活跃连接后再操作。永久下线时先吊销连接权限，再卸载目标机服务。
