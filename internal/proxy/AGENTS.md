# internal/proxy — /v1 网关热路径

> 根 `AGENTS.md` 覆盖项目级约定（CODE MAP 列了本包核心符号）；`internal/AGENTS.md` 是
> 分层地图。本文件只讲 proxy 的入口、热路径红线、续接与 Codex 分流。

## 职责 / 入口

- `router.go` `AIRouter(p)` 挂 `/v1/*`；`forward_*.go` 是各格式 HTTP 面（`HandleChat` /
  `HandleResponses` / `HandleAnthropic` / `HandleImages*` / `HandleSearch` / `HandleModels`），
  `caller_responses_ws.go` `HandleResponsesWS` 是 WS 面。
- `pipeline.go`：`guardPipeline`（鉴权→quota→余额预检→两级并发门→限流）与
  `failoverLoopWithPlan`（共享 failover 骨架，`attemptState` 按值传入）。
- `caller.go`：`handleFormat`（四种 REST 格式共享入口）→ 选号 →
  `resolveFormatContinuation` → failover 循环；`caller_*.go` 一格式一 `UpstreamCaller`。
- Codex 分流（按当轮 `sel.CredentialType.IsCodex()`）：`callCodexResponses`
  （`codex_responses_http.go`，直连流式/非流式）、`callConvertedCodexResponses`
  （`caller_converted.go`，协议转换车道）、`codex_responses_ws.go`（WS）、
  `caller_images_codex.go`；SDK 适配在 `internal/sdkbridge`。
- Codex 会话路径：`codexRequestPrep` / `conversationRequest`（HTTP）与 `dialCodexWS`（WS）；
  会话簿 `proxy.Deps.Conversations`（`*codexsdk.ConversationBook`，main 恒构造）。
- SDK 本地拒绝：`codex_local_reject.go`（`isCodexInvalidBody`、`finishCodexInvalidBody`、
  进程级计数 `convLocalRejects`）。
- 续接：`continuation.go`（`contResolve` / `contPin` / `contEnqueue` / `contBind`）、
  `continuation_bind.go`（`ContBindWorker` 异步批量绑定）、`ws_relay.go`（WS 同步 ACK 闸门）。
- 常驻 worker：`ContBindWorker`（`cont-bind`）、`ConversationWorker`
  （`codex_conversation_worker.go`，`codex-conversation`）。

## 关键不变量

- 热路径零 DB、零每请求锁、**零新增分配**（`pipeline.go` `guardPipeline` 注释；不用
  release 闭包，两条 defer 展开）。入口取对话标识也不例外（`TestFormatEntryConversationNoNewAllocs`）。
- **对话标识**（入口 `reqMeta.hasConv/convHeader/convBody`）：顺序为头 `Thread-Id`、`Session-Id`、
  `X-Session-Id`（规范形键直查 `r.Header` map，不得用小写键 `Get`），然后体键
  `prompt_cache_key`、`conversation_id`、`session_id`（复用 `scanKeys` 结果；WS 取首帧）。
  入口只记原始切片，物化只在 Codex 尝试里构造 `ConversationKey` 时发生；转换车道用入口
  原始头与原始体。标识只进 `ConversationKey`（SHA-256）与粘性哈希，不入日志、不出网。
- **会话路径 vs 槽位路径**：`AttemptPlanIdentity.Conversation = rm.hasConv && p.conversations != nil`
  （会话簿为 nil 时回到槽位，避免匿名身份）。会话路径每次尝试按当次 `sel` 重新
  `book.Prepare`（跨缓存域故障转移时对话键随之改变）；槽位路径用
  `sel.IdentityFor(!AnalyzeConversationInput(payload).FollowUp())`，不带也不捕获 turn-state。
  WS 首帧按当次 `sel` 改写模型，同一份字节同时给拨号（`WithConversation`/`WithFirstFrame`）
  与 relay 写出——二者必须逐字节相同。
- 续接 Redis 只在 Responses 带 `previous_response_id` 时发生；普通请求续接零 Redis
  （`TestContinuationOrdinaryRequestsZeroRedis`）。Codex 会话路径的 Redis 访问由 SDK
  会话簿按设计 §3 发生（本副本命中零 Redis，`TestCodexConversationLocalHitZeroRedis`），
  回写由 `ConversationWorker` 异步批量完成，不在续接代码里。
- 硬续接（API key 账号）：missing/expired/Redis 不可用一律 fail-closed（410/503/409），
  零上游拨号；钉选后 `hardContinuation=true`，绝不迁移到其他账号。
