# internal/handler/httpface — HTTP 书写与请求参数叶子包

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲
> 管理面/用户面共享的 HTTP 边界原语。

## 职责 / 入口

- `WriteJSON` / `WriteErr` / `WriteServiceErr`：响应书写与 service 哨兵→HTTP
  状态的 **唯一映射表**（404/400/409/412/403/401/503/502/500）。400 额外携带
  统计窗口的机读字段（`errors.As`，禁止字符串嗅探）。
- `Decode(r, v)`：严格 JSON 解码（`DisallowUnknownFields` + trailing-data 检查），
  **1 MiB** 请求体上限。
- `DecodeLimit(r, v, limit)`：同一实现的**可配置上限**版本；探测端点用
  `domain.ProbeMaxRequestBytes`（8 MiB）。语义（未知字段、尾随数据、字节上限）
  与 `Decode` 逐字节一致。
- `HasJSONKey` / `DecodeRaw`：需要「键存在性（含显式 null）」判据的端点先读原始
  JSON 再走严格结构校验（`DecodeRaw` 仍 1 MiB）。
- `Deref` / `Ptr` / `ClampLimit` / `LogLimit` / `ClipLogPage`：分页与指针小工具。

## 不变量

- 只依赖 `internal/domain` 与 `internal/service/errors` 叶子包；不反向依赖
  handler/service/auth/server（依赖图无环）。
- `Decode`/`DecodeLimit` 必须保留未知字段与 trailing-data 拒绝；不得改成宽松
  解码。上限是**字节**（`http.MaxBytesReader`）。
- 未命中哨兵的 service 错误一律 500 `internal error`，不泄露内部细节。
- 新增共享边界原语前先确认没有第二份本地副本。

## 验证

```bash
go test ./internal/handler/httpface/ -count=1
go test ./internal/handler/httpface/ -run TestDecodeLimit -count=1
```
