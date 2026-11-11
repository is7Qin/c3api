# internal/probe — 单账号单模型探测执行

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲探测包的
> 契约与不变量（spec `account-model-test` §5.1/§5.2/§6/§7；控制面收敛见
> `probe-control-plane`）。

## 职责

`Prober.Probe(ctx, domain.ProbeSpec)` **纯执行**一次已准入、带期限的探测：用 service
组装好的账号/模板事实发一条最小上游请求，返回 `domain.ProbeResult`（success /
latency_ms / status_code / 回显 model / 固定分类 error + 有限错误码 detail，均按
1024/2048 字节截断）。准入（4 执行 + 16 等待）与 30s 总期限由 service 拥有；本包
不 `context.WithTimeout`、不持有 sem/waiters，也不写日志。

- 静态 key 族（`api_key` / `responses-special` / `custom`）：`pkg/aiclient.Factory` 的
  `ChatCompletionRaw` / `ResponseRaw` / `AnthMessageRaw`，路径来自
  `domain.ResolveUpstreamPath`（与 proxy 热路径同一份键表/默认端点），`RelayHeaders(nil)`。
- codex 族（`codex-pat` / 未过期 `codex-oauth`）：`internal/sdkbridge.StreamBodyOnce`
  （SDK `StreamBody` HTTP arm；只搬运 SDK 机制）；请求体、200 body 读取与 SSE
  终态全部在本包。**不走**生产 `sdkbridge.Codex` 条目/Auth/失败回调/轮转。
- 不落库、不计费、不改账号状态、不进调度/规则/失败统计。

## 不可破坏的不变量

1. **出站目标只来自存储配置**：请求体/query 无 URL/host/路径字段；`BaseURL` +
   路径由 service 传入。
2. **origin 锁定**：`originGuard` 只放行 scheme + hostname + 规范化端口完全一致的
   请求（codex 固定 `https://chatgpt.com`）；内部 reject 文案
   `probe blocked request to …` 不作为结果。
3. **不跟随重定向**：静态族 `CheckRedirect = http.ErrUseLastResponse`；codex SDK
   `StreamBody` 自带 no-redirect client。3xx 原样作为失败结论（status_code=3xx）；
   malformed Location / redirect 解析错误归一为 `probe blocked cross-origin redirect`
   （status 0），不回显 Location/URL。
4. **响应体硬上限 2 MiB**：静态族 `newCappedTransport` 对非 2xx 截断到 2 MiB、
   2xx 允许读到 2 MiB+1；codex 走 `newCodexCappedTransport`，**只有 200** 允许
   2 MiB+1（溢出探测），其余状态（含 201/204/206/3xx/4xx/5xx）恰好 2 MiB，不享受
   +1 字节。调用方读到第 2 MiB+1 字节即判 `response exceeds 2 MiB`。
   codex 200 体由**唯一**的 `scanCodexBody` 增量扫描：只在**真正空行**
   （`\n` / `\r\n`）事件边界（或 EOF）判定 `response.completed` /
   `response.failed` / `error` 终态并停止读取——仅含空白的行属于事件内容，不是
   边界；不得按未收齐的前缀提前停止（会截断多行事件并产生二次解析放大），上游
   不关流不得拖到超时。终态检测与成功/失败分类共用同一字段解析规则（行首空白
   先去掉，` data:` 同样是 data 行）；事件名 `error`/`response.failed` 是失败
   信号并优先于 data.type 的 `response.completed`。读取保留独立扫描游标，只扫
   新增字节（2 MiB 单行分 1 KiB 到达不会 O(n²) 重扫）；停在终态空行时只返回
   `[:lineEnd]`，不带同一次 read 的预读尾巴。单行 data 直接引用 body 切片，只有
   多行 data 才写复用缓冲，2 MiB 的 `data:0\n\n` 微事件洪泛分配为 O(body)。
   cap 构造私有；生产只经 `guardedHTTPClient` / `guardedCodexTransport`。
5. **期限/准入边界在 service**：本包只读 `ctx.Err()` 并映射状态 0 的固定文案；
   任何超时/取消（含响应头已到但 body 未读完、以及 SDK 吞掉非 200/401 预读 body
   错误的情形）必须优先于真实 HTTP 状态。codex 非 200（含 201/204/3xx）保留真实
   状态码并按 HTTP 响应分类。service 在进入执行器前拒绝软删账号（404）与超过
   `domain.ProbeMaxModelLen`（256 字节）的模型。
6. **JSON 深度守卫（≤128）**：任何上游可控 body / SSE 事件在交给递归的 `gjson`
   之前必须先过 `domain.ShallowJSON`（线性、零分配，正确跳过字符串与转义）。超深
   数据按「不可用/非终态」处理：静态判定成功 = false、`errorCodeDetail` = 空、
   依赖 data.type 的 SSE 事件被忽略且不是终态；禁止把 2 MiB 的 `[` 交给
   `gjson.ValidBytes`。**例外**：失败事件名 `response.failed` / `error` 是无条件
   终态，不解析 data、也不受深度守卫影响。
7. **固定文案单一来源**：`domain.ProbeError*` 常量是唯一词表；本包不重复字面量。
   结果字段只有分类白名单 + 有限错误码，没有动态上游文本；不做逐 secret 替换。
8. **失败文案受控（根 AGENTS.md 反模式 7）**：结果与日志**不得**包含上游自由
   文本、原始 JSON、内部 ID 或其他凭据。`detail` 只允许 `probeErrorCodes` 内置
   有限公开错误码表内的值（以 `code=<value>` 给出）；表外值一律为空。绝不返回
   `error.message` 或原始 body。
9. **codex one-shot seam**：`sdkbridge.StreamBodyOnce` 只构造临时 PAT client 并
   搬运 HTTP 状态/raw；200 body 由它恰好关闭一次，`BodyReader` 禁止 Close，也不
   构造 payload/SSE/日志。

## 验证

```bash
go test ./internal/probe/ ./internal/sdkbridge/ ./internal/service/ -count=1
go test ./internal/probe/ -run TestProbeCodex -count=1
go test ./internal/probe/ -run 'TestProbeDeepJSON|TestProbeCodexNewlineFlood|TestProbeCodexTinyEventFlood' -count=1
```
