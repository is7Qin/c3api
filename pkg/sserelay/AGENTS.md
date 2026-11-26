# pkg/sserelay — SSE 中继（池化 + 逐帧解析 + inLine 续片状态机）

> 根 `AGENTS.md` 覆盖项目级约定。

## 职责

| 文件 | 内容 |
|---|---|
| `relay.go` | `Relay` / `Config` / `Event` / `OnEvent` / `InferEventName`（池化中继） |
| `output.go` | `Output` / `NewOutput` / `OutputOptions` / `SetSSEHeaders` / `FlushWriter` |
| `payload.go` | `PayloadError` / `AsPayloadError` |

## 不变量

- **`InferEventName` 与 `internal/protoconv` 共用**：缺名帧的事件名推断是同一份实现 + 回退全量解码。
  不要在别处写第二份。
- **`inLine` 续片状态机**：跨 chunk 的事件行拼接在这里；改了要跑全量中继测试。
- **池化复用 buffer**，热路径零额外分配为目标。
- 本包只依赖 stdlib（`bufio` / `bytes` / `context` / `encoding/json` / `errors` / `fmt` / `io` /
  `net/http` / `sync` / `time`），**不依赖任何 `internal/*`**。

## 验证

```bash
go test ./pkg/sserelay/ -count=1
```
