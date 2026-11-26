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
- `template_upstream.go` / `request_patch.go` / `request_patch_apply.go` / `request_patch_headers.go`：custom 模板出站请求修饰器的**值类型与唯一实现**——`TemplateUpstream{Paths, Patch}`（`NewTemplateUpstream` 是唯一装配/归一入口；custom 门内一次构造，非 custom ⇒ nil）、`RequestPatch` / `FormatPatch`、`ParseRequestPatch`（wire 唯一解析入口：头名规范形/同层歧义/`Accept-Encoding` 硬拒/七键/path 文法/三态归一并保留大整数）、`CanonicalBytes`（写库与 scheduler digest 共用的确定性字节；digest 必须用**读回/解析后**的值）、`ApplyRequestBodyPatch`（纯函数执行器：delete 排序 → set 排序、非 JSON 结构性跳过、失败原 body 返回）。**数据面执行核心**（`request_patch_headers.go`，P5 起 HTTP/WS/probe 三面唯一实现）：`RequestPatchHeaderLayers`（层选择）、`ApplyRequestPatchHeaderLayers`（两层 Set/Del + Host 三态 + 运行期跳 `Accept-Encoding` + `{api_key}` 展开）、`RequestPatchHeaderDecorator`（HTTP 装饰器唯一构造，proxy 与 probe 共用）、`RequestPatchFormat`（值拷贝取格式层）、`ApplyRequestPatch`（body 选择+执行唯一入口；返回 err 由调用面处理）。本包只做保存期校验与纯数据变换（不写日志/不做 IO）：残余失败的固定文案限频 WARN 是 `internal/proxy` 的实现，HTTP/WS 直接调用、probe 由组合根注入 `proxy.PatchWarnFunc`——**domain 不 import logx、不认识「日志」**；头 wire 判据在 `pkg/headervalid`（全仓单源）。

## 不变量

- 新增/修改字段先登记 openapi/生成物流程；生成物不手改。
- 热路径共享类型保持值语义/零分配友好；不得引入每请求锁或 DB 依赖。
- `TemplateUpstream` / `RequestPatch` 在快照内构造后**只读**：数据面只拿指针、不复制不修改；`RequestPatch` 的解析/序列化判据只有 `request_patch.go` 一份，数据面头/body 判据只有 `request_patch_headers.go` 的五个函数一份——proxy/probe 都只做零逻辑委托，不得复制。
- `ShallowJSON` 必须在任何递归 JSON 解析前拦截上游可控深层数据。
- **`RuleKind`（`rulekind.go`）的数值与顺序不可变**：它是「规则事件类别」的
  canonical 定义，`internal/rule` 用 `type Kind = domain.RuleKind` 别名继续对外
  提供 `rule.Kind`/`rule.Kind*`，全仓 19 个既有引用点因此零改动。把它放在
  domain 而**不是** rule，是因为用户面 handler 也要做「错误类型 → 类别」的映射，
  却不该 import 整个规则引擎（含匹配/惩罚/持久化，属执行面）；类别是纯枚举，是
  两面共享的最小事实。`String()` 随类型一起在这里（别名类型不能在外部包定义
  方法）。**新增只能追加到末尾**——rule 侧有 `f.Kind == 0` 这类判据（0 = 不投递
  健康），中间插入会让它错；rule 的 `kindFromString` 解析表与本表逐值互逆，改值
  等于改线缆/日志格式。
- 文案变更属于 API 契约变更；必须逐字保留既有值，除非用户明确裁决。

## 验证

```bash
go test ./internal/domain/ -count=1
go test ./internal/probe/ ./internal/service/ -run TestProbe -count=1
```
