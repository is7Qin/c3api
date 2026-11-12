# internal/service — 业务逻辑与探测控制面

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲 service
> 目录职责与探测入口的控制面契约。

## 职责 / 入口

- 一资源一文件；错误哨兵在 `internal/service/errors`；全部持久化经 `Store`。
- 单账号单模型探测入口：`ProbeAccount`（`probe.go`）。它负责输入校验、组装
  `domain.ProbeSpec`、读取账号/模板/凭据、写一条 `account_probe` 审计。
- **准入**：`probe_limit.go` 的 `probeLimiter` 在**仓储读取之前**获取，容量严格为
  `domain.ProbeMaxConcurrency` 个执行槽 + `domain.ProbeMaxWaiters` 个等待位；
  等待位只在排队期间持有。队列满 → 固定 `probe busy`。
- **唯一总期限**：service 在输入校验后创建 `context.WithTimeout(ctx, domain.ProbeTimeout)`；
  store 读取、凭据读取和 `Prober.Probe` 共用这一个 `pctx`。每次 store 返回后、
  模板/出口/凭据校验后、OAuth 过期早退前、调用 prober 前都要先检查 `pctx.Err()`：
  已过期/取消 → `status_code=0` 的固定结论，不再触达后续依赖。
- **日志边界**：只有形成 200 探测结论的路径恰好一条 `account_probe`——准入
  busy/ctx、OAuth 过期早退、prober 结果。validation/404/500/未装配不写。日志字段
  只有账号/格式/模型/状态/延迟/success/error；`output_preview`、`image_previews`
  与请求字段绝不写日志。

## 探测输入契约（account-probe-content）

- `AccountProbeRequest` 的 `Input` / `ImageMode` / `ImageURL` 是 `*string` 三态：
  `nil` = 缺省或显式 null（同义）；非 nil = 显式值（**含空串**，不能压成 nil）。
- `probeFormats` 六值：chat / responses / responses-ws / anthropic / images / search。
- **全部无存储校验在 store 与准入之前完成**（非法输入只返回固定文案，不占探测槽）：
  - `input`：缺省/null → 格式默认值（文本 `hi`、images generations
    `A simple red apple on a white table`、images edits `Turn this image into a
    watercolor painting`、search `OpenAI`）；显式值先 `TrimSpace`，空 →
    `probe input is empty`，trim 后 UTF-8 字节 >8192 → `probe input exceeds 8192 bytes`。
  - `image_mode`：images 缺省/null → `generations`；空串/非法 →
    `probe image mode invalid`；非 images 出现任何非 null 值（含空串）→
    `probe image mode unexpected`。
  - `image_url`：任何非 null 值 UTF-8 字节 >6 MiB → `probe image url exceeds 6 MiB`；
    非 images 出现任何非 null 值（含空串）→ `probe image url invalid`；edits
    缺省/null/空串 → `probe image url required`，非空值必须 http(s) 或
    `data:image/<mime>;base64,...`（否则 `probe image url invalid`）；generations
    非空值 → `probe image url invalid`。
  - 校验错误文案固定、**不回显 image_url/原值**；`WriteServiceErr` 只能看到固定文案。
- 模板资格仍由 `Template.FormatSupports(format, model)` + `Serves/HasModelSpace`
  判定；`openai-images`/`openai-search` 必须显式出现在模板 `supported_formats`。
- 组装 `ProbeSpec` 时把已校验的 `Input`/`ImageMode`/`ImageURL` 原样传入；prober
  不再提供第二份默认值。

## 不变量

- 探测只读：不落库、不计费、不改账号状态、不进调度/规则/失败统计。
- 固定文案只引用 `domain.ProbeError*`，不在 service 重复字面量。
- `Prober == nil` 是装配错误（500），不降级为 503。
- 写库后的 publish/reload 副作用必须 `context.WithoutCancel`；不得跑在请求 ctx 上。
- 禁止新增 `Set*` 事后回填；构造环结构性消除。

## 验证

```bash
go test ./internal/service/ -run TestProbe -count=1
go test ./internal/service/ -run 'TestProbeLimiter|TestProbeDeadline|TestProbeLog' -count=1
go test ./internal/service/ -run 'TestProbeOptional|TestProbeImage|TestProbeMultibyte|TestProbeImagesSearch' -count=1
```
