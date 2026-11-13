# internal/probe — 单账号单模型探测执行

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲探测包的
> 契约与不变量（spec `account-model-test` / `account-probe-content` §4/§5/§6；
> 控制面收敛见 `probe-control-plane`）。

## 职责

`Prober.Probe(ctx, domain.ProbeSpec)` **纯执行**一次已准入、带期限的探测：用 service
组装好的账号/模板事实发一条最小上游请求，返回 `domain.ProbeResult`（success /
latency_ms / status_code / 回显 model / 固定分类 error + 有限错误码 detail，均按
1024/2048 字节截断；文本四格式另有 `output_preview`，图片格式另有 `image_previews`）。
准入（4 执行 + 16 等待）与 30s 总期限由 service 拥有；本包不 `context.WithTimeout`、
不持有 sem/waiters，也不写日志。

- 静态 key 族（`api_key` / `responses-special` / `custom`）：`pkg/aiclient.Factory` 的
  `ChatCompletionRaw` / `ResponseRaw` / `AnthMessageRaw` / `ImagesRaw` / `SearchRaw`，
  路径来自 `domain.ResolveUpstreamPath`（与 proxy 热路径同一份键表/默认端点），
  `RelayHeaders(nil)`。
- codex 族（`codex-pat` / 未过期 `codex-oauth`）：`internal/sdkbridge.StreamBodyOnce`
  （文本四格式）、`sdkbridge.GenerateImageOnce`（images）、`sdkbridge.SearchOnce`
  （search）；只搬运 SDK 机制。请求体、200 body 读取、SSE 终态、成功判定与预览
  全部在本包。**不走**生产 `sdkbridge.Codex` 条目/Auth/失败回调/轮转。
- 不落库、不计费、不改账号状态、不进调度/规则/失败统计。

## 格式与输入（account-probe-content）

- `format` 六值：`openai-chat` / `openai-responses` / `openai-responses-ws` /
  `anthropic` / `openai-images` / `openai-search`。`openai-images` 由 `ProbeSpec.ImageMode`
  （`generations`/`edits`）选端点；`openai-search` 走 `/v1/alpha/search`。
- `ProbeSpec.Input` 已由 service 按格式填好缺省值并 trim/按 UTF-8 字节校验；本包
  **不提供第二份默认值**，只把它写入对应 wire 字段（chat `messages[].content`、
  responses `input[].content[].text`、anthropic `messages[].content`、images
  `prompt`、search `queries[]`）。
- images generations（静态族）：JSON `{model,prompt,response_format:"b64_json"}`。
- images edits（静态族）：先取 `ProbeSpec.ImageURL` 图片字节，再组 multipart
  （model/prompt/response_format=b64_json/image + 图片 Content-Type）；不内置任何
  测试图片。codex 族 edits 不提前取图，URL/data URL 直接交给 SDK。
