# internal/qualitymodel — 路由质量的行契约与保留期哨兵

> 根 `AGENTS.md` 覆盖项目级约定；运行时在 `internal/quality`。

## 为什么有这个包（R1-b）

`repository` 要读写质量行、`quality` 要定义它们，两者都不该为"行长什么样"互相依赖。

## 导出面

- `RoutingQualityRow` / `RoutingFlowRow` —— 行契约（**列序有意义**，SQL 列按位置绑 args）
- `ErrRoutingSnapshotBeyondRetention` —— 保留期策略哨兵

## 不变量

- **列序断言是必须的**：SQL 是 `INSERT ... VALUES ($1,$2,…)` 的**位置**绑定，用集合比较会放过
  "对调两列" ⇒ 数据静默错位。任何列序相关的门禁断言必须用**位置**语义，不能用 `ElementsMatch`。
- **`ErrRoutingSnapshotBeyondRetention` 留在这里而不是 `storeerr`**：它编码的是**保留期策略**
  （业务语义），且**无 `repository:` 前缀**。判据见 `internal/storeerr/AGENTS.md`。

## 验证

```bash
go test ./internal/qualitymodel/ -count=1
```
