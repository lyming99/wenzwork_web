# 添加与激活 Relay 节点 Runbook

1. 管理员完成 MFA 后打开 `/admin/relay`，选择目标 Cell 和已发布的 Linux/amd64 Release。
2. 创建稳定 Installation。主机重启只产生新 Instance，不创建第二条 Installation。
3. 选择安装模式并创建 Install Session；随后生成一次性 Token。离开页面后明文不可恢复。
4. 在目标 Linux 主机执行本地安装。控制面不要求 SSH，也不会发送远程 Shell 命令。
5. 等待管理页显示首个心跳，核对 Installation ID、Cell、Release、协议版本、地址、能力和新 Instance ID。
6. 在主机执行 `sudo relayctl identity show`，与页面指纹逐字核对。任何不一致都必须 Revoke，不能激活。
7. 运维验证管理页指定的 Relay 端口只提供 WS；需要 WSS 时在 Nginx/LB 配置证书与私钥并转发到该端口，再核对 DNS、外部 WSS 链接与 Cell 归属并逐项勾选。注册成功不能自动勾选这些项目。
8. 输入页面要求的精确确认文本并激活。随后一次心跳才会返回 Routing Ready。

验证节点重启语义：

```bash
before=$(sudo relayctl status)
sudo systemctl restart wenzwork-relay.service
sudo /opt/wenzwork-relay/current/scripts/healthcheck.sh --ready --wait 60
after=$(sudo relayctl status)
printf '%s\n%s\n' "$before" "$after"
```

核对 Installation ID 不变、Instance ID 改变，旧 Instance 在 Lease 窗口后为 Offline。不要把节点进程重启误报为新增服务器。
