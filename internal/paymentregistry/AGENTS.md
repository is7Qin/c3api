# internal/paymentregistry — 本二进制编译进来的全部支付协议

> 根 `AGENTS.md` 覆盖项目级约定。

## 职责

`registry.go` 的 `Registry`：协议清单的**唯一**事实来源。

**新增协议** = 实现 `domain.PaymentProvider` + 在 `Registry` 里加一行。`service` 与 `main` 都
**不许** import 协议包（新增上游因此是一处改动，而不是散落在装配面的多份字面量）。

## 不变量

- **协议实现永不删除**：下线只把账户 `enabled=false`。删实现会让该账户的历史订单永久无法 sync、
  迟到回调无法验签（v1 F48 的资金级结论）。
- 本包是**清单**，不是实现。协议实现在 `internal/service/epay` 等。

## 验证

```bash
go test ./internal/paymentregistry/ -count=1
```
