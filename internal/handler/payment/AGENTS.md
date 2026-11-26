# internal/handler/payment — 支付网关公开回调面

> 根 `AGENTS.md` 覆盖项目级约定；管理面在 `internal/handler/paymentadmin`。

## 职责

`handler.go` 实现 `api.gen.go`（oapi-codegen，**禁止手改**）声明的 `/api/payment/*` 端点。

## 不变量（安全关键）

- **本面无需鉴权**（支付网关不带 JWT）⇒ **所有可信度来自验签**。每个回调必须先
  `VerifyValues` 再处理，验签失败一律拒绝且**不泄露内部细节**。
- **幂等**：同一 `merchant_ref` / 订单号的重复回调必须幂等（不能重复发货/重复记账）。
- **`merchant_ref` 是只读的**：客户端试图改它 ⇒ `paymenterr.ErrMerchantRefImmutable` 映射 **400**。
- 状态映射表在 `internal/httpface.WriteServiceErr`，不要在本包自己写状态码。

## 验证

```bash
go test ./internal/handler/payment/ -count=1
```
