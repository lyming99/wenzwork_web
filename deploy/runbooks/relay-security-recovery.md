# Relay 安全、凭据与灾难恢复 Runbook

## Access Key 轮换、吊销和删除

管理端生成 Relay Access Key 时明文只显示一次，数据库只保存摘要。轮换后更新 `/etc/wenzwork-relay/relay.env` 并重启；文件保持 `root:wenzwork-relay 0640` 或更严格。吊销后 Relay 在下一次心跳收到 `revoked` 并退出，之后同一 Key 的注册也会永久失败。确认节点离线后，可以从管理页删除已吊销主机记录。

不得把长期 Relay Access Key 返回给设备。设备直连目标 Relay 只能使用管理端签发的短期 Peer Session Ticket；Ticket 不得进入 URL、日志、Trace、工单或崩溃转储。

## PostgreSQL 恢复

PostgreSQL 是 Installation、实例地址、Assignment、Operation、Access Key 摘要和 Relay 生命周期审计事件的权威来源。恢复到隔离环境后校验唯一约束、单调版本和审计完整性，再切换。不得从 Redis 反向覆盖 PostgreSQL；记录实际 RPO/RTO。

## Redis 清空或切换

先让 Peer Ticket 签发和目标 Route 解析失败关闭。Redis 恢复后无需投影 Assignment/Grant Fence：仍存活的 Relay 会立即或在下一次心跳把常驻连接快照发给 Host，Host 按 PostgreSQL 权威状态重新发布接受的 Route；设备重连也会触发同样流程。Route 写入继续按 Connection Epoch 和 Connection ID 做单调比较，旧连接不得覆盖或删除新连接。

## 目标 Relay 公网地址故障

将故障实例 Drain 或 Revoke，并从节点定向 DNS/LB 中摘除。不要把一个随机共享 WSS 地址伪装成精确节点地址。目标设备在新 Relay 建立连接后，控制设备重新向管理端会合；旧 Node/Epoch Ticket 必须拒绝。

## 证据与敏感信息扫描

每次演练扫描 journal、Trace、诊断包和配置，确认不存在 Access Key、Enrollment Token、Ticket、Authorization、设备正文或私钥。诊断包只允许包含状态、systemd 属性和脱敏日志；发现泄漏先吊销对应 Key/Ticket 签名密钥，再按安全事件流程保全证据。
