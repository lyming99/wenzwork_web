# Relay 外部 LB、DNS 与 TLS Runbook

Cell 的公网 Endpoint、DNS、证书和 LB 均由外部基础设施负责。Relay 只提供指定端口的明文 WS；Nginx/LB 负责保存证书、终止 WSS 并转发 WebSocket Upgrade。管理应用只记录外部访问链接和检查结果，不接收证书或私钥，也不直接修改云 LB 或 DNS。

## 加入节点

1. 确认 Installation 已注册、有新鲜心跳且本地指纹匹配，但保持未激活。
2. 从 Nginx/LB 所在网络直连管理页指定的 Relay 端口，验证明文 WS Upgrade；后端不得要求 TLS。
3. 在 Nginx/LB 前端验证 TLS 版本、WSS Upgrade、证书链和 SNI，再将后端以零权重或禁用状态加入。健康探针使用独立的本地健康地址，不应把数据面路径误当成 `/health/ready`。
4. 完成 DNS 和公网证书检查，把客户端实际使用的 `wss://.../v2/connect` 地址保存到管理端，并确认 Endpoint 仍属于目标 Cell，避免跨 Cell 误配。
5. 在管理端勾选 LB/DNS/Port/TLS，激活 Installation；等待 `/health/ready` 成功后逐步提高权重。

## 摘除节点

1. 在管理端把节点置为 Draining，确认拒绝新连接并发送 GOAWAY 的版本语义。
2. 将 LB 权重设为零并摘除后端；等待 LB 连接耗尽和本机活跃连接降到门限。
3. 才可停止、升级或卸载 systemd 服务。

一个 Cell 在任何时间都必须保留至少一台已验证的服务节点；禁止同时升级或摘除同一 Cell 的全部主机。若后端 WS、前端 WSS 或证书校验失败，节点保持流量未就绪，不得通过仅改数据库状态绕过。
