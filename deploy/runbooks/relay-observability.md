# Relay 可观测性与告警 Runbook

Relay 从受限健康端口的 `/metrics` 暴露注册、Host 心跳、连接、握手、Route 协商拒绝、队列、带宽和 Drain 指标。Host 的 `/api/v1/health/ready` 与 `/api/v1/health/remote-ready` 分别反映基础依赖和远程链路依赖。指标不得带用户、设备、Installation、Instance 或凭据标签。

将 [`relay-alerts.yaml`](../monitoring/relay-alerts.yaml) 加入 Prometheus rule files，并将 [`relay-dashboard.json`](../monitoring/relay-dashboard.json) 导入 Grafana。健康端口只允许监控专网、mTLS 代理或主机采集器访问。

## RelayNode not registered / heartbeat stale

检查管理端、系统时间、DNS/TLS、管理端保存的主机运行配置和经过脱敏的 `systemctl status wenzwork-relay`。确认包含 `RELAY_ACCESS_KEY` 的环境文件存在且权限正确，但不要输出值。私有化部署还需检查可选的 `RELAY_MANAGEMENT_URL` 覆盖。Lease 到期后管理端不得向客户端返回该实例；进程重启必须产生新 Instance ID。

## Relay routing not ready

若 Relay 已注册但 `wenzwork_relay_routing_ready` 持续为 0，检查 Host 的远程能力健康、Redis、防重放存储以及 Relay 数据面探针。修复后确认心跳成功、常驻 Route 被 Host 接受并可从管理端解析，再恢复流量。

## Relay route negotiation rejected

持续增长的 `wenzwork_relay_route_rejected_total` 表示 Host 拒绝 Relay 上报的常驻 Route。核对设备 Grant 状态、当前 Assignment/Cell、Connection Epoch 和 Relay Instance 绑定。撤权或迁移引起的一次拒绝是正常收敛；持续拒绝通常表示 Agent 未按 `GoAway` 刷新分配，或 Host 与 Relay 的运行版本不一致。

## Peer direct-connect failures

按顺序检查：目标 Route 是否存在且未过期、目标 Connection Epoch 是否仍一致、实例 Lease/状态是否有效、数据库中的公网地址是否为严格 `wss://.../v1/connect`、公网 DNS/TLS/防火墙是否能从控制设备访问。`wrong_relay` 表示客户端连接的 Node/Cell 与 Ticket 不一致；`target_offline` 表示目标已经断线或迁移，应重新向管理端申请 Ticket，不能复用或篡改旧 Ticket。

## Relay queue pressure

检查慢消费者、直连 WSS 带宽和写循环延迟。每连接队列固定为 4 MiB/256 帧，不能临时改为无界；必要时发送 GOAWAY，让客户端重新查询目标 Relay 并按退避重连。

## Relay v2 global queue pressure

检查 `wenzwork_relay_v2_queue_bytes` 与全局预算的比例以及拒绝计数。Bulk 最多使用全局预算的 75%，Interactive 与 Bulk 合计最多使用 87.5%，剩余容量保留给 Ping、GOAWAY、Resume 和 Link 控制帧。持续拒绝时先限制新握手并定位慢消费者；不得临时取消全局预算。

## Relay v2 Link route pressure

检查客户端是否在断线后不断创建新的 Link。每个 Controller/Device 组合只能保留一个当前 Link，断开的客户端 Link 在五分钟恢复窗口后应被回收；接近 65,536 条硬上限说明恢复反馈或版本兼容存在异常。确认 Agent 能处理 Link 级 `RESUME_EXPIRED`，再逐节点 Drain。

## Relay write loop lag

持续超过一秒表示下游 Socket、宿主机网络或调度器已拥塞。结合队列水位、出口带宽和连接数判断影响范围；优先隔离慢消费者，并确认心跳/控制优先级仍能前进。

## Relay handshake failure burst

核对客户端版本、子协议、Ticket 有效期、节点/Cell/Epoch 绑定、系统时间和 TLS。只按稳定失败原因聚合，不记录 Ticket、身份公钥或客户端标识。发布期间若失败突增，停止继续放量并回滚最近节点。

## Relay rate limit sustained

持续限流通常表示同步重连风暴或异常接入。确认客户端采用带下限的全抖动并遵守 `Retry-After`/GOAWAY；必要时降低新分配速率，不能通过扩大无界队列吸收峰值。

## Relay Drain stalled

确认 Agent 与 Client 已收到 GOAWAY、停止新业务并在服务端给出的最小延迟后重连。超过 Drain 截止仍存在连接时，记录剩余数量和原因后按发布手册处置；不要直接清除 Route 绕过 Epoch/Fence 校验。

## 验收记录

告警和 Dashboard 只证明仓库内契约存在，不替代公网多节点实机证据。至少两台 Relay、24 小时长稳、两倍容量、重连风暴、目标迁移和 RPO/RTO 必须记录在 [`relay-acceptance.md`](relay-acceptance.md)。
