# WenzWork Web

WenzWork 官方网站、账户/会员中心与管理后台。前端使用 Vue 3、TypeScript 和 Vite，后端使用 Go、Gin、GORM 与 PostgreSQL。

文档总览见[《文档目录》](docs/文档目录.md)，当前开发任务见[《开发计划》](docs/项目规划/开发计划.md)。

## 工程目录

- `web/`：官网、账户中心和管理后台。
- `server/`：统一提供 Web 静态资源与 REST API 的 Go 服务、业务逻辑和 SQL 迁移。
- `api/`：OpenAPI 契约。
- `api/remote/v1/`：远程设备 WebSocket 的版本化 Protobuf 契约。
- `deploy/`：反向代理和生产部署配置。
- `docs/`：按项目规划、架构设计、开发方案、部署运维、测试验收、架构决策和开发笔记分类的中文文档。

## 开发要求

- Node.js 24 LTS。
- Corepack + pnpm 11.15.1。
- Go 1.26.5；`go.mod` 会请求对应工具链。
- Docker Desktop / Docker Compose。

## 本地启动

1. 复制 `.env.example` 为 `.env`，只填写默认管理员邮箱和密码；本地 PostgreSQL、Redis、Mailpit 及运行参数使用开发默认值，Host 会自动生成本地应用密钥。
2. 启动开发依赖：`docker compose -f docker-compose.dev.yml up -d`。
3. 安装前端依赖：`corepack pnpm install`。
4. 执行迁移：进入 `server` 后运行 `go run ./cmd/migrate up`。
5. 启动 API：仍在 `server` 中运行 `go run ./cmd/api`。
6. 启动前端：回到根目录运行 `corepack pnpm dev:web`。

前端默认地址为 `http://localhost:5173`，API 默认地址为 `http://localhost:8080`，Mailpit 为 `http://localhost:8025`，MinIO 控制台为 `http://localhost:9001`。

## 首个管理员

迁移完成且数据库还没有超级管理员时，在根目录 `.env` 填写 `SYSTEM_ADMIN_EMAIL` 与 `SYSTEM_ADMIN_PASSWORD`。API 每次启动都会先检查数据库：若尚无超级管理员，则按这两个默认字段和可选的 `SYSTEM_ADMIN_DISPLAY_NAME` 幂等创建首个账号；若已有管理员，则不会覆盖账号、密码或权限。空库却缺少有效默认凭据时，API 会在监听端口前明确失败。也可在 `server` 目录手工执行同一套单次引导：

```powershell
$env:BOOTSTRAP_ADMIN_EMAIL='admin@example.com'
$env:BOOTSTRAP_ADMIN_PASSWORD='<使用独立的 8 字符以上强密码>'
$env:BOOTSTRAP_ADMIN_DISPLAY_NAME='WenzWork 管理员'
go run ./cmd/admin bootstrap
```

该命令和 API 启动门禁都只允许创建第一个 `super_admin`，不会覆盖现有账户或继续提升第二个账户。便携 Host 包的 `start.sh` / `Start.ps1` 仍会在启动 API 前执行同一引导，直接运行 API 或本地测试包也无需再手工创建管理员。首次登录后应在 `/admin/setup` 完成系统配置。`ADMIN_MFA_REQUIRED` 与 `COOKIE_SECURE` 均为显式选项且默认关闭，`APP_ENV=production` 不会替管理员自动开启；启用前者后，管理员需在 `/account/security` 配置 TOTP。手工引导完成后从终端环境中移除 `BOOTSTRAP_ADMIN_PASSWORD`。启用 `COOKIE_SECURE` 时必须从配置的 HTTPS 地址登录；Host 会拒绝从 HTTP 页面创建一个浏览器无法保存的安全会话，前端也会在进入账户页前通过 `/me` 确认会话 Cookie 已生效。

## 管理后台

