# internal/pricing — 定价抓取与手工价格快照

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。

## 职责

| 文件 | 内容 |
|---|---|
| `fetch.go` | `Fetcher` / `NewFetcher` / `FetchResult` / `Parse`（上游价格表抓取与解析） |
| `manual.go` | `Preview` / `PreviewEntry` / `SnapshotReader` / `SyncStats`（手工价格与预览） |
| `worker.go` | `SyncWorker` / `NewSyncWorker` / `SyncWorkerConfig` / `SettingReader` / `Upserter`（周期 cron） |
| `stats.go` | `SyncWorkerStats` |

## 不变量

- **换算系 `×1e5` 与 `×1e11` 禁混用**（本仓库历史 bug 来源）。金额单位统一为**毫分**：
  `1 USD = 100,000 毫分`。
- **`per-call` 计费不走 `/1e6` 除法**：`PricePerCallMillis` 是"毫分/单元"（search 每次 / 图片每张），
  例外于"毫分 / 1M token"口径。
- **快照只读**：`service` 侧持 `atomic.Pointer` 快照；本包产物经窄接口注入。
- **cron 由本包自管**（不交给 `internal/snapshot` 注册表）——避免双 reload 竞争。

## 验证

```bash
go test ./internal/pricing/ -count=1
```
