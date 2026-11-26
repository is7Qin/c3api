# internal/service/epay — 易支付（epay）协议实现

> 根 `AGENTS.md` 覆盖项目级约定。协议清单注册见 `internal/paymentregistry/AGENTS.md`。

## 职责

| 文件 | 内容 |
|---|---|
| `client.go` | `Client` / `NewClient` / `QueryRequest`（HTTP 调用） |
| `provider.go` | `Provider` / `New` / `Config`（实现 `domain.PaymentProvider`） |
| `sign.go` | `SignValues` / `VerifyValues`（签名与验签） |

## 不变量

- **验签必须常数时间**（防时序侧信道）；签名串的**键排序**是协议的一部分，不要"优化"掉。
- **协议实现永不删除**（见 `paymentregistry`）：下线只把账户 `enabled=false`。
- 迟到回调仍要能验签 ⇒ 密钥轮换要保留旧密钥窗口。

## 验证

```bash
go test ./internal/service/epay/ -count=1
```