- `/admin/users`：创建账户、搜索账户、禁用/启用账户，以及设置或取消 Pro 会员权限。
- `/admin/analytics`：按今日、近 7 日、近 30 日或自定义范围查看按小时/按日的独立 IP 与下载次数趋势、来源网站与直接访问、同日下载率与注册率、最近 20 个新增访问 IP、地区、热门路径及账户登录记录；仅具备 `admin.audit.read` 权限并完成 MFA 的管理员可见。
- `/admin/releases`：维护软件版本、更新公告、安装文件列表、发布/下架状态，并可永久删除版本记录。
- `/admin/redemption-codes`：批量创建兑换码、一次性导出明文、查看逐码状态，并删除未使用的兑换码。
- `/admin/help-documents`：维护帮助文档草稿，确认发布安全 HTML 静态快照，并归档旧文章。
- `/admin/feedback`：筛选会员反馈，维护处理状态、会员可见回复与内部备注。

所有后台接口都在服务端执行 RBAC 校验；写操作还要求同源校验、CSRF Token 并记录审计日志，显式启用管理员 MFA 门禁后还会校验二次验证保证级别。

会员登录后可在 `/account/feedback` 提交反馈并查看处理进度。管理端 CLI 的认证、命令和 JSON 输入格式以命令行 `--help` 和 [`api/openapi.yaml`](api/openapi.yaml) 为准。
桌面客户端网页登录、设备授权轮询、30 天滚动续签和 Pro 权益检测协议以 [`api/openapi.yaml`](api/openapi.yaml) 及对应服务端实现为准。

## 远程管理与端到端加密数据面

远程设备、任务、Peer Query 与 E2EE 文件传输的目标架构见[《远程管理系统架构》](docs/架构设计/远程管理系统架构.md)。仓库保留 `remote-poc` 作为协议回归程序，同时已实现可运行的 `device-agent`、设备 Access Key、浏览器控制身份、项目/任务投影和设备端 AI/文件 RPC；控制端接入、直接 Relay 连接和 E2EE 握手见[《Client 与 Device 长连接加密通信接入指南》](docs/架构设计/客户端与设备长连接加密通信接入指南.md)，设备部署步骤见[《设备端接入与远程控制指南》](docs/部署运维/设备端接入与远程控制指南.md)。阶段 0 的历史边界与尚未承诺的生产容量见[《远程管理概念验证范围》](docs/项目规划/远程管理概念验证范围.md)。

POC 覆盖两 Cell、每 Cell 两 Node 的动态分配与迁移、Ed25519 短期 Ticket、设备持钥证明、Assignment/Grant/Connection Epoch 栅栏、100 次重复命令只执行一次、Protobuf WSS 握手、文件 X25519/HKDF/XChaCha20-Poly1305 固定向量，以及文件块只走 Relay mTLS Interconnect 的协议约束。运行：

```powershell
cd server
go run ./cmd/remote-poc
```

浏览器远程控制和 Relay 管理 API 使用 `/api/v1/remote/**` 与 `/api/v1/admin/relay/**`；设备认证、分配和同步 API 使用 `/v1/device/**`。Relay 设备连接使用 Cell 独立入口 `/v1/connect`，不经过网站 API 网关。所有 REST 路径以 [`api/openapi.yaml`](api/openapi.yaml) 为契约源。

## Relay 接入（MVP + 生产化开发中）

宿主机部署主干已包含 PostgreSQL 权威模型、管理端生成且可撤销的 Relay Access Key、`.env` 直连与自动重连、兼容的一次性 Enrollment/mTLS Directory、Redis Route/Fence/Capacity、目标 Relay 直连 WSS 数据面、可恢复 Operator/Outbox、Node/Cell Drain、Assignment 迁移、管理后台、签名 Release、`relayctl`、平台原生服务脚本与低基数监控。官方 Relay Release 矩阵覆盖 `linux`、`windows`、`darwin` 与 `amd64`、`arm64` 的六种组合；Linux 使用 systemd，Windows 使用 `WenzWorkRelay` Windows Service，macOS 使用 `com.wenzwork.relay` LaunchDaemon。安装和升级入口分别位于 [`deploy/relay`](deploy/relay)、[`deploy/relay/windows`](deploy/relay/windows) 和 [`deploy/relay/darwin`](deploy/relay/darwin)，管理页默认 Linux/amd64，并只会生成与安装记录平台、架构完全匹配的一键部署命令和可下载脚本。Relay 管理与包下载地址允许部署者选择 HTTP 或 HTTPS；生产公网仍建议使用 HTTPS。

