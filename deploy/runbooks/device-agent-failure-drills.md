# Device Agent 故障演练手册与记录模板

本手册用于隔离的验收设备。不得在唯一生产副本、个人日常项目或未验证备份的主机上执行磁盘、数据库损坏或强制终止演练。

## 1. 通用准备

- 记录 Release、OS/架构、Agent/客户端版本、设备 ID、开始时间和执行人；不得记录 Access Key、Ticket、路径、命令、Prompt、回复或文件摘要。
- 使用专用测试项目和可丢弃数据卷；先执行安装器健康检查并创建可恢复快照。
- 默认关闭 `terminal.interactive`、`tasks.v2`、`workflow.v2`、`ai.workspaceTools`，每个场景只打开必要能力。
- 预先验证只读文件访问，并准备一键恢复为 `WENZWORK_AGENT_FEATURE_FLAGS=-terminal.interactive,-tasks.v2,-workflow.v2,-ai.workspaceTools`。
- 每次注入故障后观察至少一个完整重连/恢复窗口；结果必须包含实际时间线和日志证据，不能只填“符合预期”。

## 2. 场景矩阵

| 场景 | 注入点 | 必须证明的结果 | 自动化先验 |
| --- | --- | --- | --- |
| 客户端随机断开 | 活动文件流、PTY、任务、AI 流 | 写请求不重复；旧流被取消；重连按水位收敛 | Peer RPC、PTY、Task/AI 恢复测试 |
| Agent 进程崩溃 | 持久化前后、工具运行中 | 不重复启动已接受任务；无遗留进程树；streaming 状态收敛 | Task/AI restart 与 ProcessSupervisor 测试 |
| Relay 进程/网络断开 | 建连、Peer RPC、传输中 | Agent 抖动退避重连；Ticket/epoch 不降级；正文不落 Relay | Relay/Agent reconnect 测试 |
| 磁盘满 | 受配额的独立 Agent 数据卷 | 原子写不产生半文件；返回稳定错误；释放空间后可恢复 | 真实 tmpfs `ENOSPC` 状态回滚、大小上限与安装器恢复暂存写失败测试 |
| BusinessStore 损坏 | 停机后替换专用测试库 | 启动失败关闭；不会新建空库覆盖；恢复快照后身份/数据一致 | `TestBusinessStoreCorruptionFailsClosedAndBackupRestores` |
| 系统休眠/唤醒 | 活动连接与 PTY/AI 流 | 旧连接 epoch 失效；不重放输入；客户端明确重连或 reset | PTY attach/replay 与 reconnect 测试 |
| Wi-Fi/有线/VPN 切换 | DNS、IP 和路由变化 | 重新解析并重连；不跳过 TLS；旧项目流不串线 | TLS URL、reallocation 与 cache isolation 测试 |
| 升级健康检查失败 | 已备份后、切换新版本后 | 二进制、环境和完整数据集一起回滚；失败现场保留 | 三平台脚本契约测试 |

## 3. 安全注入方法

### 随机断开和进程崩溃

通过测试编排器在随机种子固定的时间点终止客户端连接、Relay 测试进程或 Agent 服务。记录种子和稳定 ID，不记录载荷。Agent 服务崩溃场景应由服务管理器重启；进程树检查必须在退出后和恢复后各执行一次。

### 磁盘满

只对专用小容量 VHD/APFS sparse volume/loopback filesystem 设置配额或填充，不得对系统盘或真实项目盘运行填满命令。演练前解析并记录挂载点，确认它只承载测试 Agent 数据根及同级回滚暂存路径；Linux/macOS 不能只把默认数据根本身或其子目录单独挂载，否则安装器会按设计失败关闭。验收至少覆盖主状态原子写、SQLite 事务、SecretStore 更新、任务日志上限、文件 staging，以及升级恢复暂存失败时活动数据保持不变。

### 数据库损坏

停止服务并确认文件句柄关闭，复制完整数据快照，然后只破坏专用测试数据根下的 `.business.sqlite`。预期 Agent 明确启动失败且原损坏文件仍在；恢复完整快照后再启动。不得通过删除数据库让程序静默创建空库来宣称恢复成功。

### 休眠和网络切换

真实设备上保持一个带已确认序列的 PTY、一个持续 AI Mock 流和一个幂等写请求。休眠至少跨过一次心跳周期，唤醒后切换网络。检查输入不会重放、输出序列不倒退、旧 Ticket/connection epoch 不能重新占用路由。

## 4. 通过条件

- 无任务重复执行、终端输入重放、孤儿进程、文件半发布或用户消息静默丢失。
- 身份、BusinessStore、控制状态和 SecretStore 要么全部保持新状态，要么全部恢复到同一快照。
- 文件只读在关闭单项高风险能力后仍可用；设备仍在线并能上报能力。
- 日志、数据库、trace、崩溃报告和演练附件通过隐私扫描，不含禁采正文。
- 恢复步骤在预定 RTO 内完成；任何不可恢复数据、权限绕过或高危泄漏均阻止扩大灰度。

## 5. 单次记录模板

```text
记录 ID：
日期/执行人/复核人：
平台与实体设备型号：
Release / Agent / 客户端版本：
场景与随机种子：
前置快照 ID（不得写路径）：
注入时间线：
观察到的稳定错误码、耗时、队列深度、字节数：
恢复时间线与 RTO：
隐私扫描结果：
数据一致性结果：PASS / FAIL
安全结果：PASS / FAIL
证据位置（受控系统）：
遗留问题与负责人：
```

仓库自动化只能作为真实演练的先验。Windows、macOS、Linux 的休眠、网络切换、服务升级和长稳结果必须由实体设备记录证明。
