# internal/pricingresult — 价格同步/预览的结果值（中立叶子包）

> 根 `AGENTS.md` 覆盖项目级约定；依赖图规则见 `internal/architecture/AGENTS.md`。

## 为什么有这个包

`internal/handler` 要驱动一次价格手动同步并读它的结果，但**不该认识** `internal/pricing`
的实现面（fetcher / repo / settings / cron / worker 生命周期）。P6-1 之前 handler 的字段直接
写成 `*pricing.SyncWorker`，这条边靠 `productionLedger` 的 legacy 债豁免才成立。

双方**唯一**该共享的东西是「一次同步/预览产出了什么」。把它放进一个只依赖 stdlib 的中立叶子，
消费方接口（`handler.PricingSync`，由 handler 自己声明）与实现（`pricing.SyncWorker`）就能各自
独立演化。与 `internal/modelmap` / `internal/identityenv` 同范式。

## 职责 / 入口

| 文件 | 内容 |
|---|---|
| `pricingresult.go` | `SyncStats`（Rows/Skipped/Updated/Variants）、`Preview`（ToAdd/ToUpdate/Skipped/Entries/VariantsChanged）、`PreviewEntry`（Model/Mode/Action） |
| `errors.go` | `FetchError{Err error}` + `Error()` / `Unwrap()` |

`pricing` 侧留**类型别名**（`type SyncStats = pricingresult.SyncStats` 等，三个一起），
所以 `SyncWorker` 的既有签名与全部调用点零改动。

## 不变量

- **零内部依赖**：只 import stdlib。多一条 `internal/*` 或 `pkg/*` 的 import 就让「中立叶子」
  失效，矩阵里本包的规则是显式 `to: []string{}`，加 import 会直接红。
- **只放纯数据**。加任何函数/方法进来之前先问：它是不是纯数据？不是就别放（没有解析、
  没有校验、没有 HTTP 语义）。
- **`FetchError` 用 `errors.As` 判，不要退回哨兵判定**。消费方（handler）判的是**类型**，
  不是 `pricing.ErrPriceFetch` 这个变量名——后者是实现方私有。
- **`FetchError` 不包装 `serviceerr.ErrUpstream`**，也不该包装：本包不知道 HTTP 状态码映射；
  而且 `ErrUpstream` 被 `codex_login`/`payment` 等多处共享生产，拿它当谓词会把无关上游故障
  误标成「价格同步失败」。「拉取失败是上游不可用」这条语义归**实现方**（`pricing/manual.go`
  在外层再包一条 `%w` 给 `httpface.WriteServiceErr` 用）。
- **这层 `%w` 不是 handler 的承重件**：handler 的 502 分支只认 `FetchError`，与
  `ErrUpstream` 包装存在与否无关（`As` 与 `Is` 相互独立）。别为「不包装就红」造门禁。
- **`Preview.Entries` 有写入方、有测试、未接线**：`pricing` 写它，`internal/pricing/manual_test.go`
  断言它，但 `POST /pricing/sync/preview` **目前不返回** `entries`。接线是一次**独立特性**
  （契约里已定义该字段），不是解耦的副作用——**不要顺手做**。

## 验证

```bash
go test -count=1 ./internal/pricingresult/          # 本包无测试文件（纯数据）
go test -count=1 ./internal/pricing/                # 别名 + 双层包装的契约
go test -count=1 -run TestPricingSyncPredicateIsPrecise ./internal/handler/   # 谓词精确性门禁
```