Relay 进程只在管理页指定的本地端口提供明文 WS，不直接保存或加载 WSS 证书/私钥。公网 WSS 由部署者在 Nginx 或等价反向代理配置 TLS 并转发到该 WS 端口；管理页只保存 Relay 监听端口和客户端实际使用的 `ws://` / `wss://.../v2/connect` 链接，两者端口无需相同。

Access Key 只在生成时返回明文，数据库仅保存 SHA-256 摘要；主机或进程重启后复用受平台权限保护的环境文件自动连接，管理端吊销权限后 Relay 停止重试，已吊销主机可从管理页删除。Host 在配置 Redis 后默认启用远程能力，并在 `cache/host-secrets/` 自动创建两把相互独立的 Ed25519 连接签名密钥；无需再配置 `REMOTE_MVP_*`、`RELAY_TICKET_*` 或 `RELAY_DEVICE_LINK_GRANT_*`。控制设备先向账户中心查询目标设备当前所在 Relay，再携带短期凭证直接连接目标 Relay；Relay 间不转发正文，也不依赖 NATS。Redis 不可用或自动密钥损坏时 Device API 保持失败关闭，网站其他能力继续服务。

测试客户端位于 `server/cmd/relay-client-test`，运行 `corepack pnpm build:relay-client-test` 可交叉构建 Windows amd64、Linux amd64 和 Darwin arm64 三个平台。`run` 模式验证单客户端握手与 Ping/Pong；`peer` 模式会让两个独立设备完成 `PEER_OPEN → PEER_READY`，并使用 X25519/HKDF/XChaCha20-Poly1305 在两个方向各发送至少 100 条密文：

```powershell
cd server
go run ./cmd/relay-client-test peer `
  --control-url https://control.example.com `
  --state-file .\source-state.json `
  --target-state-file .\target-state.json `
  --message-count 100
