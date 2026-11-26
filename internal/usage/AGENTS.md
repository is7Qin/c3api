# internal/usage — usage_logs 唯一写点 + 配额累加 + 离线聚合

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。

## 职责

| 文件 | 内容 |
|---|---|
| `usage.go` | `usage_logs` 的**唯一写点**（`InsertBatch`）+ 在线配额累加 |
| `errlog.go` | `ErrLogWorker` / `ErrLogInserter` / `ErrLogConfig`（错误日志批量写） |
| `attemptlog.go` | `AttemptLogWorker`（尝试链落库，S3 去重锚 `AttemptOrdinal`） |
| `stats.go` / `stats_agg.go` | watermark 离线聚合（`usage_stats` / `usage_entity_stats`） |
| `retention.go` | `PartitionManager` / `NewRetention` / `DiskProbe` / `RetentionBlockedAlert`（日分区与保留期） |
| `disk_unix.go` / `disk_other.go` | 磁盘水位探测（build tag 分平台） |

## 不变量

- **请求路径零统计计算**：`usage_stats` 只由离线聚合 worker 写入（DELETE+INSERT 覆盖语义）。
- **日分区**：`usage_logs` / `err_logs` / `usage_stats` 三表被 migrate hook 排除，DDL 归
  `PartitionManager` 管（不在 `internal/ent` 迁移里）。
- **`ttft_hist` 是 PG `bigint[]`**：ent 无数组类型，不进 ent schema；由 SQL 侧
  `count(*) FILTER` 生成，读取面走 pgx 直查扫描。
- **`call_count` 不入 `TotalTokens`**（功能调用非 token）。
- **按 userID 分片 8**（`usage.flush_interval` 500ms + `quota_flush_interval` 10s）。

## 验证

```bash
go test ./internal/usage/ -count=1
```
⚠ 本包多数测试是 PG 集成测试，需要 `TEST_DATABASE_URL` + `TEST_REQUIRE_PG=1`。
