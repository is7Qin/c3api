# internal/handler — 管理面 HTTP 边界（oapi-codegen 路由 + 实现）

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲
> handler 根包（`/api/admin`）的接线与请求边界。

## 职责 / 入口

- `api.gen.go` 是 `openapi/openapi.yaml` 经 `go generate ./internal/handler/`
  生成的 **ServerInterface + 类型 + chi 路由**（生成物，禁止手改）。新增/修改
  管理端点：改 openapi → 重生成 → 在 `AdminAPI` 实现方法 → 跑 `pnpm gen:api`（web）。
- `handler.go` 的 `AdminAPI` 把生成接口转发到 `service.Service`；业务规则、
  哨兵错误、准入/期限全部在 service。用户面在 `user/`，供应商面在 `supplier/`；
  本包不含 `/v1`（在 `internal/proxy`）。
- 错误写出统一走 `internal/handler/httpface`：`WriteServiceErr` 是 service
  哨兵→HTTP 状态的唯一映射；未知错误一律 500 `internal error`（不回显内部细节）。
- 严格解码：管理面请求体默认 `httpface.Decode`（`DisallowUnknownFields` +
  trailing-data 检查 + 1 MiB 上限）。探测端点
  `PostAccountsIdTest` 使用 `httpface.DecodeLimit(..., domain.ProbeMaxRequestBytes)`
  （8 MiB），并把 `nullable.Nullable[string]` 的 `input/image_mode/image_url`
  按三态投影成 `*string`：缺席/null → nil，显式值（含空串）→ 非 nil。
- 探测结论 `AccountTestResult` 必须包含 `output_preview`（恒 string）与
  `image_previews`（恒数组；nil 必须写成 `[]`）。这些字段只回显给发起探测的
  管理员/账号 owner，绝不写日志。

## 不变量

- 生成物 `api.gen.go` 不手改；schema/required/nullable/enum/maxLength 变更只能
  通过 `openapi/openapi.yaml` + `go generate`。
- handler 只搬运请求/响应，不做业务校验；400 的字段语义由 service 决定。
- 探测端点一次请求只调用一次 `svc.ProbeAccount`；不写库、不计费、不改账号状态。
- 错误响应不得包含上游 body、URL、内部 ID、key/签名或凭据（根反模式 7）。
- 测试：`go test ./internal/handler/ -run 'TestPostAccountsIdTest|TestProbeSchemaContract'`；
  OpenAPI 契约由 `TestProbeSchemaContract` 锁住（nullable/enum/maxLength/required）。

## 验证

```bash
go generate ./internal/handler/
go test ./internal/handler/ ./internal/handler/httpface/ -count=1
```