```

两个状态文件必须不同；交互式 OAuth 或可选的 Access Token 仅用于测试过程且不会写入输出。`peer` 模式会保持目标设备连接，在取得账户中心返回的精确 `relayUrl`、Relay Node/Cell 与目标连接 Epoch 后，建立第二条直连 WSS 完成双向传输。可复用验收记录见[《中继服务最小版本验收记录》](docs/测试验收/中继服务最小版本验收记录.md)。当前实现已覆盖可靠的类型化任务命令和实时 Peer 密文转发；多节点公网实机、跨 Relay 大文件流、故障切换和长稳容量仍是生产上线门禁，不在本地功能完成度中作容量承诺。

完整计划与逐项真实状态分别见[《中继节点管理与主机部署计划》](docs/开发方案/中继节点管理与主机部署计划.md)和[《中继节点管理实现状态》](docs/项目规划/中继节点管理实现状态.md)。

## Release 服务端包

需要一次性生成 Host、Relay、Device Agent 的 Linux/Windows/macOS × amd64/arm64 共 18 个便携部署包时，使用 [`scripts/Build-DeploymentPackages.ps1`](scripts/Build-DeploymentPackages.ps1)；按系统与组件的安装步骤见[《WenzWork 安装部署》](docs/安装部署/README.md)，包结构、hash 校验、容器卷备份、升级、私有仓库 Token 和发布流程见[《Host、Relay 与 Device Agent 跨平台部署包》](docs/部署运维/跨平台部署包.md)。

Linux 服务端 Release 包同时包含 Go 程序、`web/` 前端产物、`init_server.sh`、`start_server.sh`、`stop_server.sh` 与 `upgrade_server.sh`，生产服务器不需要 Node.js。归档不携带 `.env`；首次初始化会从 `.env.example` 创建它并提示填写默认管理员邮箱、密码，内部初始化状态保留默认值。未提供 PostgreSQL/Redis 时，脚本用随机凭据启动仅监听回环地址的 Docker 容器。Host 会把应用密钥、远程签名密钥及 Relay CA 自动生成到 `cache/host-secrets/`，迁移临时数据库、创建首个超级管理员并启动页面：

```bash
./init_server.sh             # 缺少 .env 时从模板创建并提示填写
$EDITOR .env                  # 只填写 SYSTEM_ADMIN_EMAIL / SYSTEM_ADMIN_PASSWORD
./start_server.sh start       # 自动准备 PostgreSQL/Redis、迁移、创建管理员并启动
sudo ./configure_server_memory.sh       # 2 GiB 主机：配置内存、Swap 和 systemd
sudo ./start_server.sh status
sudo ./start_server.sh restart
sudo ./stop_server.sh         # 通过 systemd 优雅停止服务
sudo ./upgrade_server.sh      # 拉取、校验、备份、升级并重启最新 Release
```

`configure_server_memory.sh` 为 2 GiB 主机配置 `GOMEMLIMIT=256MiB`、`MemoryHigh=384M`、`MemoryMax=512M`，在主机没有活动 Swap 时创建并持久化 1 GiB `/swapfile`，最后安装 systemd 服务；脚本可重复执行且会先备份被修改的配置。已有 Swap 或需要自定义时可查看 `--help`。其他内存规格可以直接执行 `sudo ./start_server.sh install-systemd`，并按实际峰值调整 `.env`。

`install-systemd` 根据当前安装目录生成 `wenzwork-api.service`，默认使用安装目录所有者运行服务；需要指定专用用户时，在安装前设置 `WENZWORK_SERVICE_USER` 与可选的 `WENZWORK_SERVICE_GROUP`。服务由内核 OOM Kill 或异常退出后会在 3 秒后自动拉起，并通过 `ExecStartPre` 在正常启动和升级后执行迁移。安装完成后，`start`、`stop`、`restart`、`status` 和 `upgrade` 会自动识别并使用 systemd，不再创建 nohup/PID 文件；没有 systemd 的环境仍兼容原有独立进程模式。服务日志使用 `journalctl -u wenzwork-api.service` 查看，`systemctl show wenzwork-api.service -p MainPID -p NRestarts -p MemoryCurrent -p MemoryPeak` 可查看重启和内存状态。完整巡检、升级、日志、OOM 和回滚操作见[《生产环境维护手册》](docs/部署运维/生产环境维护手册.md)。

首次登录会自动进入 `/admin/setup`，由超级管理员配置正式站点 URL、PostgreSQL、Redis、系统邮箱、允许来源、注册开关、会话 Cookie 的 Secure 属性、管理员 MFA 门禁和 GitHub 仓库。Host 先确认 PostgreSQL/Redis 连通，并向默认管理员邮箱实投测试邮件；随后才迁移目标数据库、创建同一管理员并原子写回安装目录 `.env`，清空管理员明文密码并要求重启。HTTPS 地址会切换到生产模式，但安全 Cookie 与管理员 MFA 仍以页面中的独立勾选为准。密码哈希、静态页面、缓存路径、远程区域和签名参数继续使用安全默认值，特殊场景仍可通过高级环境变量覆盖。随包 Caddy 配置把请求反向代理到 `127.0.0.1:8080`；将 Caddyfile 第一行的 `:8088` 替换为正式域名即可让 Caddy 管理 HTTPS。

访问统计只信任 `TRUSTED_PROXY_CIDRS` 中反向代理提供的 `X-Forwarded-For`；默认值仅包含本机 Caddy。页面访问会更新 IP 的首次/最后访问时间和累计 PV，成功获取下载地址或开始代理下载时记录下载 IP，新账户首次注册时记录注册 IP。下载率与注册率以所选范围内的独立访问 IP 为分母，并按 `Asia/Shanghai` 自然日判断同一 IP 是否发生下载或注册；迁移会回填既有访问 IP，但下载与注册转化只能从本功能部署后开始准确累计。

写入访问、下载、注册与登录事件时不会同步请求归属地服务；管理员打开访问统计或登录记录后，服务端才解析当前结果中的 IP，并把成功结果缓存到 `ip_geolocation_cache` 30 天。解析优先使用 `GEOIP_CITY_DATABASE_PATH` 配置的本地 MaxMind GeoLite2/GeoIP2 City `.mmdb`，未命中时依次请求 Toolshu HTTPS 接口与 ip-api.com 免费 HTTP 接口，任一服务不可用会自动切换；失败结果短暂缓存，管理页面继续显示“未知”。设置 `IP_GEOLOCATION_API_ENABLED=false` 可完全关闭第三方查询。ip-api.com 免费接口仅允许非商业用途且不提供 HTTPS，生产使用前应确认其许可与隐私要求，或仅启用本地 MaxMind 数据库。

首次启动不要求 SMTP 或 S3；首次登录的系统设置页会用候选 SMTP 配置向默认管理员邮箱实投测试邮件，投递失败时不会通过初始化或写入 `.env`。S3 仍属于可选高级配置，默认使用 Release 资产。首次系统配置成功后，Host 自动清空 `.env` 中的 `SYSTEM_ADMIN_PASSWORD`；再次启动会通过数据库状态识别现有管理员。

在 Windows 命令提示符中，只需提供版本号即可完成 18 个部署包的交叉编译、验包、tag 推送和正式 GitHub Release 发布：

```bat
build_and_publish -v v0.2.4
```

PowerShell 可执行 `.\build_and_publish.cmd -v v0.2.4`，或直接执行 `pwsh .\scripts\Build-And-Publish-Release.ps1 -v v0.2.4`。入口会规范化可省略的 `v` 前缀，并先查询该 tag 是否已有正式 Release：已发布版本只读核对 tag commit、Release Manifest、校验表及 18 个归档的远端大小与 SHA-256，全部一致后幂等返回，不会提交当前改动、重建或覆盖资产；新版本要求当前分支为 `main` 且已经包含最新 `origin/main`，自动暂存全部未忽略改动、创建 `发布 WenzWork <版本>` 提交并推送到 `origin/main`，确认远端包含该提交后再完成全量构建、验包、tag 和正式 Release 发布。合并冲突、远端领先/分叉、提交钩子产生新改动或推送失败都会在构建前停止。构建清单使用 Git commit 时间，避免相同提交重建时因当前时间导致摘要漂移。该命令会提交当前工作区，只能用于确认准备发布的源码，不能用于普通开发构建。

Unix 便携包在线升级会依次从 `https://work.wenzflow.com`、`https://wenzwork.com` 的公开版本目录取得匹配当前组件、平台和架构的受控下载地址及 SHA-256，两个站点都不可用时才回退到构建包记录的公开 GitHub Release 直链，避免匿名 GitHub API 限流。三处均无匹配包时才报告升级失败。私有仓库才需要在 `.env` 追加高级设置 `GITHUB_ACCESS_TOKEN`；也可以同时用 `GITHUB_RELEASE_REPOSITORY` 覆盖仓库。`WENZWORK_OFFICIAL_RELEASE_BASE_URL` 仅用于私有镜像等显式覆盖场景，设置后会替代内置的两个官网地址：