- search 请求体固定字段形状：`{id,model,input:[{type:"web_search_call",
  request:{queries:[input]}}],commands:[],settings:{},max_output_tokens:256}`；
  `id` 恒为 `probe_<16 hex>`：进程启动时生成一次 64-bit seed（crypto/rand；失败
  回退纳秒时间戳），每次请求 `seed XOR atomic.Uint64.Add(1)`——同进程并发/重复
  探测不会共用 correlation id。

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
4. **响应体硬上限：文本/SSE/search 2 MiB，images 8 MiB**：静态族文本用
   `newCappedTransport` 对非 2xx 截断到 2 MiB、2xx 允许读到 2 MiB+1；images
   静态族用 `newCappedTransportLimit(..., ProbeMaxImageResponseBytes,
   ProbeMaxBodyBytes, true)`（**只有 200** 允许 8 MiB+1；非 200 恰好 2 MiB）。
   search 用 `newCappedTransportLimit(..., ProbeMaxBodyBytes, ProbeMaxBodyBytes,
   true)`（只有 200 允许 2 MiB+1）。codex 文本/search 2 MiB，codex images
   8 MiB（非 200 仍 2 MiB），均由 `probeResponseLimits` 选择。调用方读到第
   max+1 字节即判 `response exceeds 2 MiB`（images 为 `response exceeds 8 MiB`）。
   codex 200 体由**唯一**的 `scanCodexBody` 增量扫描：
   只在**真正空行**（`\n` / `\r\n`）事件边界（或 EOF）判定 `response.completed` /
   `response.failed` / `error` 终态并停止读取——仅含空白的行属于事件内容，不是
   边界；不得按未收齐的前缀提前停止（会截断多行事件并产生二次解析放大），上游
   不关流不得拖到超时。终态检测与成功/失败分类共用同一字段解析规则（行首空白
   先去掉，` data:` 同样是 data 行）；事件名 `error`/`response.failed` 是失败
   信号并优先于 data.type 的 `response.completed`。读取保留独立扫描游标，只扫
   新增字节（2 MiB 单行分 1 KiB 到达不会 O(n²) 重扫）；停在终态空行时只返回
   `[:lineEnd]`，不带同一次 read 的预读尾巴。单行 data 直接引用 body 切片，只有
   多行 data 才写复用缓冲，2 MiB 的 `data:0\n\n` 微事件洪泛分配为 O(body)。
   cap 构造私有；生产只经 `guardedHTTPClient` / `guardedHTTPClient200Only` /
   `guardedCodexTransport`。`guardedCodexTransport` 包的是出口的 `eg.Codex.Transport`
   （关闭自动压缩的 Codex 底座，与生产 sdkbridge 同源）；`eg.Codex == nil` 只有测试
   字面量，回退 `eg.HTTP.Transport`。
5. **期限/准入边界在 service**：本包只读 `ctx.Err()` 并映射状态 0 的固定文案；
   任何超时/取消（含响应头已到但 body 未读完、SDK 吞掉非 200/401 预读 body
   错误、以及图片取图阶段）必须优先于真实 HTTP 状态、响应体上限溢出与状态分类。
   codex 非 200（含 201/204/3xx）保留真实状态码并按 HTTP 响应分类。service 在
   进入执行器前拒绝软删账号（404）与超过 `domain.ProbeMaxModelLen`（256 字节）的模型。
6. **JSON 深度守卫（≤128）**：任何上游可控 body / SSE 事件在交给递归的 `gjson`
   之前必须先过 `domain.ShallowJSON`（线性、零分配，正确跳过字符串与转义）。超深
   数据按「不可用/非终态」处理：静态判定成功 = false、`errorCodeDetail` = 空、
   依赖 data.type 的 SSE 事件被忽略且不是终态；禁止把 2 MiB 的 `[` 交给
   `gjson.ValidBytes`。**例外**：失败事件名 `response.failed` / `error` 是无条件
   终态，不解析 data、也不受深度守卫影响。
7. **固定文案单一来源**：`domain.ProbeError*` 常量是唯一词表；本包不重复字面量。
   结果字段只有分类白名单 + 有限错误码，没有动态上游文本；不做逐 secret 替换。
8. **失败文案受控（根 AGENTS.md 反模式 7）**：错误路径（status != 200，含
   201/204/206、取图失败）的结果与日志**不得**包含上游自由文本、原始
   JSON/SSE/envelope、URL、内部 ID 或其他凭据。`detail` 只允许 `probeErrorCodes`
   内置有限公开错误码表内的值（以 `code=<value>` 给出）；表外值一律为空。绝不
   返回 `error.message` 或原始 body。
9. **codex one-shot seam**：`sdkbridge.StreamBodyOnce` / `GenerateImageOnce` /
   `SearchOnce` 只构造临时 PAT client 并搬运 HTTP 状态/raw；body 由 seam 恰好关闭
   一次。生成图片 typed success 的真实状态由 status-capture transport 取回，严禁
   把 201/204/206 冒充 200；200 读出 >上限（StreamBodyOnce/SearchOnce 2 MiB、
   GenerateImageOnce 8 MiB）→ `sdkbridge.ErrBodyTooLarge` → 固定
   `response exceeds 2 MiB` / images `response exceeds 8 MiB`。
