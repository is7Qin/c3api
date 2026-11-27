# internal/pgretry — PostgreSQL 事务级可重试错误分类

> 根 `AGENTS.md` 覆盖项目级约定；依赖图规则见 `internal/architecture/AGENTS.md`。

## 为什么有这个包

判"这个错误值不值得重试一次事务"完全由 PostgreSQL 的 SQLSTATE 决定（死锁 `40P01`、
锁不可用 `55P03`）。这是**基础设施事实**，不是业务规则。

它原先住在 `internal/supplier`，使得该包被迫 import `jackc/pgx/v5/pgconn`——一个只做
记账/解冻编排的包，因此永远无法成为"纯领域"。把它抽出来，`supplier` 就与驱动无关了，
而任何需要事务重试的调用方都能直接依赖本包。

## 职责 / 入口

| 文件 | 内容 |
|---|---|
| `pgretry.go` | `IsRetryableTxErr(err error) bool` + 两个 SQLSTATE 常量 |

只此一个函数。`errors.AsType[*pgconn.PgError]` 解出 SQLSTATE；包装过的错误同样可识别。

## 不变量

- **零内部依赖**：只 import `errors` 与 `pgconn`。不 import 仓库内任何包——
  领域包不得知道 pgx，这是同一条原则的另一面。
- **只认两个 SQLSTATE**：`40P01`（死锁）/ `55P03`（锁不可用）。其余一律 `false`，
  包括 `40001`（序列化失败）——**不要"顺手"加**，那会改变事务重试次数与开销，
  需要单独评估。
- **`nil` 返回 `false`**：不重试，且不 panic。

## 验证

```bash
go test ./internal/pgretry/ -count=1
go list -f '{{join .Imports "\n"}}' ./internal/pgretry | rg -x 'github.com/is7qin/c3api/.*' || echo "零内部依赖 OK"
go test ./internal/architecture/ -count=1
```
