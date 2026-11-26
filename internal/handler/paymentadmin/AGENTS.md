# internal/handler/paymentadmin — 支付渠道管理面（`platform_admin`）

> 根 `AGENTS.md` 覆盖项目级约定；公开回调在 `internal/handler/payment`。

## 职责

`handler.go` 实现 `api.gen.go`（oapi-codegen，**禁止手改**）声明的支付渠道管理端点。

## 不变量

- **本面在 `/api/admin/*` 之下**，由 `internal/server.adminAuth` 门控（`platform_admin` JWT 或该身份
  的管理 key）。
- **`ErrMerchantRefImmutable` 映射 400**（不是 409）：它是业务配置错误，不是存储冲突。
  这个判定用 `errors.Is`，**禁止字符串嗅探**——正是在这里消费 `internal/paymenterr`。
- **协议实现永不删除**：下线只把账户 `enabled=false`（见 `internal/paymentregistry/AGENTS.md`）。

## 验证

```bash
go test ./internal/handler/paymentadmin/ -count=1
```
