# internal/paymenterr — 支付域的业务错误哨兵

> 根 `AGENTS.md` 覆盖项目级约定。与 `internal/storeerr` 的分界见该包的 `AGENTS.md`。

## 为什么有这个包（R1-c）

`handler/paymentadmin` 要为一个业务判定消耗 `errors.Is`，却因此 import 整个存储实现层。
方向必须是"**实现 → 契约**"。

## 为什么不放进 `internal/storeerr`（最关键，别改回去）

`storeerr` 的判据是"**任何**存储实现都会有的协议分类——删掉业务语境后它还成立吗？"
`ErrNotFound` / `ErrConflict` / `ErrStaleIdentityRevision` 删掉业务语境依然成立，所以它们在那里。

本包的哨兵**不成立**：离开支付域，"`merchant_ref` 是谁的哪个字段"就无从解释。
更直接的证据是它的消费方与 HTTP 映射：`ErrMerchantRefImmutable` 被**有意映射成 400**（不是 409）
⇒ 它是**业务配置错误**，不是存储冲突。

## 导出面

| 哨兵 | HTTP | 语义 |
|---|---|---|
| `ErrMerchantRefImmutable` | **400** | `merchant_ref` 是只读的（客户端试图改它） |

实现：`fmt.Errorf("%w: merchant_ref is read-only", storeerr.ErrConflict)` ——
**同时**是 `storeerr.ErrConflict`（供通用冲突分类）**与**本包的语义哨兵。这个双层 `%w` 是有意的。

## 验证

```bash
go test ./internal/paymenterr/ -count=1
```
