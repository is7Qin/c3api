# internal/invalidate — 管理面变更的去抖定向失效

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。

## 职责

`invalidate.go` 的 `Debouncer` / `New` / `Kind` / `State` / `Config` + 五个 reloader 窄接口
（`AuthReloader` / `BalancesReloader` / `ClientsReloader` / `SchedReloader` / `RulesReloader`）；
`stats.go` 的 `DebouncerStats`。

## 不变量

- **脏标记路径零锁零 DB**：`Mark` 走 atomic CAS 合并 + 非阻塞唤醒，**不阻塞任何调用方**
  （消除压测实证的 33,705 goroutine `reloadMu` 串行雪崩）。
- **后沿语义**：定时窗口到点批量执行一次合并重载；执行完成后若又脏**立即再执行**，
  不按固定间隔 throttle——不与长 reload 重叠。
- **接线矩阵**（定稿）：用户 CRUD/余额变更 → auth + 余额快照全量；模板变更 → sched 全量 +
  clients 失效；settings → `ScopeSettings`；规则 → rule reload。
- **本包不 import 业务包**：五个 reloader 都是窄接口，由 `main` 注入。

## 验证

```bash
go test ./internal/invalidate/ -count=1
```
