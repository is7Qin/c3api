# internal/probe — 单账号单模型探测执行

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲探测包的
> 契约与不变量（spec `account-model-test` §5.1/§5.2/§6/§7）。

## 职责

`Prober.Probe(ctx, domain.ProbeSpec)` 用 service 组装好的账号/模板事实发一条最小上游
请求，返回 `domain.ProbeResult`（success / latency_ms / status_code / 回显 model /
脱敏 error+detail）。

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
   `StreamBody` 自带 no-redirect client。3xx 原样作为失败结论（status_code=3xx）。
4. **响应体硬上限 2 MiB**：`capTransport` 对非 2xx 截断到 2 MiB；2xx 允许读到
   2 MiB+1，调用方读到第 2 MiB+1 字节即判 `response exceeds 2 MiB` 并关闭。
5. **并发/超时**：进程内信号量（默认 4）+ `context.WithTimeout`（默认 30s，常量，
   不开配置键）；信号量等待受同一 ctx 约束。
6. **脱敏**：凭据与绑定代理密码的原文 / JSON 转义 / URL 编码（QueryEscape+
   PathEscape）全部替换 `***`，再按 1024/2048 字节截断 error/detail。
7. **结果文本只放解码后的错误文本或 SSE 终态摘要**，不放未解码原始 JSON，也不回显
   上游成功响应体。