```dotenv
GITHUB_RELEASE_REPOSITORY=lyming99/wenzwork_web
GITHUB_ACCESS_TOKEN=github_pat_替换为服务器升级专用Token
WENZWORK_OFFICIAL_RELEASE_BASE_URL=https://releases.example.com
```

私有仓库 Token 建议使用只授予目标仓库 `Contents: read` 的 Fine-grained Token，公开仓库可以留空。Token 写入权限为 `600` 的临时 curl 配置，不出现在命令参数、日志或 API 服务进程环境中；systemd 单元也不会直接加载整个 `.env`，而是通过隔离的 `migrate`/`run` 入口移除部署 Token 后再启动程序。升级脚本会持续输出来源尝试、校验、解压、备份、停止、安装、恢复运行状态等阶段百分比，并让 curl/wget 显示文件传输进度。下载完成后脚本验证 Release 中的 SHA-256，并在服务仍在线时完成解压校验和备份；只有替换文件时才停止当前托管实例。安装后通过同一 systemd 单元执行迁移、启动和健康检查，不会与 nohup 产生双进程。离线升级可执行 `./upgrade_server.sh <服务端安装包> [SHA256SUMS 文件]`；`./start_server.sh upgrade ...` 继续作为兼容入口。误拼写的 `upgrage_server.sh` 也随包提供，但建议使用正确名称。独立模式的运行日志和 PID 默认保存在 `logs/` 与 `run/`。

`S3_ADDRESSING_STYLE` 支持 `auto`、`path` 和 `virtual`。本地 MinIO 使用 `path`；阿里云 OSS 使用 `virtual`，并建议配置 `S3_ENDPOINT=https://s3.oss-cn-hangzhou.aliyuncs.com`、`S3_REGION=cn-hangzhou`。变量缺失或设为 `auto` 时，会自动为阿里云 OSS/AWS 端点选择 virtual-hosted style，其他兼容端点保持 path-style。

