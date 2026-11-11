# internal/sdkbridge — Codex SDK 适配层

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲
> sdkbridge 的职责与探测 seam。

## 职责 / 入口

- 生产 Codex 适配：`Codex`（`codex.go`）持有账号级 SDK client/Auth 缓存、fatal
  回调、轮转与 WS/HTTP 传输；`clients.go` 是 SDK 工厂接线。
- 诊断/探测 seam：`StreamBodyOnce`（`stream_body_once.go`）。它只做：
  - 一次性 PAT client + `WithTransport`（不传 `WithTimeout`；期限只由 ctx 决定）；
  - `StreamBody` 的 200/非 200 状态与 raw 搬运；`AuthPermanentlyRevokedError` 记 401；
  - 200 body 在 `BodyReader` 返回后**恰好关闭一次**；`BodyReader` 禁止 Close。
- **不承载探测语义**：不构造探测 payload、不识别 SSE 终态、不映射固定错误文案、
  不写日志。请求体/终态/错误分类在 `internal/probe`。

## 不变量

- 不记录/不回显 token、payload、上游原文；SDK error 文本不进入结果。
- 生产 Codex 与 one-shot seam 分离：seam 不取 ClientTable、不装 fatal 回调、不轮转。
- 非 200 body 由 SDK 读尽并关闭；调用方 cap transport 负责 2 MiB 上限。
- codex WS 面不喂任何客户端头（spec §12）；HTTP 面头策略仅在 `pkg/aiclient` 维护。
- 生成物不手改；日志只走 `pkg/logx`。

## 验证

```bash
go test ./internal/sdkbridge/ -count=1
go test ./internal/sdkbridge/ -run TestStreamBodyOnce -count=1
```