- **Codex 不写续接绑定**：没有任何 Codex 传输写续接绑定（W1/W2/W4/W5 在 Codex 分流之后、
  W6/W7 的 `contTag` 对 Codex 为空，均不可达，代码处有注释）。`resolveContinuationBinding`
  （HTTP 与 WS 共用）：计划里没有任何非 Codex 候选（`Scheduler.PlanHasNonCodexCandidate`
  为 false，纯 Codex 组）→ 不做续接解析，不查 Redis，续接存储故障也不 503，
  `previous_response_id` 交给 SDK。有非 Codex 候选时照常先 `contResolve`：命中 → 照旧
  `contPin`（API key 产出的 id 即使首个预留是 Codex 也钉回原账号）；未命中且首个预留是
  Codex → 不 410、`hardContinuation=false`，id 交给 SDK；未命中且首个预留不是 Codex →
  照旧 410；Redis 不可用 → 503（无法判断是否存在 API key 绑定，放行会让 Codex 静默丢
  上下文）。混合组里 Codex 产出的 id 是否 410 取决于首个预留落在哪类账号（不确定，已认可）。
- **SDK 本地拒绝**（`codexsdk.ErrInvalidRequestBody`，请求体不能整形：非 JSON 对象、缺字符串
  `model`、`input` 形态错、`service_tier` 含控制字节等）是客户端错误：四个落点——HTTP 两个
  core 的 SDK 错误分支、`handleCodexDialError` 开头、`codexFirstFrameFailed`（首帧）、
  `relayWS` client-loop（后续帧）——一律 `errors.Is` 识别，记录 400/`Err4xx`，**不 Classify、
  不 MarkResult、不 emitCodexOutcome、不故障转移**。HTTP 回 OpenAI 形态
  `{"error":{"message":"invalid request body","type":"invalid_request_error"}}`（转换车道同样）；
  WS 回 `{"type":"error","status":400,"error":{…}}`，拨号/首帧落点随后关闭，后续帧落点丢弃
  该帧、会话继续、不写错误槽。识别只在错误分支做，热路径零开销；`sdkbridge` 必须保留
  错误链。
- `ConversationWorker`：`worker.Loop` 驱动节拍（默认 50ms，可注入）调 `book.Flush`，Flush
  串行（`flushMu`）；`Close` 在停机预算内最后 Flush 一次；`FlushNow` 是测试屏障。`Stats()`
  扁平键：`conv_*` = `ConversationStats` 全部字段 + `conv_local_rejects`，`shape_*` =
  `ShapeStatsSnapshot()` 全部字段；改字段须同步 `openapi.yaml` `WorkerStatus.stats` 注释与
  `web/src/locales/{en,zh}.ts` 的 `ops.stats.*`（`TestConversationWorkerStatsKeys` 对齐 SDK 字段）。
- 客户端断连记 499，不进 failover 分类；上游错误原文不透给客户端；Codex WS 面不喂任何
  客户端头（握手头只来自 SDK）；客户端会话类头与体中的对话标识、`client_metadata` 不出网
  （`TestCodexClientIdentityNeverEgress`）。
- 测试：只用 require；禁 `t.Parallel`、禁 `time.Sleep`（用屏障或可确定观测，例如未 Start
  的 `ContBindWorker` 的 `Queued()`、会话簿存储替身的 SaveTurns channel）。

## 边界

- 选号、槽位身份、`Selection` 字段在 `internal/scheduler`；本包只读 `Selection`
  （`Conversation`/`CacheDomain`/`InstallationID`），不读未导出的槽位会话。
- 续接存储实现在 `internal/continuation`（不在本包改）；SDK 请求整形与会话簿实现在 codex-sdk。
- 计费/用量记录单写点在 usage；本包只经 `routeLog`/`recordRejected`/`finish` 出生定态。

## 验证

```bash
go test ./internal/proxy/ -count=1
go test ./internal/proxy/ -run 'TestCodexSkipsContinuation|TestCodexConvertedContinuationNotEnqueued|TestContinuation' -count=1
go test ./internal/proxy/ -run 'TestCodexInvalidBodyIsClientError|TestConversationWorker|TestCodexConversation|TestCodexClientIdentityNeverEgress|TestConversationIdentifierPrecedence|TestAffinityPrefersSessionHeaders|TestFormatEntryConversationNoNewAllocs|TestCodexWSPrewarmFailureIsPreCommit' -count=1
```
