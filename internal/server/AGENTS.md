# internal/server — chi 路由装配点（传输壳）

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。

## 职责

| 文件 | 内容 |
|---|---|
| `server.go` | `NewServer` / `Options` / `Server`：装配 chi 路由 |
| `middleware.go` | `adminAuth`（平台管理员 JWT **或** 该身份的 `mk-` 管理 key）、recover / 请求日志等 |
| `schema_gate.go` | `ReadyFunc` / `SchemaStatus`：迁移水位就绪门（未就绪不放流量） |

路由分组：
```
/api/admin/*    → adminAuth（静态 token 已删；platform_admin JWT 或 platform_admin 管理 key）
/api/user/*     → user.Router（register/login 公开，其余 RequireJWT）
/api/payment/*  → 支付网关公开回调，**无鉴权**（网关不带 JWT）
三个 AI 端点 + /healthz
```

## 不变量

- **所有 API 统一收口于 `/api/*`**：前端 SPA 占用根及 `/api/user/*`、`/app/*`，两者前缀无重叠 ⇒
  SPA fallback 可无歧义地回 `index.html`。
- **`adminAuth` 是快照权威**：`role == platform_admin && status == active` 才放行；降权后该管理 key
  立即失去 `/api/admin` 可达（仍是其 user 面 key）。
- **本包是装配点**：允许 import `internal/auth`（注入 `Issuer` / `SnapshotProvider` 做中间件）——
  这是**结构性**依赖，登记在 `importgraph_test.go` 的 `keep` 区，不是债。
- **管理面 uid 的 ctx 访问器在 `internal/auth`**（`auth.WithUserID` / `auth.UserIDFromContext`），
  **不在**本包。业务层要用它不该依赖传输壳。

## 验证

```bash
go test ./internal/server/ -count=1
```
