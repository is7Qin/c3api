# internal/handler/user — 用户面 `/api/user/*`

> 根 `AGENTS.md` 覆盖项目级约定；`internal/handler`（根包）= 管理面 `/api/admin/*`，
> 传输装配在 `internal/server`。

## 职责

用户面路由实现（`router.go` 装配）：register / login 公开，其余 `RequireJWT`。

一资源一文件：`keys.go`（客户端 key）/ `usage.go` / `stats.go` / `orders.go` / `groups.go` /
`redemptions.go` / `management_keys.go` / `temp_balances.go` / `payment_methods.go` / `models.go` /
`errlogs.go` / `features.go` / `custom_pages.go` / `balance_warning.go` / `auth.go` / `convert.go` /
`handler.go`。

`api.gen.go` 是 **oapi-codegen 生成物，禁止手改**（见 `internal/handler/generate.go`）。

## 不变量

- **生成物不手改**：改 API 走 `openapi/openapi.yaml` → `go generate` → 实现 → 前端 `pnpm gen:api`。
- **handler 只做 JSON 解码与类型转换**，业务校验在 `service`（写面唯一校验权威）。
  重复判定只可能与 service 分歧。
- **`supplier` 角色不得访问 `/api/admin/*`**（见 `internal/AGENTS.md` 角色模型）。
- 本包**允许** import `internal/rule`（设置转换）——那是 `testLedger` 口径下的既有债
  （`handler/user → rule`，P6 控制器化后消除）。

## 验证

```bash
go test ./internal/handler/user/ -count=1
```
