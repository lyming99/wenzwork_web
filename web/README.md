# WenzWork Web Frontend

Vue 3、TypeScript 与 Vite 8 前端，包含官网、账户/会员中心和管理后台三个路由区域。

从仓库根目录运行：

- `corepack pnpm dev:web`：开发服务器。
- `corepack pnpm lint:web`：ESLint。
- `corepack pnpm typecheck:web`：Vue/TypeScript 类型检查。
- `corepack pnpm test:web`：Vitest。
- `corepack pnpm build:web`：生产构建。

API 默认通过 Vite 代理访问 `http://localhost:8080/api/v1`。业务数据不能以浏览器存储作为真实来源；账户、会员和兑换码均由 Go API 与 PostgreSQL 管理。
