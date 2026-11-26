# internal/pricing — 定价抓取与手工价格快照

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。

## 职责

| 文件 | 内容 |
|---|---|
| `fetch.go` | `Fetcher` / `NewFetcher` / `FetchResult` / `Parse`（上游价格表抓取与解析） |
| `manual.go` | `SnapshotReader` / `SyncNow` / `Preview`（手工价格与预览）。⚠ `SyncStats` / `Preview` / `PreviewEntry` 的 canonical 定义已下沉到 `internal/pricingresult`，本包只留**类型别名**（三个一起，漏一个就编译失败） |
| `worker.go` | `SyncWorker` / `NewSyncWorker` / `SyncWorkerConfig` / `SettingReader` / `Upserter`（周期 cron） |
| `stats.go` | `SyncWorkerStats` |

## 不变量

- **换算系 `×1e5` 与 `×1e11` 禁混用**（本仓库历史 bug 来源）。金额单位统一为**毫分**：
  `1 USD = 100,000 毫分`。
- **`per-call` 计费不走 `/1e6` 除法**：`PricePerCallMillis` 是"毫分/单元"（search 每次 / 图片每张），
  例外于"毫分 / 1M token"口径。
- **快照只读**：`service` 侧持 `atomic.Pointer` 快照；本包产物经窄接口注入。
- **cron 由本包自管**（不交给 `internal/snapshot` 注册表）——避免双 reload 竞争。
- **fetch 失败的三层包装不能简化**（`manual.go` 的 `SyncNow` / `Preview` 各一处）：
  ```go
  return nil, fmt.Errorf("%w: %w", &pricingresult.FetchError{
  	Err: fmt.Errorf("%w: %w", ErrPriceFetch, err),
  }, serviceerr.ErrUpstream)
  ```
  三层各有消费者，**不要**把它改回 `errors.New` 或单层 `%w`：
  1. `&pricingresult.FetchError` → `handler` 用 `errors.As` 判 502（**专属、精确**，不认变量名）；
  2. `ErrPriceFetch` → 本包测试与既有调用方的 `ErrorIs` 契约；
  3. `serviceerr.ErrUpstream` → 任何经 `httpface.WriteServiceErr` 的路径拿到通用 502。

  ⚠ 但只有第 1 层是 handler 502 的承重件——第 3 层去掉后 handler 行为不变（`As` 与 `Is`
  相互独立）。**不要**为「不包装 ErrUpstream 就红」造门禁：那不是真门禁。
  反过来，若只留第 3 层让 `WriteServiceErr` 兜住 handler，502 响应体会回显
  `err.Error()`（含上游 fetch 原文）——功能性回归 + 信息泄漏，见 `price_entry.go` 的注释。
- **本包不再被 handler import**（P6-1）：生产侧只经 `internal/pricingresult` 共享结果值。
  同包测试里 handler 仍 import 本包（用真实 `*pricing.SyncWorker` 验行为不变），那条边登记在
  `testLedger`，**不要把 pricing 加进 `testFixtureAllow`**（前缀匹配会让任意包的测试都能 import 它）。

## 验证

```bash
go test ./internal/pricing/ -count=1
go test -count=1 -run TestPricingSyncPredicateIsPrecise ./internal/handler/   # 谓词精确性
```
