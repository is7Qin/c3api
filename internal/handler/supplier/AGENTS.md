# internal/handler/supplier — 供应商面 `/api/user/supplier/*`

> 根 `AGENTS.md` 覆盖项目级约定；角色模型见 `internal/AGENTS.md`。

## 职责

`api.gen.go` 是 **oapi-codegen 生成物，禁止手改**（`-include-tags supplier`）。

## 不变量

- **可达角色集是单一事实源**：`domain.SupplierSurfaceRoles()` = `{supplier, platform_admin}`。
  **不得**在门控与归属校验两处各自重抄角色名（否则新增可达角色时会漂移）。
- **作用域以 JWT 自身 `user_id` 为准**：`platform_admin` 进入供应商面也只能管理**自己名下**的账号
  与收益，不互相放大。
- 业务逻辑在 `internal/supplier` + `internal/service`；本包只做转换与错误映射。

## 验证

```bash
go test ./internal/handler/supplier/ -count=1
```