版本文件可以来自 S3、GitHub Release 或另一套 WenzWork 镜像站。本地文件通过同源管理员 API 流式上传到 S3，不要求存储桶开放浏览器 CORS；普通 HTTP(S) 外链会由服务端安全下载、检测并转存到 S3；读取 GitHub 最新 Release 时则保存受控的 Asset 引用，不再要求私有仓库附件支持匿名访问。发布界面分别配置 Web/服务端、桌面端、手机端三个 GitHub 仓库、各自的访问 Token 与可选镜像站地址；Token 使用系统主密钥加密保存，查询 Release、拉取附件和解析下载链接立即生效，无需重启。镜像拉取直接查询目标站 `/api/v1/releases/latest`，只接受目标站同源的公开下载路由并保存与链接绑定的受控引用，不要求配置 S3；安装包首次公开下载时由服务端从镜像链接直接写入本机缓存，并严格核对目录声明的文件名、大小和 SHA-256，后续下载复用已验证缓存。缓存缺失时镜像站必须在线；镜像地址、首次请求及重定向目标都受公网 HTTP(S) 与 SSRF 防护约束，内网、本机、带凭据、查询参数或片段的地址会被拒绝。镜像资产始终使用服务端缓存下载，不受 S3 或 GitHub 跳转模式影响。版本号按项目类型独立判重，公开 `/releases` 与 `/releases/latest` 通过 `project=web|desktop|mobile` 选择目录，旧客户端省略参数时仍读取桌面端。管理后台 Token 与部署包 `.env` 中的 `GITHUB_ACCESS_TOKEN` 用途彼此独立：前者只供站点服务端拉取受控资产，后者只供单个已安装程序升级。`GITHUB_RELEASE_REPOSITORY` 是 Web 仓库和旧环境 Token 的兼容初始值，桌面端与手机端默认仓库分别为 `lyming99/wenzwork`、`lyming99/wenzwork_mobile`；数据库初始化后均可在页面修改。

公开下载页按 Web/服务端、桌面端和手机端展示各自最新版本与历史记录。Web 项目发布匹配平台和架构的 `wenzwork-host-deployment-*` 后，页面只显示一个官方 Host 脚本按钮，不再收集数据库、Redis、SMTP、Relay 或路径参数。脚本从本站受控路由下载并校验归档，生成默认管理员密码、自动准备临时 PostgreSQL/Redis，再初始化并后台启动 Host。Relay 由管理后台按平台和架构生成带签名校验的一键命令及下载脚本，默认 Linux/amd64；Device Agent 由用户“远程设备”页生成含一次性设备 Key 的一键脚本，默认 Windows/amd64。公开仓库均无需 GitHub Token。详细边界见[《网页一键部署》](docs/部署运维/网页一键部署.md)。

升级前已发布的旧“自定义链接”仍保持只读重定向，避免现有下载立即失效；再次编辑时必须改为受管理的 S3 或 GitHub Release 资产。

管理后台可持久化选择三种公开下载策略：

- “直链”：Go 服务从资产所属的 S3、GitHub Asset API 或受控镜像链接拉取文件，校验大小和 SHA-256 后原子写入 `RELEASE_ASSET_CACHE_DIR`；后续请求复用本地缓存，并支持 HTTP Range。GitHub 私有资产由服务端携带 Token 拉取，Token 不会发送给浏览器；镜像资产不读取或写入 S3。
- “S3 链接”：后台填写 S3、OSS 或 CDN 的公开 URL 前缀，下载路由自动拼接对象键并返回重定向。此模式仅适用于 S3 资产，且要求前缀下的对象对下载者可读。
- “GitHub 链接”：服务端使用已保存 Token 请求 GitHub Asset API，再把 GitHub 返回的临时下载地址交给浏览器；支持私有仓库，不要求用户登录 GitHub，也不会在跳转中暴露 Token。此模式仅适用于 GitHub Release 资产，服务器必须能访问 `api.github.com` 和 GitHub 资产域名。

