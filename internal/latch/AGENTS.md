# internal/latch — 规则失效锁存（单实例内存）

> 根 `AGENTS.md` 覆盖项目级约定。叶子包的依赖方向见 `internal/architecture/AGENTS.md`。

## 职责

`latch.go` 的 `LatchStore` / `Hub` / `LatchKey` / `NewLatchStore` / `NewHub`：
单实例内存锁存（account + fingerprint + revision）+ 规则失败事件的同步扇出中介。

## 不变量

- **叶子包**：只依赖 `internal/rule` 的事件类型。**无环**——`rule` 永不回指本包。
- **不依赖 scheduler**：scheduler **反向持有**本包指针（反向持有即 import 环）。
- **所有权**：`main` 构造 `LatchStore` + `Hub` 各恰好一次，经构造参出借。scheduler 只借指针，
  `reload` 清理 / `TryLatch` / `IsLatched` / `Snapshot` 逻辑不变。
- **生命周期与 Redis 无关**：latch 是单实例内存；`RuntimeHealth` 的 Redis 面保持独立。

## 验证

```bash
go test ./internal/latch/ -count=1
```
