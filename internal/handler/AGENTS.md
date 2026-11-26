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
- 错误写出统一走 `internal/httpface`：`WriteServiceErr` 是 service
  哨兵→HTTP 状态的唯一映射；未知错误一律 500 `internal error`（不回显内部细节）。
- 严格解码：管理面请求体默认 `httpface.Decode`（`DisallowUnknownFields` +
  trailing-data 检查 + 1 MiB 上限）。探测端点
  `PostAccountsIdTest` 使用 `httpface.DecodeLimit(..., domain.ProbeMaxRequestBytes)`
  （8 MiB），并把 `nullable.Nullable[string]` 的 `input/image_mode/image_url`
  按三态投影成 `*string`：缺席/null → nil，显式值（含空串）→ 非 nil。
- `codex_account_id.go`：**codex account id 派生策略的唯一实现**（`deriveAccountID`）
  + 在线查询端口 `AccountIDResolver`。
  - `func (h *AdminAPI) deriveAccountID(ctx, typ credential.Type, oauthTok, patKey string) string`
    收拢了此前散在 3 处的同一条策略（`ext_codex.go` 单账号保存 + `ext_codex_import.go`
    两个批量导入的内联循环）：OAuth 走离线解 JWT（`codexproto.DeriveAccountID`，纯函数），
    PAT 走端口在线 whoami，其它类型不派生；**失败/不可派生一律返回 `""`，不阻塞保存**
    （交给 service 层行级必填校验兜底，留空 → 下次保存重试）。
  - 为什么在 handler 私有方法而不是 service/domain：两家都没有「取 account id」的调用链，
    只有管理面需要；下沉是虚假抽象。真有第二个消费者时再上提。
  - **`AccountIDResolver` 的 nil 语义**：未装配 ⇒ **只**对 PAT 分支留空（不 500、不 panic、
    不阻塞保存）；OAuth 分支是纯函数，与端口无关，永远可用。组合根恒装配
    `sdkbridge.PATWhoami{}`，由 `cmd/server/account_id_resolver_wiring_test.go` 的 AST 门禁钉住
    「必须经 `OpsOptions.AccountIDResolver` 构造注入、不许回填 setter」。
  - `AccountIDResolver` **与 `CodexUsageProber` 是两个端口，不要合并**：读额度快照 ≠
    查 account id。合并只是让注入项少一个，却迫使任何只要一边的装配方实现另一边。
- 探测结论 `AccountTestResult` 必须包含 `output_preview`（恒 string）与
  `image_previews`（恒数组；nil 必须写成 `[]`）。这些字段只回显给发起探测的
  管理员/账号 owner，绝不写日志。
- `pricing_port.go`：`PricingSync` 是**本包声明的消费方接口**（只有 `SyncNow` +
  `Preview` 两个方法，返回 `internal/pricingresult` 的中立结果值）。实现在
  `internal/pricing`（方法集是超集 ⇒ 自动满足），由组合根 `cmd/server` 注入到
  `OpsOptions.PricingSync`。**不要把字段类型改回 `*pricing.SyncWorker`**——那会让
  本包重新认识 pricing 的 fetcher/repo/settings/cron/worker 实现面（就是被清掉的那条债）。

## 不变量

- 生成物 `api.gen.go` 不手改；schema/required/nullable/enum/maxLength 变更只能
  通过 `openapi/openapi.yaml` + `go generate`。
- handler 只搬运请求/响应，不做业务校验；400 的字段语义由 service 决定。
- 探测端点一次请求只调用一次 `svc.ProbeAccount`；不写库、不计费、不改账号状态。
- 错误响应不得包含上游 body、URL、内部 ID、key/签名或凭据（根反模式 7）。
- **`/accounts/usage` 的用量错误分类判类型，不判哨兵**：`errors.As(err, &ce)` 取
  `*codexproto.Error` 的 `Kind`（`ErrKindAuthExpired` → `auth_expired`；
  `ErrKindUpstream` → `upstream_unavailable`），未知错误走 `default` → `null/null` +
  Warn，**不误标**。两处都不要退回：
  - **不要**判 `sdkbridge.ErrAuthExpired` / `sdkbridge.ErrUpstream`——那正是本包
    → sdkbridge 那条 P6 债的来源（P6-2 已清）；实现方换哨兵、加包装层都不该影响分类；
  - **不要**判通用的 `serviceerr.ErrUpstream`——它被多处共享生产，会把无关上游故障
    误标成 codex 用量上游错误。
  分类类型住中立叶子包 `internal/codexproto`（见该包 `AGENTS.md`）。
- **本包禁止 import `internal/sdkbridge`**（直接边）。account id 的离线解析与用量错误
  的分类都由 `internal/codexproto` 承载；在线 whoami 走上面那个端口。
  ⚠ `go list -deps ./internal/handler` 仍会列出 sdkbridge——那是经 `service → probe`
  的传递依赖（P6-2 之前就存在），与本包的**直接边**不是一件事。
- **`/pricing/sync` 与 `/pricing/sync/preview` 的 fetch 失败都判 `*pricingresult.FetchError`
  （`errors.As`），并写固定文案 `"pricing sync failed"`**。两条都不要退回：
  - **不要**判哨兵（`pricing.ErrPriceFetch` / `pricingresult` 里的任何变量名）——那会让本包
    认识实现方的变量；
  - **不要**判通用的 `serviceerr.ErrUpstream`——它被 `codex_login`/`payment` 等多处共享生产，
    会把无关上游故障误标成价格同步失败；
  - **不要**改走 `WriteServiceErr` 兜住 `FetchError`——那个 502 分支会把 `err.Error()`
    原样写进响应体（含上游 fetch 原文），既是行为漂移也是信息泄漏。
  门禁：`TestPricingSyncPredicateIsPrecise`（`pricing_port_test.go`）。
- **`POST /pricing/sync/preview` 目前不返回 `entries`**（只回 `to_add/to_update/skipped/
  variants_changed` 四个 int）。契约里已定义 `PricingSyncPreviewResponse.entries`、
  `pricingresult.Preview.Entries` 也已写好并有 pricing 侧测试，所以"顺手补全"是一次**合理
  的独立特性**——但它不是解耦的副作用，**不要在这个口径下顺手做**（单独开单）。
- 测试：`go test ./internal/handler/ -run 'TestPostAccountsIdTest|TestProbeSchemaContract'`；
  OpenAPI 契约由 `TestProbeSchemaContract` 锁住（nullable/enum/maxLength/required）。

## 验证

```bash
go generate ./internal/handler/
go test ./internal/handler/ ./internal/httpface/ -count=1

# P6-2 相关：派生策略 + nil 降级 + 错误分类口径
go test -count=1 -run 'TestDeriveAccountIDTypes|NilResolverDegrades|UsageClassificationByKind' ./internal/handler/
# 装配门禁（cmd/server）：端口必须经 OpsOptions 构造注入
go test -count=1 -run TestAccountIDResolverCtorWiring ./cmd/server/
# 直接 import 必须为空（传递依赖见上文说明）
go list -f '{{join .Imports "\n"}}' ./internal/handler | rg -x 'github.com/is7qin/c3api/internal/sdkbridge'
```
