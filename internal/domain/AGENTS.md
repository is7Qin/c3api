# internal/domain — 共享类型与纯规则

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲
> domain 的边界与探测词表。

## 职责 / 入口

- 跨层共享的纯数据类型/常量/纯函数（`Template`、`Account`、`ProbeSpec/Result`、
  路由与模型映射、`ShallowJSON`、额度 admission 等）。
- **不做 IO**、不持有连接/缓存、不 import service/repository/probe。
- 探测固定结果词表：`ProbeError*` 常量是唯一来源；`ProbeContextError` 只把
  `context.Canceled/DeadlineExceeded` 归一到固定文案，其余返回 request failed。
  openapi 描述不是完整词表，不得作为第二份定义。
- 探测硬上限集中在 `domain/probe.go`：`ProbeMaxBodyBytes`（2 MiB 文本/SSE/search
  上游体）、`ProbeMaxImageResponseBytes`（8 MiB images 上游体；b64_json 正常结果
  会超过 2 MiB）、`ProbeMaxRequestBytes`（8 MiB 探测请求体）、`ProbeMaxInputLen`（8192 字节）、
  `ProbeMaxImageSourceBytes`（6 MiB image_url 字符串）、`ProbeMaxImageBytes`
  （4 MiB 取图/预览合计；单张预览复用 2 MiB 的 `ProbeMaxBodyBytes`）、`ProbePreviewMaxLen`（64 KiB）、`ProbeMaxImagePreviews`
  （4 张）、`ProbeTimeout/ProbeMaxConcurrency/ProbeMaxWaiters/ProbeMaxModelLen/
  ProbeMaxJSONDepth`。
- `ProbeImageModeGenerations/Edits` 与 `ProbeDefaultInput*` 是 image mode 与输入
  默认值的唯一常量来源（service 读取，prober 不复制）。
- `ProbeSpec` 携带已校验的 `Input/ImageMode/ImageURL`；`ProbeResult` 携带
  `OutputPreview`（≤64 KiB 脱敏文本）与 `ImagePreviews`（校验后的 data URL 列表）。

## 不变量

- 新增/修改字段先登记 openapi/生成物流程；生成物不手改。
- 热路径共享类型保持值语义/零分配友好；不得引入每请求锁或 DB 依赖。
- `ShallowJSON` 必须在任何递归 JSON 解析前拦截上游可控深层数据。
- 文案变更属于 API 契约变更；必须逐字保留既有值，除非用户明确裁决。

## 验证

```bash
go test ./internal/domain/ -count=1
go test ./internal/probe/ ./internal/service/ -run TestProbe -count=1
```