10. **新 images/search 成功 = HTTP 200 + 可解析可用输出**：status != 200（含
    201/204/206）保留真实 `status_code`、`error=classifyUpstreamStatus(status)`、
    `detail` 只从该 body 提取白名单 code，预览为空；status 200 但不可用 →
    `upstream response missing usable output`。文本四格式保持既有静态族 2xx /
    codex 200 成功语义。
11. **安全预览（唯一窄例外）**：仅在 HTTP 200 且成功判定通过后抽取。文本四格式
    按确定性字段路径（chat choices/message.content、responses output_text→
    output[].content[].output_text、anthropic content[]text、codex SSE 终态
    `response.completed` 的 `data.response`）；search 只取 `output` 字符串，为空
    时按顺序取 `results[].web_search.title/content`（**url 永不投影**）；images
    的 `output_preview` 恒为空，`image_previews` 只取 `data[].b64_json`。先抽取、
    再脱敏（`sanitizePreview`：任意 scheme://、mailto:、data:、www.、Bearer、
    sk/pk/mk/pat/rk/ak token、req/resp/msg/chatcmpl/call/fc/toolu/run/thread/
    asst/file/img/igc ID、UUID，大小写不敏感），最后按
    `domain.ProbePreviewMaxLen`（64 KiB）UTF-8 边界截断。脱敏只作用于
    `output_preview`；`image_previews` 是经 base64+PNG/JPEG/WebP 魔数校验后重编码
    的 `data:image/<mime>;base64,...`（单张解码 ≤2 MiB、合计 ≤4 MiB、最多 4 张，
    不合格图片丢弃）。成功文本是 best-effort redacted model output，**不是
    formal secret boundary**；只返回给管理员/账号 owner，**绝不写日志**，不进入
    usage/err/rule，也不得用于错误分类。原始 JSON/SSE/envelope 绝不回显。
12. **图片取图（静态族 edits）**：`image_url` 由 service 校验为 http(s) 或
    `data:image/<mime>;base64,...`。data URL 本地解码（坏 base64 → `probe image
    invalid`，解码 >4 MiB → `probe image exceeds 4 MiB`）；http(s) 用账号出口但
    **不套 base_url origin guard**，必须：克隆 client 后清空 Jar、全新空请求头
    （仅 `Accept: image/*` 与 `User-Agent`，绝不携带入站头/账号 key/
    Authorization/x-api-key/ChatGPT-Account-ID/Cookie）、不跟随重定向、HTTP 200、
    Content-Type `image/*`（缺失时 `http.DetectContentType`）、原始字节 ≤4 MiB。
    失败映射唯一：非 200/重定向/连接失败 → `probe image fetch failed`；非图片/
    坏 base64 → `probe image invalid`；超 4 MiB → `probe image exceeds 4 MiB`；
    ctx 取消/到期 → `probe canceled`/`probe timed out`（优先）。失败时
    `status_code=0`、latency 含取图耗时、预览为空、上游图片端点 0 命中。
13. **codex 新格式结论顺序**：ctx 取消/到期 > `ErrBodyTooLarge` > (status != 0
    且 err != nil → 按真实 HTTP 状态分类) > 传输分类（status=0）。201/204/206
    必须报 `upstream error` 而不是 `probe request failed`。

## 验证

```bash
go test ./internal/probe/ ./internal/sdkbridge/ ./internal/service/ -count=1
go test ./internal/probe/ -run TestProbeCodex -count=1
go test ./internal/probe/ -run 'TestProbeDeepJSON|TestProbeCodexNewlineFlood|TestProbeCodexTinyEventFlood' -count=1
go test ./internal/probe/ -run 'TestProbe.*Images|TestProbe.*Search|TestProbe.*Preview|TestSanitizePreview' -count=1
go test ./internal/probe/ -run TestProbeCodexTransportNoAutoGzip -count=1
```

## 最近变更

- 2026-11-13 `e0744c9`（codex-conversation-integration §5.5）：`guardedCodexTransport`
  改用 `eg.Codex` 底座（codex 探测出站不带自动 `Accept-Encoding: gzip`）。
