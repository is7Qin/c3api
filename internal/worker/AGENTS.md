# internal/worker — 后台任务抽象

> 根 `AGENTS.md` 覆盖项目级约定。常驻 worker 的**注册序与排空序**见 `internal/AGENTS.md`。

## 职责

`worker.go` 的 `Manager` / `Worker` / `New`：统一后台任务生命周期。
`loop.go` 的 `Loop` / `GoLoop` / `GoRecover` / `CatchPanic`：循环骨架与 panic 捕获。

## 不变量

- **顺序启动、反向排空**（注册序的逆序）——保证依赖方先停。
- **panic 捕获，进程不崩**：`CatchPanic` 记录后继续（或按配置退出循环）。
- **Worker 不接管各模块的周期 ticker**：`scheduler` syncLoop / `balances` ticker / auth-sync /
  price cron 均留模块自管——避免双 reload 竞争。
- Channel 屏障替代 `time.Sleep`；本仓库 **`time.Sleep` = 0**，不要引入。

## 验证

```bash
go test ./internal/worker/ -count=1
```
