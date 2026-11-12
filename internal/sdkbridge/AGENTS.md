# internal/sdkbridge — Codex SDK 适配层

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲
> sdkbridge 的职责与探测 seam。

## 职责 / 入口

- 生产 Codex 适配：`Codex`（`codex.go`）持有账号级 SDK client/Auth 缓存、fatal
  回调、轮转与 WS/HTTP 传输；`clients.go` 是 SDK 工厂接线。
- 诊断/探测 seam（`stream_body_once.go` / `one_shot.go`）——全部只做一次临时
  PAT client + `WithTransport`（不传 `WithTimeout`；期限只由 ctx 决定），
  **不取** ClientTable/Auth 缓存、**不装** fatal 回调、**不轮转**、**不写日志**：
  - `StreamBodyOnce`：`StreamBody` 的 200/非 200 状态与 raw 搬运；
    `AuthPermanentlyRevokedError` 记 401；200 body 在 `BodyReader` 返回后恰好
    关闭一次；`BodyReader` 禁止 Close。
  - `GenerateImageOnce(ctx, token, accountID, transport, params)`：SDK
    `GenerateImage`；外层 `oneShotTransport` 捕获最后一次 HTTP 状态与响应体读出
    字节数，**typed success 返回真实状态**（201/204/206 不得冒充 200）+
    `MarshalImageResponse` 生成的 wire body；`*HTTPError` → 真实 status + raw；
    `AuthPermanentlyRevokedError` → 401 + raw；其他错误 → 捕获 status（无则 0）+
    err；200 读出 >8 MiB（含 typed success / parse 失败）→ `ErrBodyTooLarge`。
  - `SearchOnce(ctx, token, accountID, transport, payload)`：SDK `Search`；真实
    status + raw；revoked → 401 + raw；传输错误 → 0 + err；200 且 len(raw) >
    2 MiB → `ErrBodyTooLarge`（非 200 由调用方 cap 截断到恰好 2 MiB，不判溢出）。
- `ErrBodyTooLarge` 是本包导出的 200 溢出哨兵；probe 用 `errors.Is` 映射固定
  文案：StreamBodyOnce/SearchOnce 为 `response exceeds 2 MiB`，
  GenerateImageOnce 为 `response exceeds 8 MiB`。body 由 SDK 读取/关闭；调用方
  cap transport 保证对应上限（2 MiB / 8 MiB）。

## 不变量

- 不记录/不回显 token、payload、上游原文；SDK error 文本不进入结果。
- 生产 Codex 与 one-shot seam 分离：seam 不取 ClientTable、不装 fatal 回调、不轮转；
  两次调用之间没有共享账号状态（不缓存 fatal、不刷新）。
- 非 200 body 由 SDK 读尽并关闭；调用方 cap transport 负责对应上限。
- codex WS 面不喂任何客户端头（spec §12）；HTTP 面头策略仅在 `pkg/aiclient` 维护。
- 生成物不手改；日志只走 `pkg/logx`。

## 验证

```bash
go test ./internal/sdkbridge/ -count=1
go test ./internal/sdkbridge/ -run TestStreamBodyOnce -count=1
go test ./internal/sdkbridge/ -run 'TestGenerateImageOnce|TestSearchOnce|TestOneShot' -count=1
```
