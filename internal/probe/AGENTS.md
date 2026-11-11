# internal/probe — 单账号单模型探测执行

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲探测包的
> 契约与不变量（spec `account-model-test` §5.1/§5.2/§6/§7）。

## 职责

`Prober.Probe(ctx, domain.ProbeSpec)` 用 service 组装好的账号/模板事实发一条最小上游
请求，返回 `domain.ProbeResult`（success / latency_ms / status_code / 回显 model /
固定分类 error + 有限错误码 detail，均按 1024/2048 字节截断）。

- 静态 key 族（`api_key` / `responses-special` / `custom`）：`pkg/aiclient.Factory` 的
  `ChatCompletionRaw` / `ResponseRaw` / `AnthMessageRaw`，路径来自
  `domain.ResolveUpstreamPath`（与 proxy 热路径同一份键表/默认端点），`RelayHeaders(nil)`。
- codex 族（`codex-pat` / 未过期 `codex-oauth`）：`internal/sdkbridge.ProbeResponses`
  （SDK `StreamBody` HTTP arm；仅 200 成功、不跟随重定向）；**不走**生产
  `sdkbridge.Codex` 条目/Auth/失败回调/轮转。
- 不落库、不计费、不改账号状态、不进调度/规则/失败统计；日志由 service 记一条
  （本包不写日志）。

## 不可破坏的不变量

1. **出站目标只来自存储配置**：请求体/query 无 URL/host/路径字段；`BaseURL` +
   路径由 service 传入。
2. **origin 锁定**：`originGuard` 只放行 scheme + hostname + 规范化端口完全一致的
   请求（codex 固定 `https://chatgpt.com`）；reject 文案 `probe blocked request to …`。
3. **不跟随重定向**：静态族 `CheckRedirect = http.ErrUseLastResponse`；codex SDK
   `StreamBody` 自带 no-redirect client。3xx 原样作为失败结论（status_code=3xx）；
   malformed Location / redirect 解析错误归一为 `probe blocked cross-origin redirect`
   （status 0），不回显 Location/URL。
4. **响应体硬上限 2 MiB**：静态族 `NewCappedTransport` 对非 2xx 截断到 2 MiB、
   2xx 允许读到 2 MiB+1；codex 走 `NewCodexCappedTransport`，**只有 200** 允许
   2 MiB+1（溢出探测），其余状态（含 201/204/206/3xx/4xx/5xx）恰好 2 MiB，不享受
   +1 字节。调用方读到第 2 MiB+1 字节即判 `response exceeds 2 MiB` 并关闭。codex
   200 体增量扫描，只在**真正空行**（`\n` / `\r\n`）事件边界（或 EOF）判定
   `response.completed` / `response.failed` / `error` 终态并停止读取、关闭——仅含
   空白的行属于事件内容，不是边界；不得按未收齐的前缀提前停止（会截断多行事件并
   产生二次解析放大），上游不关流不得拖到超时。终态检测与下游校验必须用同一字段
   解析规则（行首空白先去掉，` data:` 同样是 data 行）；事件名
   `error`/`response.failed` 是失败信号并优先于 data.type 的
   `response.completed`，失败事件名也立即停止读取。读取保留独立扫描游标，只扫描
   新增字节（2 MiB 单行分 1 KiB 到达不会 O(n²) 重扫）；停在终态空行时只返回
   `[:lineEnd]`，不带同一次 read 的预读尾巴（否则有效终态会被误判成 2 MiB 溢出）。
   校验端是单遍流式扫描：单行 data 直接引用 body 切片，只有多行 data 才写复用
   缓冲，2 MiB 的 `data:0\n\n` 微事件洪泛分配为 O(body) 而不是 O(events)。
   两个 cap 的 `New*CappedTransport` 都只是测试 seam，生产装配只经
   `guardedHTTPClient` / `guardedCodexTransport`。
5. **并发/超时**：进程内信号量（默认 4）+ `context.WithTimeout`（默认 30s，常量，
   不开配置键）；信号量等待受同一 ctx 约束。任何超时/取消（含响应头已到但 body
   未读完、以及 SDK 吞掉非 200/401 预读 body 错误的情形）→ `status_code=0` + 固定
   `probe timed out` / `probe canceled`；映射 SDK error 前先检查 `ctx.Err()`。
   codex 非 200（含 201/204/3xx）保留真实状态码并按 HTTP 响应分类。
6. **JSON 深度守卫（≤128）**：任何上游可控 body / SSE 事件在交给递归的 `gjson`
   之前必须先过 `domain.ShallowJSON`（线性、零分配，正确跳过字符串与转义）。超深
   数据按「不可用/非终态」处理：静态判定成功 = false、`errorCodeDetail` = 空、
   依赖 data.type 的 SSE 事件被忽略且不是终态；禁止把 2 MiB 的 `[` 交给
   `gjson.ValidBytes`。**例外**：失败事件名 `response.failed` / `error` 是无条件
   终态，不解析 data、也不受深度守卫影响（data 为空/`[DONE]`/非 JSON 同样失败）。
7. **固定文案不做凭据子串替换**：结果字段只有分类白名单 + 有限错误码，没有动态
   上游文本；按 secret 做 substring 替换会用短 key（如 `up`）破坏
   `upstream rejected credentials` 等固定文案，代码中不保留逐 secret 替换调用。
8. **失败文案受控（根 AGENTS.md 反模式 7）**：结果与日志**不得**包含上游自由
   文本、原始 JSON、内部 ID 或其他凭据。`error` 只允许固定分类文案（probe
   timed out / probe canceled / probe blocked cross-origin redirect / probe
   request failed / upstream rejected credentials / upstream endpoint or model not
   found / upstream rate limited / upstream rejected request / upstream redirect
   rejected / upstream error / upstream response missing usable output / upstream
   stream failed / response exceeds 2 MiB）；`detail` 只允许**内置有限公开错误码
   表**内的值（invalid_request_error / invalid_api_key / model_not_found /
   context_length_exceeded / rate_limit_exceeded / rate_limit_error /
   insufficient_quota / server_error / api_error / overloaded_error /
   authentication_error / permission_error / not_found_error / request_too_large /
   content_policy_violation / usage_not_included / invalid_prompt /
   model_not_supported / unsupported_model / billing_hard_limit_reached /
   account_deactivated）以 `code=<value>` 形式给出；表外值（含任意形态的上游
   `error.code`/`error.type`、其他租户 key、内部请求 id）一律为空。绝不返回
   `error.message` 或原始 body。
