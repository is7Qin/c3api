# internal/supplierport — 供应商记账/解冻/视图链的存储契约

> 根 `AGENTS.md` 覆盖项目级约定；依赖图规则见 `internal/architecture/AGENTS.md`。

## 为什么有这个包

这些接口由**消费者**（`internal/supplier` 的 worker 编排）声明、由**实现者**
（`internal/repository`）实现——方向是正确的依赖倒置。但定义原先住在
`internal/supplier` 里，而那个包同时装着 worker 生命周期、`logx`、重试与快照，
于是**实现者被迫依赖整个编排包**，形成"低层依赖高层"的假象边。

把契约抽到中立叶子后：

```
internal/supplier    → 只依赖本包（编排 worker）
internal/repository  → 只依赖本包（实现接口）
```

双方不再互相认识。与 `internal/loadport`（P5-1 同样做法）同范式。

## 职责 / 入口

| 接口 | 实现 | 用途 |
|---|---|---|
| `CreditStore` | `repository.SupplierRepo` | 记账链：取批 / 冻结小时 / 应用事务 / 存量释放 / lag |
| `CreditLocker` | `repository.SupplierRepo` | 记账链串行化（同一时刻一个实例推进游标） |
| `ThawStore` | `repository.SupplierRepo` | 解冻链：到期桶处理 + 物理观测 |
| `ViewStore` | `repository.SupplierRepo` | 供应商视图装载（归属 + 分成率） |

行值类型（`BatchRow` / `UIDCredit` / `DayRecon` / `ThawBucketSnapshot`）**在
`internal/domain`**，本包只引用它们——因为聚合函数（`AggregateBatch` 等）也要用这些
值，而那些函数在 domain；若把行值放本包，domain 就得反向依赖本包。

## 不变量

- **零逻辑**：只放 interface。不要在这里加 SQL、重试策略、时钟读取或校验——
  那些属于实现方（`repository`）或编排方（`supplier`）。
- **只依赖 `domain`**：多一条 `internal/*` 依赖就要重新审一遍"这真的是契约层吗"。
- **接口方法一经发布即契约**：改签名要同步实现方 + 所有 fake（`cmd/server`、`repository` 测试）。
- **不要把 worker 生命周期放进本包**：`Name/Start/Close` 属编排，不属契约。

## 验证

```bash
go test ./internal/supplierport/ -count=1        # 无独立测试（纯声明）
go test ./internal/supplier/ ./internal/repository/ -count=1
go test ./internal/architecture/ -count=1
go list -f '{{join .Imports "\n"}}' ./internal/supplierport   # 只应有 context + internal/domain
```
