# Relay 实机验收记录模板

此文件是执行模板，不是已完成证据。只有在至少两台具有不同公网 WSS 地址的真实或虚拟 Relay 主机、PostgreSQL 和 Redis 上运行，并附时间戳和原始结果后，才能签署上线。

## 环境

| 字段 | 实际值 |
| --- | --- |
| 变更/测试编号 | 待填写 |
| Git commit / Release / Key ID | 待填写 |
| Relay A 主机、Node、Cell、WSS | 待填写 |
| Relay B 主机、Node、Cell、WSS | 待填写 |
| 执行人、复核人、日期 | 待填写 |

## 必须附证据

- [ ] 两台 Relay 使用各自 `.env` 和可撤销 Access Key 注册；重启后 Installation 不变、Instance ID 更新并自动重连。
- [ ] 目标设备常驻 Relay B，控制设备常驻 Relay A；管理端返回 Relay B 的精确 `relayUrl`、Node、Cell、目标 Epoch 和短期 Peer Ticket。
- [ ] 控制设备不经 Relay A，直接向 Relay B 建立第二条 WSS，两个方向各传输至少 100 条有序 E2EE 帧。
- [ ] 直连控制连接不会覆盖控制设备在 Relay A 的常驻 Redis Route。
- [ ] 旧 Ticket 重放、错 Node/Cell、错来源密钥、目标断线或 Epoch 变化均被拒绝。
- [ ] 轮换/吊销 Access Key 后旧 Key 不能注册；已吊销且离线的主机可删除。
- [ ] Drain/Resume/Revoke 通过心跳期望状态收敛，不依赖消息总线。
- [ ] Redis 清空/不可读、PostgreSQL 切换时新握手和签发失败关闭，恢复后投影与 Route 正确重建。
- [ ] 24 小时长稳、两倍目标连接、30%～50% 重连风暴；记录 P50/P95/P99、错误率、内存和队列曲线。
- [ ] 公网 WSS 证书、SSRF/横向越权、日志与配置 Secret 扫描无阻塞项。

## 签署

产品、运维、安全和发布负责人分别填写“通过/阻塞”、姓名和日期。任何待填写、模拟数据或未附原始证据的项目均视为未通过。
