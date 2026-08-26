# Relay Drain 与故障摘除 Runbook

## 计划内 Drain

1. 记录 Installation、Instance、Cell、公网 WSS 地址、当前连接数和变更窗口。
2. 从管理端发起 Drain，并观察 Operation；不在控制面执行远程 Shell。
3. Relay 在下一次心跳取得 `draining` 期望状态，拒绝新连接并向已有连接发送 GOAWAY。
4. 从公网 DNS/LB 的节点定向配置中摘除该实例，等待连接降至零或达到批准的截止时间。
5. 升级或停止服务。恢复后确认新 Instance ID、新 Lease、公网 WSS 精确可达，再 Resume。

## 非计划故障

- 进程退出：systemd 自动重启并复用 `.env` 中的 Access Key；新进程生成新 Instance ID，旧 Lease/Route 自然过期。
- 主机失联：立即从公网入口摘除，等待 Lease 过期；不得伪造心跳或延长 Redis TTL。
- 目标设备迁移：正在使用旧 Ticket 的控制连接会断开。客户端重新查询管理端，取得新 Relay 地址、目标 Epoch 和 Ticket 后直连。
- 整个 Cell 故障：通过可审计的 Assignment Operation 迁移到允许的 fallback Cell，不直接改 Redis。
- Redis 不可读：新握手和新 Peer Ticket 失败关闭；恢复后由 PostgreSQL 权威事实重建投影。

强制截止必须记录原因、受影响连接、Operation ID 和操作人。删除 Installation 不能替代 Drain；只有已吊销且离线的主机记录才允许删除。