### 本机构建推送

Release Access Key 的 SHA-256 摘要保存在数据库中，推送接口每次请求都读取数据库校验；管理员可在“软件版本管理 → 基础配置”生成或填写新密钥并立即轮换，无需重启，旧密钥随即失效，查询接口和审计日志均不返回明文。升级时，Host 会把 `RELEASE_ACCESS_KEY` 或 `RELEASE_ACCESS_KEY_FILE`（默认 `cache/host-secrets/release-access-key`）中的旧密钥一次性导入数据库，之后数据库配置始终优先，重启不会覆盖管理员设置。构建端优先读取 `WENZWORK_RELEASE_ACCESS_KEY`（同时兼容 `RELEASE_ACCESS_KEY`），也可在命令中传 `-AccessKey`。真实密钥不要提交到仓库或写入构建日志。

推送接口为 `POST /api/v1/release-push/assets` 与 `POST /api/v1/release-push`：前者按原始请求体接收文件并强制核对大小、SHA-256，后者原子合并版本目录。文件持久保存在 `RELEASE_PUSH_STORAGE_DIR`（默认 `cache/release-push`），与可清理的 `RELEASE_ASSET_CACHE_DIR` 分离，不经过 S3。`local` 文件无论全局下载策略如何都由服务端直接交付。

Host、Relay、Device Agent 可按组件、系统和架构选择构建并推送；唯一必填的业务参数是版本号，公告省略时显示“软件名称 版本号更新啦~”。本地推送不会读取 Git commit，也不要求工作区干净；同一项目尚无该版本时创建版本，已有该版本时更新同名文件并保留其他文件。管理端地址可通过 `WENZWORK_RELEASE_HOST` 配置，同时兼容 `RELEASE_HOST`、`WENZWORK_RELEASE_URL` 和 `WENZWORK_RELEASE_BASE_URL`；命令行 `-ReleaseBaseUrl` 优先级最高，地址必须是绝对 HTTP(S) URL：

```powershell
$env:WENZWORK_RELEASE_ACCESS_KEY = Read-Host 'Release Access Key' -MaskInput
$env:WENZWORK_RELEASE_HOST = 'https://wenzwork.example'
pwsh ./scripts/Build-And-Push-Release.ps1 v0.3.0
pwsh ./scripts/Build-And-Push-Release.ps1 -Version v0.3.0 -Components relay,device-agent -Platforms linux -Architectures amd64 -UpdateNotes '稳定性与升级流程优化'
```

默认会立即发布；传 `-Draft` 只更新草稿。脚本根据接口的 `201 Created` / `200 OK` 明确输出本次是创建还是更新。同一项目和版本的后续推送按文件名替换同名产物并保留其他文件，因此各原生平台可以分批补齐；已经下架的版本不会被自动恢复。正式 GitHub Release 构建仍保留独立的 Git commit 与干净工作区门禁。

`DOWNLOAD_CDN_BASE_URL` 仅用于记录对象上传时的存储地址快照；用户实际走哪种下载方式及直跳前缀均由管理后台控制。生产部署应为缓存目录预留磁盘空间，并把它放在升级不会覆盖的位置。

## 验证

- 前端：`corepack pnpm lint:web`、`corepack pnpm typecheck:web`、`corepack pnpm test:web`、`corepack pnpm build:web`。
- 后端：进入 `server` 后运行 `go test ./...` 与 `go build -buildvcs=false ./cmd/api ./cmd/admin ./cmd/remote-poc`；在完整 Git checkout 中可移除该参数以写入 VCS 元数据。
- 协议：运行 `go run github.com/bufbuild/buf/cmd/buf@v1.72.0 lint api`，并用 `corepack pnpm generate:api` 重新生成 OpenAPI/Protobuf 类型。
- PostgreSQL 集成/race：设置 `TEST_DATABASE_URL` 后，在 `server` 中运行 `go test -race -tags=integration ./internal/auth ./internal/analytics ./internal/catalog ./internal/membership`。
- 全部：`corepack pnpm check`。

不要提交真实密钥、安装包或本地数据库数据。生产环境应启用 HTTPS，并建议管理员按部署边界显式开启安全 Cookie 与管理员 MFA；密钥应从密钥系统注入配置。
