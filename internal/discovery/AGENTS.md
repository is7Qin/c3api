# internal/discovery — Redis 实例发现

> 根 `AGENTS.md` 覆盖项目级约定。spec 2026-08-25-redis-instance-discovery-design。

## 职责

`discovery.go` 的 `Discovery` / `New` / `Stats` / `RendezvousOwner`：
单 ZSET 心跳成员协议替换手工 `cluster.instances` 设置——实例心跳注册、活体计数即多实例预算分摊基数 N。

## 协议

- key `c3api:cluster:members`
- member = 实例 ID（`hostname-pid-bootnonce`，复用 `notify/publisher.go` 的 `Src` 生成模式，
  `main` 注入同一值）
- score = 最近心跳 unixmilli
- 单个心跳循环每 tick 一条 pipeline 同时完成心跳与计数

## 不变量

- **Redis 只承载可丢的易失协调状态**，不是 SoR。
- **`RendezvousOwner` 是"某 key 归哪个实例"的确定性判据**（rendezvous hashing），
  实例集合变化时只有少数 key 迁移。
- 实例 ID 必须与 `notify` 的 `Src` **同源**（`main` 注入同一值），否则自播跳过失效。

## 验证

```bash
go test ./internal/discovery/ -count=1
```
