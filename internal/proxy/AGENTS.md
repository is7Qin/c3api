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
- 续接：`continuation.go`（`contResolve` / `contPin` / `contEnqueue` / `contBind`）、
  `continuation_bind.go`（`ContBindWorker` 异步批量绑定）、`ws_relay.go`（WS 同步 ACK 闸门）。

## 关键不变量

- 热路径零 DB、零每请求锁、**零新增分配**（`pipeline.go` `guardPipeline` 注释；不用
  release 闭包，两条 defer 展开）。
- 续接 Redis 只在 Responses 带 `previous_response_id` 时发生；普通请求续接零 Redis
  （`TestContinuationOrdinaryRequestsZeroRedis`）。Codex 会话路径的 Redis 访问由 SDK
  会话簿按设计 §3 发生（本副本命中零 Redis），不在续接代码里。
- 硬续接（API key 账号）：missing/expired/Redis 不可用一律 fail-closed（410/503/409），
  零上游拨号；钉选后 `hardContinuation=true`，绝不迁移到其他账号。
- **Codex 不做续接**：没有任何 Codex 传输写续接绑定（直连流式 W3 已删除；W1/W2/W4/W5
  在 Codex 分流之后、W6/W7 的 `contTag` 对 Codex 为空，均不可达，代码处有注释）。首个预留
  的 `sel` 是 Codex 时，HTTP 与 WS 都跳过 `contResolve`/`contPin`（`hardContinuation=false`），
  `previous_response_id` 交给 SDK 白名单；首个预留为 API key 时行为不变（混合组可能经旧
  绑定钉选到其他账号）。
- 客户端断连记 499，不进 failover 分类；上游错误原文不透给客户端；Codex WS 面不喂任何
  客户端头（握手头只来自 SDK）。
- 测试：只用 require；禁 `t.Parallel`、禁 `time.Sleep`（用屏障或可确定观测，例如未 Start
  的 `ContBindWorker` 的 `Queued()`）。

## 边界

- 选号、槽位身份、`Selection` 字段在 `internal/scheduler`；本包只读 `Selection`。
- 续接存储实现在 `internal/continuation`（不在本包改）；SDK 请求整形在 codex-sdk。
- 计费/用量记录单写点在 usage；本包只经 `routeLog`/`recordRejected` 出生定态。

## 验证

```bash
go test ./internal/proxy/ -count=1
go test ./internal/proxy/ -run 'TestCodexSkipsContinuation|TestCodexConvertedContinuationNotEnqueued|TestContinuation' -count=1
```

## 最近变更

- 2026-11-13（codex-conversation-integration §5.7，C3-10）：删除 Codex 直连流式续接入队与
  `codexStreamPlan.contEligible`；首个预留为 Codex 时跳过续接解析；更新“零 Redis”注释。
  接入第二轮还会改：入口置位 `AttemptPlanIdentity.Conversation`、`codexRequestPrep`
  交给 SDK 会话簿、删除 `codexTurnStateReplay`、SDK 本地拒绝不伤账号。
