# internal/quality — 路由质量折叠（flow / live window / Redis 载荷）

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。纯值类型与行契约在
> `internal/qualitymodel`。

## 职责

把路由尝试的细粒度事实折叠成可观测的单元格与分钟桶：

| 文件 | 内容 |
|---|---|
| `recorder.go` | `AttemptContext` / `CanonicalKey` / `Cell` / `FlowMinute` / `Key` |
| `fold_*.go` | 折叠链：`fold_fact`（事实）→ `fold_cells`（单元格）→ `fold_expand` / `fold_consume` / `fold_seed` / `fold_handoff` / `fold_owner`（所有权） |
| `live_window.go` | 滑动活窗 |
| `redis_encode.go` | Redis 载荷编码（`AppendQualityCellJSON*`） |
| `sync.go` | 同步 worker |

## 不变量

- **折叠计数必须守恒**：`fold_fact.go` 导出 `FlowChainCapacityOverflow` / `FlowChainIncompleteObserved` /
  `ProcessCrashLossUnobservable` 三个可观测哨兵。**丢失必须被计入，不允许静默丢**。
  `FlowChainEnqueueOverflow` 同理。`ResetFlowChainCountersForTest` 仅供测试。
- **所有权明确**：`FlowOwner` 拥有提交的行；`SnapshotAck` 保留累计所有权。折叠**不得 alias 输入行**
  （`TestFlowOwner_FoldDoesNotAliasInputRows`）。
- **`Close` 幂等且 join**，`Close` 后拒绝新的 fold（`TestFlowOwner_CloseGatesLateFolds`）。
- **Redis 载荷编码是热路径**：复用 buffer（`TestAppendQualityCellJSONReusedBuffer`），不要每次分配。
- **行/桶的上限必须精确折叠**，不是截断（`TestFlowOwner_NoRowCapFoldsExactly` /
  `TestFoldCounterFidelity_CapOverflowCountsWithoutCellAdds`）。

## 验证

```bash
go test ./internal/quality/ -count=1                      # 134 个测试，本包最重
go test ./internal/quality/ -run 'TestFlowOwner' -count=1
go test ./internal/quality/ -run 'TestFoldCounterFidelity' -count=1
```
