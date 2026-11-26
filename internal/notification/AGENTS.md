# internal/notification — 通知投递（邮件 / 告警冷却）

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。

## 职责

| 文件 | 内容 |
|---|---|
| `worker.go` | `Worker` / `New` / `WarningMailEnqueue`：余额告警等通知的入队与投递 |
| `cooldown.go` | `Cooldown` / `NewCooldown`：同一告警的去重冷却（Redis） |
| `delivery.go` | 投递实现 |
| `errors.go` | 哨兵 |

## 不变量

- **冷却键在 Redis**：Redis 只承载可丢的易失协调状态（见 `pkg/redisx` 的边界）。
- **不阻塞请求路径**：告警投递是后台 worker；`billing.BalanceWarningSink` 只入队。
- **每个三元组只 WARN 一次**（与 `internal/protoconv` 的 sink 去重口径一致）。

## 验证

```bash
go test ./internal/notification/ -count=1
```
