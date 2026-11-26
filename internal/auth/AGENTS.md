# internal/auth — 认证体系（密码 / JWT / RBAC 中间件 / 身份上下文）

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲认证边界。

## 职责 / 入口

| 文件 | 导出面 |
|---|---|
| `password.go` | `HashPassword` / `VerifyPassword` / `ValidatePasswordLen`（bcrypt，与 sub2api 同参数） |
| `jwt.go` | `Issuer` / `NewIssuer` / `Claims`（HS256，TTL 24h） |
| `middleware.go` | `RequireJWT` / `RequireRole` / `ClaimsFrom` / `UserStatusProvider` |
| `identity.go` | `RequireIdentity` / `SnapshotProvider` / `ManagementKeyProvider` |
| `ctx_user.go` | `WithUserID` / `UserIDFromContext`（管理面已鉴权 uid 的 ctx 访问器） |

## 不变量

- **快照 role 覆盖 claims.Role**：JWT 的 `claims.Role` **不是**角色信任源；每个受保护请求都查
  `UserStatusProvider.UserSnapshot`，降权（`platform_admin → user`）后旧 JWT 立即失效（快照刷新
  ≤ Reload 周期）。claims 24h 长时效不作角色依据。
- **快照缺失 ⇒ fail-closed 拒绝**（启动首刷失败 / Reload 失败保留旧快照 / NOTIFY 丢失同纪律）。
- **`opts.Auth == nil` ⇒ JWT/mk- 路径整体拒绝**（fail-closed）。
- **管理 key 分支不得回落 JWT**：Bearer `mk-` 前缀先判，失败/非 `platform_admin` 直接 401。
- **ctx key 一律私有**（`ctxClaimsKey` / `ctxAdminUserIDKey`）：写入只经 `With*`，读取只经
  `*From`，外部无法自造 key 伪造身份。
- **本包是叶子级**：只依赖 `internal/domain` + `internal/httpface`。不 import service/repository/
  server/scheduler。`internal/handler` 已允许 import 本包（allow 矩阵显式白名单）。

## 为什么 `UserIDFromContext` 在这里

它原住在 `internal/server`（传输壳），而唯一生产消费者是 `internal/handler`（兑换码 `created_by`）。
一个纯 context 读取让 API 层反向依赖装配层是没有道理的（P4-c-1）。搬到与本包既有的
`ClaimsFrom` 同构的位置后，`handler → server` 那条债消失。`server` 侧改调 `auth.WithUserID`。

## 验证

```bash
go test ./internal/auth/ -count=1
```
