# internal/supplier — 供应商收益记账链 / 解冻链的纯逻辑与 worker 契约

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。spec 2026-10-09 §5。

## 职责

| 文件 | 内容 |
|---|---|
| `aggregate.go` | `AggregateBatch` / `AddChecked` / `BatchRow` / `DayRecon` / `UIDCredit`（聚合法定与守恒） |
| `credit.go` | `CreditWorker` / `CreditStore` / `CreditConfig` / `CreditLocker` / `NewCredit` |
| `freeze.go` / `thaw.go` | `EffectiveFreezeHours` / `BucketAvailableAt`（冻结参数与桶时刻） |
| `payout.go` / `risk.go` | 结算、重试分类、风控 |
| `snapshot.go` / `view.go` | 供应商视图快照 |

## 不变量

- **持久 SQL 由 `internal/repository` 承载**（绑定真实 PG）；本包保持**无 DB 依赖**，纯函数可单测
  （冻结参数、桶时刻、聚合法定/守恒、重试分类）。实测依赖面只有 `domain` + `worker` + `logx` +
  `pgx/pgconn`（仅错误分类）+ stdlib。
- **不 import `internal/repository`**：数据经窄接口（`CreditStore` / `CreditLocker`）注入。
- **聚合必须守恒**：`AddChecked` 检查溢出；`floor(cost × share_bp / 10000)` 的收益快照语义见
  `domain.UsageLog` 的 `SupplierEarnMillis` 字段注释。
- **归属随选中固定**（非收尾重查）：转属/删除不改在途请求归属。

## 已知架构债

`repository → supplier`（`productionLedger`）——repository 反向依赖本聚合。修复需把供应商视图
抽成端口，属架构改造级（上一轮探明：14 个方法转发 + 8 处业务函数），**不要顺手做**。

## 验证

```bash
go test ./internal/supplier/ -count=1
```
