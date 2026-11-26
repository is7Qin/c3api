# internal/notify — 多实例变更广播（PG LISTEN/NOTIFY）

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。

## 职责

管理面变更落库成功后经 `Publisher` 发一条 NOTIFY（单 channel `c3api_invalidate`，紧凑 JSON 载荷
与 `invalidate.State` 同构）；每实例一个 `Listener` worker LISTEN 该 channel，解析后调注入的
`Dispatcher` 转发现有的 `invalidate.Debouncer.Mark`——**本地/远端变更共享同一去抖窗口**，天然合并去重。

| 文件 | 内容 |
|---|---|
| `publisher.go` | `Publisher` / `NewPublisher` / `Change` / `Marshal` / `Unmarshal` / `Execer` |
| `listener.go` | `Listener` / `NewListener` / `ListenerConfig` / `Dispatcher` / `ConnectFunc` / `Conn` |
| `stats.go` | `ListenerStats` |

## 不变量

- **载荷守卫**：PG NOTIFY 载荷上限 8000B。批量账号变更的 Groups 集合可能超限 ⇒ marshal 后 `> 6KB`
  时**丢弃 Groups 并置 `Templates=true`**（降级 sched 全量重载，sched 全量包含组级重载，语义仍正确）。
- **`Src` 自播跳过**：`Publisher` 自动填 `src=实例 ID`，接收端跳过自身发布的 NOTIFY（省一次重复 reload）。
- **计费扣费路径绝不发布 NOTIFY**（每 flush 即风暴）。
- **本包不 import `internal/invalidate`**：`Dispatcher` 由 `main` 注入（避免环）。

## 验证

```bash
go test ./internal/notify/ -count=1
```
