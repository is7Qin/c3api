# internal/snapshot — 内存快照生命周期注册表

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。

## 职责

统一各内存快照的**生命周期**：启动就绪（`ReloadAll` 全量首刷）+ NOTIFY 事件分发（按 `Scope`
精确重载）+ 状态可观测（`Status`）。

## 边界（用户拍板 2026-08-11，别越界）

- **只做事件驱动与状态追踪**，**不接管**各模块周期 ticker（scheduler syncLoop / balances ticker /
  auth-sync / price cron 均留模块自管——避免双 reload 竞争）。
- **不缓存/存储快照数据**：数据仍在各模块（Auth `RWMutex` / scheduler store / Balances
  `atomic.Pointer` / rulesMu / service pricing snapshot）。注册表只持名称/scope/状态**元数据**。
- **不进入请求热路径**：所有锁只在启动/NOTIFY/管理面低频路径。快照读取照旧模块内原子读。

## scope 语义

`Snapshot` 注册时声明关心的变更 scope；NOTIFY 变更按类型映射 scope 后 `Reload(ctx, scopes...)`
**只重载命中 scope 的快照**（未命中不动）。

## 验证

```bash
go test ./internal/snapshot/ -count=1
```
