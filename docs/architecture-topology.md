# github.com/is7qin/c3api 架构拓扑（标准文档）

> 受众：vibecoding agent（改热路径/加表/加 worker/接线前必读）+ 新贡献者 onboarding。
> 基准：main HEAD `9391c66`（2026-08-15，数据库残留清理后）。所有代码锚点均基于该 commit 的 worktree 核实。
> 约定：本文档只陈述 main 现状；快照注册表（#13）已合并，§7/§9 按合并后现状陈述。
> 详细 API 契约见 `docs/admin-api.md` 与 `openapi/openapi.yaml`，本文不重复。

## 1. 系统概览

```mermaid
flowchart LR
    subgraph Client["客户端"]
        C1["OpenAI SDK / curl"]
        C2["Anthropic SDK / Claude Code"]
        C3["Codex CLI（resp-ws）"]
        W1["浏览器（SPA）"]
    end

    subgraph G1["网关实例 1（单二进制 server）"]
        direction TB
        S["chi 路由<br/>/healthz /api/admin /api/user /v1/* /assets + SPA / /user /app"]
        PX["proxy 热路径<br/>鉴权→门禁→选号→转发"]
        WH["常驻 worker 群<br/>billing/usage/errlog/retention/stats-agg/<br/>scheduler/notify/invalidate/<br/>pricing-sync/auth-sync/rule-engine"]
    end

    subgraph G2["网关实例 N（多实例，可选）"]
        direction TB
        S2["chi 路由"]
        PX2["proxy 热路径"]
        WH2["worker 群"]
    end

    PG[("PostgreSQL 18<br/>19 ent 表（含 3 分区表）+ 1 辅助表<br/>c3api_invalidate NOTIFY")]
    UP1["上游 openai（REST + SSE）"]
    UP2["上游 anthropic（REST + SSE）"]
    UP3["上游 responses / resp-ws（WS）"]
    LLM["litellm 官方价格表<br/>（GitHub raw JSON）"]

    C1 -->|"POST /v1/chat/completions"| S
    C2 -->|"POST /v1/messages"| S
    C3 -->|"WS /v1/responses"| S
    W1 -->|"/ (SPA: / /user /app) + /api/* + /assets"| S
    C1 --> S2
    C2 --> S2
    C3 --> S2
    S --> PX
    S2 --> PX2
    PX --> UP1 & UP2 & UP3
    PX --> WH
    PX2 --> WH2
    WH --> PG
    WH2 --> PG
    G1 -.NOTIFY 广播.-> PG
    G2 -.NOTIFY 广播.-> PG
    WH -->|"price_sync_cron 拉取"| LLM
```

- 单二进制部署：前端 `web/` 构建产物经 `cmd/server/embed.go:14` 的 `go:embed all:dist` 内嵌进 Go 二进制；运行时 = `server` 进程 + 挂载 config（Dockerfile 三阶段：node → go → alpine 非 root）。
- 部署面（一句话带过）：
  - `compose.yml`：`db`（postgres:18-alpine，数据挂载 ./deploy/data/pg）+ `app`（单容器，`C3API_ADMIN_TOKEN`/`C3API_DB_DSN` 环境变量注入，config 只读挂载自 ./deploy/config.toml）。
  - `Dockerfile`：多阶段构建，`CGO_ENABLED=0` 静态单二进制。
  - `scripts/build.sh`：pnpm 构建 web → 拷入 `cmd/server/dist` → `go build -o bin/server`。
  - `tools/`：`tools/loadtest`（打压测，-mode stream/fill，交错跑 + 每请求 CPU）、`tools/fakeupstream`（假上游，chunks/latency 可配）、`tools/e2e`（端到端计费测试）。
- 运维观测面：/api/admin/ops/workers、/api/admin/overview、/api/admin/users-top 收敛进 handler 包（`internal/handler/ops.go:62`、`overview.go:24`、`users_top.go:18`），worker 观测聚合在 `cmd/server/main.go:384-398`；`internal/server` 已纯通用装配（Options 注入 AdminHandler/UserHandler/AIHandler/WebFS，`internal/server/server.go:22-34`）。

## 2. 模块地图

`internal/` 每包一行职责 + 依赖方向。**禁止反向**的约束单独标注。

| 包 | 职责 | 依赖（被谁依赖） |
|---|---|---|
| `internal/config` | TOML 加载 + env 覆盖（`C3API_*`） | main 入口 |
| `internal/server` | 纯通用装配：/healthz、/api/admin 鉴权中间件、/api/user 分流、AI Mount、SPA fallback（业务 handler 由 Options 注入） | main |
| `internal/handler`（+`/user`） | 管理面 API（openapi 生成，含 ops/overview/users-top）+ 用户面 API | 依赖 service/repository |
| `internal/service` | 业务层：settings/pricing 快照、变更发布 `publish`、ClusterInstances | 依赖 repository/notify/invalidate/rule |
| `internal/repository` | ent 持久化门面，只暴露 domain 类型（`internal/repository/repository.go:13`）；分区表 DDL 独占管理 | 被所有上层依赖 |
| `internal/domain` | 域类型 + 格式/错误分类常量（六格式 `types.go:17-34`） | 各处 |
| `internal/ent` | ent ORM 生成代码 + schema（19 文件） | repository |
| `internal/proxy` | **请求热路径**：鉴权/门禁/限流/选号/转发/用量路由 | 依赖 scheduler/rule/auth/credential/billing/usage/sdkbridge/protoconv |
| `internal/scheduler` | 账号调度：快照/预生成路由/并发槽/异步状态回写（含组级 GroupPub 发布，`internal/scheduler/scheduler.go:34-46`） | 依赖 rule/credential；**不 import notify**（发布面接口化，`cmd/server/dispatcher.go:17-25` 装配侧粘合） |
| `internal/rule` | 规则引擎：事件队列 worker（Name="rule-engine"）+ 状态动作 apply | scheduler 注入 apply 回调 |
| `internal/credential` | 凭据类型注册表 + Provider 分发（api_key/responses-special/codex-oauth/codex-pat，`internal/credential/credential.go:29-46`） | proxy/scheduler 消费 |
| `internal/auth` | JWT issuer/verify + RequireJWT 中间件 | server/user handler |
| `internal/billing` | 计费：价格矩阵纯函数、余额快照、批量扣费 Flusher | proxy/repository |
| `internal/usage` | 明细 Recorder、err_logs 落盘 worker、retention worker、**stats-agg 离线聚合 worker** | proxy/billing/repository |
| `internal/notify` | 多实例 NOTIFY 发布/监听（`Change` 载荷 + Dispatcher 接口） | main 装配（service/scheduler 发布） |
| `internal/invalidate` | 管理面变更去抖定向失效（Debouncer） | main 装配；service 作 Invalidator |
| `internal/pricing` | litellm 价格表拉取 + cron 同步 worker（三线：文本/image/function 价） | service/repository |
| `internal/protoconv` | 协议转换（四方向，纯标准库） | proxy（convertedCaller） |
| `internal/worker` | worker.Worker 契约 + Manager（顺序启动/反向排空/panic 栈日志/Go 托管） | cmd/server 装配 |
| `internal/snapshot` | 快照注册表（#13 已合并）：Registry/ReloadAll/Status + scope 精确分发 | cmd/server 装配（dispatcher 消费） |
| `internal/sdkbridge` | codex-sdk 适配层（T2-T6：生图/Responses/Stream/WS/Search/Dial + 凭据派生 clientFor + 失效回调 + OAuth 轮转回写） | proxy |
| `pkg/aiclient` | openai/anthropic SDK 客户端懒构建工厂（**非唯一引用点**——proxy 各 caller 直接 import SDK 类型，第三 SDK codex-sdk 经 sdkbridge） | proxy |
| `pkg/httpx` | 共享上游 http.Transport（连接池参数；Proxy 默认 nil 直连） | main → aiclient/sdkbridge/pricing |
| `pkg/sserelay` | 字节级 SSE relay（帧原样透传 + Observer 旁路 + Mapper 挂载） | proxy 流式路径 |
| `pkg/cryptox` / `pkg/logx` | 客户端 key 生成（ck- 前缀随机 hex，明文落库，见 key_raw 设计） / 结构化日志 | 各处 |

装配链（`cmd/server/main.go:42-556`）：config → logx → PG 池（`OpenPG` + `stdlib.OpenDBFromPool` 桥接，`main.go:83,89`）→ 三分区 bootstrap（`main.go:102-108`）→ function_price 种子（`main.go:110-115`）→ notify Publisher（`main.go:131`）→ ruleEngine/sched/rec/statsAgg/errlogW/retention（`main.go:134-167`）→ proxy.Auth（`main.go:173`）→ invalidate Debouncer（`main.go:230`）→ service（`main.go:241`）→ **snapReg 五路注册**（`main.go:248-263`）→ dispatcher（`main.go:266-271`）→ notify listener（`main.go:282`）→ authSync（`main.go:291`）→ billing Flusher + BillingHooks（`main.go:293`）→ proxy.New（`main.go:309`）→ **codexAdapter**（`main.go:322-351`：独立 transport + SetCodex）→ `SetInstancesProvider(svc)`（`main.go:355`）→ pricing SyncWorker（`main.go:363`）→ opsWorkers 观测聚合（`main.go:384-398`）→ handler/server.New（`main.go:420`）→ **snapReg.ReloadAll 统一首刷**（`main.go:443-456`，全成功置位 bootLoaded）→ worker.Manager 注册 + StartAll（`main.go:460-471`）→ HTTP 监听（`main.go:484`）→ 优雅停机链（`main.go:500-521`）。`-pprof` flag 监听失败 Warn 不 fatal（`main.go:51,69-75`）。

依赖约束（改代码前先看）：
- **notify 不 import invalidate/service**（`internal/notify/listener.go:15-16`）；**scheduler 不 import notify**（`cmd/server/dispatcher.go:17-25`）——跨层发布全部接口化，由 cmd/server 装配侧粘合。
- **proxy 不 import server/handler**；handler 不 import proxy（AI 端点经 `proxy.AIRouter` 在 main 装配）。
- **repository 不依赖任何业务包**（domain 类型下沉）。
- ent schema 文件名 snake_case 对照（勿混淆）：`internal/ent/schema/account_ext.go`↔包 `accountext`、`group_assignment.go`↔`groupassignment`、`template_ext.go`↔`templateext`、`redemption_code.go`↔`redemptioncode`、`redemption_use.go`↔`redemptionuse`、`temp_balance.go`↔`tempbalance`、`usagelog.go`↔`usagelog`、`usagestat.go`↔`usagestat`、`errlog.go`↔`errlog`、`function_price.go`↔`functionprice`、`image_price.go`↔`imageprice`（`internal/ent/schema/` 共 19 个文件）。

## 3. 请求热路径

**入口**：`internal/server/server.go:86-91` AI 组 `Mount("/")`（inflightLimiter :87）→ `internal/proxy/router.go:19-58` 路径决定格式（**8 端点**：chat/completions、responses×2（HTTP+WS）、messages、images generations/edits、alpha/search、models；responses 带 upgrade 头 → WS 编排 `internal/proxy/caller_responses_ws.go:64`）。

**骨架**（`internal/proxy/caller.go:105-430` `handleFormat`，顺序即纪律）：

```mermaid
flowchart LR
    A["Authenticate<br/>internal/proxy/auth.go:127"] --> B["QuotaExhausted<br/>本地预算快读 internal/proxy/gate.go:267"]
    B --> C["余额预检<br/>BalanceOf 快照 internal/proxy/caller.go:140"]
    C --> D["Acquire 两级并发门禁<br/>internal/proxy/gate.go:219"]
    D --> E["固定窗口限流 Allow<br/>internal/proxy/limit.go:39"]
    E --> F["读体 + json.Valid +<br/>gjson 提 stream/model/tier"]
    F --> G["sched.Select 选号<br/>（含协议转换补差）"]
    G --> H["failover 循环<br/>每轮缺价预检 + codex 分流<br/>credentialFor + caller.Call"]
    H --> I["finish/record：<br/>Release + DeductQuota +<br/>计费 + routeLog 路由"]
```

每个决策点：
- **鉴权**（`internal/proxy/auth.go:127-151`）：`Authorization: Bearer` 或 `x-api-key`（Anthropic 口径）→ 快照按明文等值查（零 DB）；key 或归属用户禁用 → 401 即时失效。
- **认证双路径**（`internal/server/middleware.go:38-67` adminAuth + `internal/server/server.go:68-79` /api/admin 组 Handle）：/api/admin 组 = 静态 admin token `Bearer <AdminToken>` **OR** platform_admin JWT（`JWTIssuer.Verify` + `claims.Role==platform_admin` + 快照用户状态 active 校验；JWT 路径注入 `adminUserIDKey`）；/api/user 组 = 内部公开分流（`internal/handler/user/router.go:22-40`：register/login 公开，其余 RequireJWT）；AI 组 = key 鉴权（无 JWT）。
- **配额**：`quotaExhausted`（`internal/proxy/gate.go:267-319`）本地预算两原子读；耗尽才触发 DB 复核认领（`internal/proxy/gate.go:320-374`，慢路径单飞 + 10s 失败退避）；复核公式 `budget = consumed + ceil(remaining_eff/N)`（#37 P1 收敛修正，防复核无限续额）。
- **余额预检**（`internal/proxy/caller.go:140-147`）：BillingCapture 门控快照读零 DB（滞后 ≤ balance_refresh_interval）；快照缺失/<0 且非免费组 → 402（余额 0 放行——临时额度由 FEFO 扣费消化）；免费组（EffectiveMultiplier==0）放行。
- **并发门禁**：user → key 两级 CAS（`internal/proxy/gate.go:219-244`），key 失败回滚 user 计数；跨 reload 在途值继承。
- **限流**：`internal/proxy/limit.go:39-56` 固定窗口 `ceil(group_key_rpm/N)`；`cooldown_429/backoff_*` 已移除（2026-08-13 用户裁决：配置含这些键将启动失败）——429 冷却与错误退避由规则引擎（种子 + `/api/admin/rules` 自定义）接管。
- **选号**：`internal/scheduler/selection.go:17` tier1（模型硬白名单 Serves）→ tier2（仅全模型账号）→ 默认桶（仅全模型账号）；预生成加权轮询序列（零热路径计算）；协议转换只补差（`internal/proxy/caller.go:41-61` convertedRoute，off 零开销）。
- **缺价预检**：failover 循环内每轮 `caller.go:288-293` + `precheckPrice :438-451`——images 查 image_price、其余查 pricings，缺价 402 释放槽。
- **codex 分流**（`caller.go:303-309`）：按 `sel.CredentialType` 换 codexImagesCaller（:320-321 codex 类型跳单字符串凭据走 sel.Ext → AccountCredential 直供适配层）。
- **流式透传**：aiclient 流式入口经 `pkg/sserelay` 字节级 relay + Observer 旁路提取 usage；C1 批次能力：EOF 末帧 flush（`relay.go:217-230`，无末尾空行上游丢 completed 帧 → cost=0 修复）、deadline watcher（`relay.go:353-376` + `middleware.go:154-156`）、normalize 错误分类（`relay.go:269-277` 三态可分）、relayBufio 池（`relay.go:86-98`）、按需武装 flush timer（`relay.go:307-323`）；WS 1:1 透传（`internal/proxy/caller_responses_ws.go:272`，心跳 :352）。
- **usage 计费**：`finish`（`internal/proxy/forward.go:136-149`）→ `routeLog`（`forward.go:391-405`）分表路由——放行行（error_type ∈ {none, abort}）billed → Flusher / 非 billed → rec.Record；拒绝路径走 `recordRejected`（`forward.go:347`——401/429/402/限流 → err_logs 不进 usage_logs）；images/search 按次计费 `applyImageBilling`/`applyFunctionBilling`（`forward.go:252,283`，call_count）。

**热路径纪律**（改这里先读）：
1. 热路径**零 DB、零 per-request 锁**（`internal/proxy/forward.go:1-3` 包注释）；所有快照读 = 内存原子（atomic.Pointer / RWMutex 读锁 / CAS）。
2. **开关关闭 = 快照读 + 分支**：`protocol_convert=off` 不触达 protoconv（`internal/proxy/caller.go:244-249`）、`strip_image_tools` 关 = 布尔读 + 分支（`internal/scheduler/selection.go:93`）、billing off = `shouldBill` 恒 false（`internal/proxy/forward.go:150-157`）。
3. **开关开启 = bytes.Contains 预筛零解析**：W4 图像 tool 剥离 `internal/proxy/strip_image.go` 预筛（无 "image" 子串零解析，压测 ~1.5% QPS 差异）；W5 转换走 gjson 预筛 + Raw 零拷贝（`internal/protoconv/jsraw.go:1-9`）。
4. **永不阻塞投递**：usage.Record（`internal/usage/usage.go:154`）、billing Flusher.Record（`internal/billing/flusher.go:198`）均为无 channel 的锁内归并；errlog 投递 select-default 非阻塞丢弃（`internal/usage/errlog.go:147,161`）。

## 4. 协议适配面

六格式（`internal/domain/types.go:17-34`）：`openai-chat` / `openai-responses` / `openai-responses-ws` / `anthropic` / `openai-images`（generations/edits 双端点）/ `openai-search`（按次计费）。

| 格式 | 入口 | caller | 流式 |
|---|---|---|---|
| chat | `POST /v1/chat/completions` | `internal/proxy/caller_chat.go` | SSE relay |
| responses | `POST /v1/responses` | `internal/proxy/caller_responses.go` | SSE relay |
| responses-ws | `WS /v1/responses`（upgrade 头判定，`internal/proxy/router.go:19-58`） | `internal/proxy/caller_responses_ws.go:64` | 原生 WS 1:1 |
| anthropic | `POST /v1/messages` | `internal/proxy/caller_anthropic.go` | SSE relay |
| images | `POST /v1/images/generations` / `/edits` | `internal/proxy/caller_images.go`（双调用器，`forward.go:101-103`）+ codex 变体 `caller_images_codex.go`/`caller_images_stream.go` | SSE relay |
| search | `POST /v1/alpha/search` | `internal/proxy/forward_search.go:59`（独立编排：独立选号 + 四类型分派 + 按次计费） | unary |

pkg 职责边界：
- `pkg/aiclient`（`pkg/aiclient/aiclient.go:5-22`）：**openai/anthropic 官方 SDK 客户端懒构建工厂**——鉴权头注入（格式决定头名：openai → `Authorization: Bearer`、anthropic → `x-api-key`）+ typed 面非流式超时（UpstreamTimeout，:28-30）；**非唯一引用点**——proxy 各 caller 直接 import openai-go/anthropic-sdk 类型（`caller_chat.go:14`、`caller_anthropic.go:14`、`caller_responses.go:15`、`caller_converted.go:16-17`），第三 SDK codex-sdk 经 sdkbridge 适配。
- `internal/sdkbridge`（`internal/sdkbridge/codex.go:16`）：**codex-sdk 唯一适配层**——GenerateImage/GenerateImageStream/Responses/StreamResponses/Search/Dial（T2-T6）+ 凭据派生 clientFor + 失效回调 + T5 轮转回写 WriteOAuthRotation；codex 非流式超时各自包 ctx WithTimeout（`codex_responses_http.go:92`、`caller_images_codex.go:111`——HTTPClient.Timeout 不可用，流式/非流式四方法共享，`forward.go:36` 注释）。
- `pkg/httpx`：共享 `http.Transport`（连接池参数：max_idle_conns 8192、per_host 2048、force_http2、idle_conn_timeout 90s、dial_timeout 10s，`config.example.toml:20-26`）；openai-go/anthropic-sdk 共用同一 `*http.Client`（`cmd/server/main.go:175-189`）；**codex SDK 另有独立 transport**（`main.go:332-342`：httpx 网关同形态 + MaxConnsPerHost 显式上界，补压测修复 MaxIdleConnsPerHost=2 连接风暴）；**httpx.Proxy 默认 nil 直连**（`pkg/httpx/httpx.go:24-28,33`——不再隐式 ProxyFromEnvironment 防 HTTP_PROXY 静默改道）。
- `pkg/sserelay`：字节级 SSE relay——增量读帧原样转发 + 自适应批量 Flush + Observer 旁路（仅 usage 提取，不参与转发决策；`pkg/sserelay/relay.go:5-8`）+ EOF 末帧 flush + deadline watcher + normalize 错误分类 + relayBufio 池 + **Mapper 挂载**（W5 转换，`relay.go:57-62`，Observer 仍见原始帧）。
- 凭据抽象 `internal/credential`：Provider 只返回凭据值，不感知请求格式（`internal/credential/credential.go:7-11` 正交原则）；未知类型显式报错不静默 fallback（`internal/proxy/forward.go:681-687` credentialFor）；**codex 复合凭据不进注册表**——走 `sel.Ext` → AccountCredential 直供适配层（`caller.go:310-321` 注释，单字符串表达不了）。

加新格式 = 1 个 caller 文件 + `internal/proxy/forward.go:96-113` 注册表一行（callers + convCallers 四方向 + images/codexImages 双调用器）+ router.go 一个端点（现 8 端点，`router.go:19-58`）；images 同格式双端点 + codex 双调用器为既有格局之外的特例。

## 5. 协议转换层（internal/protoconv）

- 边界（`internal/protoconv/protoconv.go:5-13` 包注释）：**纯标准库**（encoding/json），与 OpenAI/Anthropic SDK 零耦合；按 `groups.protocol_convert` 快照值分派（off 不经过本包——热路径分支在 proxy 判定）；WS 帧流转换不做（resp-ws 1:1 透传）。
- 四方向（`internal/protoconv/protoconv.go:27-54`）：`ConvertRequest`（chat→resp / mess→resp / resp→mess / chat→mess）+ `ConvertResponse`（非流式）；`NewStreamMapper`（流式 SSE 事件映射，`internal/protoconv/protoconv.go:60-62`）；四方向常量在 `domain/types.go:310-323`。
- **字节级纪律**（`internal/protoconv/jsraw.go:20-25`）：gjson 预筛（`gjsonKeyEq` 长度校验 + 逐字节比较零分配）→ `gjson.Result.Raw` 零拷贝切片直接拼入输出 → 单缓冲复用（`StreamMapper` 的 buf/dbuf，`internal/protoconv/protoconv.go:94-107`；帧返回后下一帧覆盖，调用方不得跨帧保留）；chat→resp 方向字节级组装，其余方向 map 组装（`EncodeFrame`）。
- 缺名帧处理（P3 教训）：无 `event:` 名帧从 data 的 `type` 字段推断（`internal/protoconv/protoconv.go:167-175` `inferEventName`，Map 内分派 :116-125），无法推断原样透传。
- 转换 on/off 开销实证（`docs/superpowers/plans/2026-08-11-w3-loadtest.md` §二 与 `docs/superpowers/plans/2026-08-11-protoconv-opt-loadtest.md` §一，均压测机（内部环境，IP 存部署清单） 实证）：
  - w3 历史：on 相对 off = QPS **-16.4%**、每请求 CPU **+32%**（+71.5µs）、首字节 **+34ms**。
  - 字节级优化后（请求 274→6 allocs、流式逐帧 59→1.1 的 benchmark 声明）：QPS 差距收敛至 **-10.9%**（52.3k→46.6k）、CPU **+17.8%**（+41.4µs）、首字节 **+21ms**；pprof 转换路径占比 22.6% → 8.5%。
  - off 基线无回退（52.3k / 232.8µs，与 w3 off 同量级）——"off 热路径零开销"保持成立。

## 6. 数据模型

**19 张 ent 表**（`internal/ent/schema/` 19 个文件；ent migrate 清单 20 张 = 17 普通 + 3 分区，`internal/ent/migrate/schema.go:625-646`；另非 ent 辅助表 `stats_agg_watermark`，**总账 21 张**）：

| 表 | schema 文件 | 说明 |
|---|---|---|
| accounts | account.go | 上游账号（status/cooldown/max_concurrency/weight + template_id） |
| account_exts | account_ext.go | 账号类型化扩展（codex oauth/pat 凭据） |
| groups | group.go | 组（倍率、protocol_convert、key 限制） |
| group_assignments | group_assignment.go | 用户-组关联（专属倍率） |
| keys | key.go | 网关 key（hash/quota/quota_used/concurrency） |
| pricings | pricing.go | 模型价格（source 行级互斥 manual > litellm） |
| **function_prices** | function_price.go | 按次价表（search 等 per-unit 端点；model 唯一 + price_per_call + 同款 source 机制） |
| **image_prices** | image_price.go | 图片生成价三列 + 同款 source 机制——价格表**三件套**（pricings + image_prices + function_prices） |
| redemption_codes / redemption_uses | redemption_code.go / redemption_use.go | 兑换码 + 使用记录（`(code_id,user_id)` 唯一 + user_id 前缀索引，`redemption_use.go:37-44`；90 天 TTL 有界批删 ≤5000 行/轮，`retention.go:35-38` + `partition.go:535-558`） |
| rules | rule.go | 规则引擎表（种子 + 自定义） |
| settings | setting.go | 内置设置注册表（`internal/domain/settings.go:10-38` 类型化 key） |
| temp_balances | temp_balance.go | 临时额度（FEFO 扣费） |
| templates / template_exts | template.go / template_ext.go | 模板 + 类型化扩展（strip_image_tools 等开关） |
| users | user.go | 用户（balance/status/concurrency） |
| **usage_logs** | usagelog.go | 计费明细（**分区表**；瘦身——无 status_code/error_message，`usagelog.go:33-39`） |
| **err_logs** | errlog.go | 错误审计明细（**分区表**；status_code integer + error_message，`errlog.go:12,35`） |
| **usage_stats** | usagestat.go | 聚合统计（**分区表**） |

另：`accounts`↔`groups` 多对多隐式 join 表 `account_groups`（`internal/ent/account/account.go:58`）。

**三表分区**（`internal/repository/partition.go`，单一实现三表共用）：
- 分区键：usage_logs/err_logs = `created_at`；usage_stats = `bucket_time`（小时桶 24 桶/日分区）。主键 `(id, 分区键)`（分区表硬约束），id 走专用序列 `{table}_id_seq`，DROP TABLE 级联回收（`internal/repository/partition.go:38-41`）。
- 保留期（`config.example.toml:40,55-56` + `cmd/server/main.go:162-171`）：usage_logs **30 天**、err_logs **7 天**（错误审计短保留）、usage_stats **180 天**（聚合长保留）——**全部 DROP 分区 O(1)**（PG DELETE 不释放空间，用户裁决），retention worker 每小时巡检按名 DROP + 预建当日/明日分区。
- ent migrate 跳过分区表（`internal/repository/partition.go:570-582` `migrateHookExcludesPartitioned`——atlas 对分区表 diff 规划期必失败，真实 PG 实测结论）；表 DDL/索引由 bootstrap 独占管理（`ensureTablePartitioned` `internal/repository/partition.go:358-389`：未分区 → DROP 重建表/序列/OWNED BY/索引，已分区 → 仅补当日/明日分区；幂等 + 42P07/42710/23505/42P01 容忍多实例并发）；**align 补列机制已删（2026-08-15）**——无补列路径，存量库兼容面整体消灭（全表从零创建，多实例并发 bootstrap 竞态由 `isBootstrapRaceError` `partition.go:329-331` 容忍）。
- 关键索引（`internal/repository/partition.go:141-148,178-182,233-236`）：usage_logs `(created_at)` + `(group_id/account_id/user_id/key_id, created_at)` + **`usagelog_request_id_created_at` 唯一索引**（`partition.go:147`，幂等键 A-P2-3）；err_logs `(created_at)` + `(group_id/user_id, created_at)`；usage_stats 唯一索引 `(bucket_time, group_id, account_id, template_id, user_id, model, is_error)`（即 Upsert 冲突目标）+ **`usagestat_user_id_bucket_time` 复合索引**（`partition.go:235`——bucket_time 冗余独立索引已删（F3）后的覆盖索引）。
- **stats_agg_watermark 辅助表**（`partition.go:409-414` DDL + :421-426 调用）：`EnsureUsageStatsPartitioned` 同步骤建单行 watermark——stats-agg worker 每周期读聚合位置、推进与 DELETE+INSERT 同事务。

## 7. 快照与注册表层级

**main 现状（#13 已合并）**：快照注册表 `snapshot.Registry`（`internal/snapshot/snapshot.go`：ReloadAll 启动就绪 :126-133、Reload 空 scopes 前置 return :139-142、scope 精确分发 :143-159）只持有 Name/Scope/LastReload 元数据，**不接管模块 ticker、不缓存快照数据、不进请求热路径**（用户拍板 2026-08-11，`snapshot.go:9-19` 包注释）。快照数据形态与周期刷新仍各模块自管（atomic.Pointer 整表换入或 RWMutex + 锁内换 map）：

| 快照 | 实现 | 刷新源 |
|---|---|---|
| auth（key/user 元数据 + gate 计数） | `internal/proxy/auth.go:68` Reload，锁内整体换 | invalidate Users/Keys + authSync 60s 周期兜底 + 启动 ReloadAll |
| scheduler（组/账号/路由 + 并发槽） | `internal/scheduler/scheduler.go:102` snapshotStore | invalidate 全量/组级 + 30s syncLoop ticker |
| rules（规则表） | `internal/rule/engine.go:148` Reload | invalidate Rules + 启动 ReloadAll |
| pricing（模型价格**三线**：pricing + image_price + function_price） | `internal/service/pricing.go:35-71` + `image_pricing.go:23` + `function_pricing.go:23` | ReloadPricing 三线（sync 成功/管理端改价后，`main.go:369-373`），**不进 invalidate**（`internal/invalidate/invalidate.go:26` 注释） |
| balances（余额 + 倍率） | `internal/billing/balances.go:43-51`（atomic.Pointer，Set 原地 Store） | invalidate Users/Multipliers + BalanceRefreshInterval ticker（`internal/billing/flusher.go:134-145`） |
| settings | `internal/service/setting.go:95` ReloadSettings | invalidate Settings + 启动（**自身不进注册表**，`main.go:242-248` 注释） |
| credential.Registry | `internal/credential/credential.go`（类型注册表 + Provider 分发，无快照语义） | 静态注册 |

**注册表装配**（`cmd/server/main.go:248-263` + `cmd/server/snapshots.go:26-62`）：五路注册 auth/scheduler/rules/pricing/balances（billing 关不注册 balances）；**启动首刷 = ReloadAll 单一入口**（`main.go:443-456`，取代旧分散 ruleEngine.Reload fatalf + sched.InvalidateAllSync fatalf + auth/balances 构造时加载）。

**E2 启动双刷**：ReloadAll 返回空 errs（全成功）置位 `bootLoaded`（`main.go:447-456`）；dispatcher 首连 `FullRefresh` CAS 消费后**跳过五路 ReloadAll 仅补 ReloadSettings**（`dispatcher.go:41-49,136-155`）；部分失败不置位 → 首连仍全量。

**notify.Dispatcher 接口**（`internal/notify/listener.go:25-39`）：`Apply(Change)` + `FullRefresh()`；实现放装配侧 `cmd/server/dispatcher.go:36-155`（notify 不 import invalidate 的依赖环约束；Apply :72-110、FullRefresh :136-155）。settings 变更 → 同步 ReloadSettings + 注册表 ScopeSettings 精确重载 auth（#36 预算即时重算，`dispatcher.go:93-105`；`snapshots.go:30` auth 声明 ScopeSettings）。

## 8. worker 拓扑

统一契约 `worker.Worker`（`internal/worker/worker.go:21-25`：Name/Start/Close 均幂等，Close 未 Start 也安全）；`worker.Manager` 顺序启动、**反向排空**、panic 捕获（`internal/worker/worker.go:30-138`；E1 批次：panic 栈入日志 :127-134、Shutdown 等 Go 托管 goroutine（WaitGroup）:88-116、StartAll 启动期无锁 :57-66、单次生命周期（started atomic.Bool，Register 在 StartAll 后报错）:43-51、Go 托管命名 goroutine :123-138）。

注册顺序与反向排空（`cmd/server/main.go:460-470`）：`inv → sched → ruleEngine → rec → errlogW → pricingSync → retention → statsAgg` →（billing 开后）`billFlusher` → `listener, authSync`。停机时反向：listener/authSync 先关（旁观者）→ billFlusher（最后一个产生计费流量的 worker，扣费全量落库）→ statsAgg → retention → pricingSync → errlogW → rec → ruleEngine → sched → inv。

| worker | Name | 类型 | 节奏/背压 | 排空/停机语义 |
|---|---|---|---|---|
| billing.Flusher | "billing"（`internal/billing/flusher.go:118`） | ticker 批量 | `flush_interval`（1s）+ `balance_refresh_interval`（10s）；无 channel，pending map 锁内归并，`Record` 永不阻塞（`internal/billing/flusher.go:198`）；水线 1M 行 Warn（:42） | Close：等在途批次（flushMu）→ 预算内排空循环，超时 Cancel baseCtx 截断 Warn（`internal/billing/flusher.go:228-283`）；O1 复测教训：Background ctx 会拖死停机 |
| usage.Recorder | "usage"（`internal/usage/usage.go:146`） | 双 loop（logWriterLoop + quotaFlushLoop，`usage.go:132-145`） | `flush_interval` 500ms / `quota_flush_interval` 10s（**统计桶机制整体删除**，2026-08-14 离线聚合化，`usage.go:152-153`）；swap 换批 + 按 userID 取模分片 N worker（`usage.go:244-250`）；毒丸行止损 **poisonBisect 二分隔离**（`usage.go:288,319-349`，隔离行回灌不丢）；flushLogs 与 flushQuota 共用 flushMu（`usage.go:216-218`） | 同 Flusher 模式：等在途 → 预算排空 → 截断 Warn（`internal/usage/usage.go:576-629`） |
| usage.StatsAggWorker | "stats-agg"（`internal/usage/stats_agg.go:123`） | ticker | 每周期两范围+三查询+单事务 DELETE+INSERT+watermark 重建 usage_stats（`stats_agg.go:69-80`）；advisory lock 防多实例并发（:30）；`stats_agg_interval` 默认 5m、0=禁用（:42）；追赶上限 1h（:48）、滞后 5s（:54） | 无资源需排空，Close nil（禁用/正常均安全） |
| usage.ErrLogWorker | "errlog"（`internal/usage/errlog.go:105`） | 双队列 + ticker | 有界队列（reject 4096 / exempt 1024）+ select-default 非阻塞投递（满→丢弃计数，`internal/usage/errlog.go:147,161`）；豁免队列恒落盘；单批 500 行 / 500ms，单批超时 5s 失败即丢弃（`errlog.go:47-49,58,184-190`） | Close：置位 closed（无尾窗口静默丢）→ 等 loop → 预算内排空，超时截断并入丢弃计数（`internal/usage/errlog.go:236-287`） |
| usage.RetentionWorker | "retention"（`internal/usage/retention.go:65`） | ticker 1h | 三表独立 cutoff 各自 DROP + 预建当日/明日；逐表错误隔离（`internal/usage/retention.go:97-148`）；**另含 redemption_uses 90 天 TTL 有界批删**（≤5000 行/轮，普通表无分区可 DROP，`retention.go:32,38,56-58,172` + `partition.go:547`）；启动即巡检 runOnce（:105）；观测面 lastPatrol/lastDrop*（:73-80） | 无排空需求（DROP/预建/批删均幂等），Close 直接 nil（`internal/usage/retention.go:199`） |
| scheduler | "scheduler"（`internal/scheduler/scheduler.go:117`） | syncLoop + writebackLoop 双 goroutine（`scheduler.go:132-133`） | sync `sync_interval` 30s；writeCh 有界 4096 满则丢弃 DB 回写（`scheduler.go:117`，内存状态已生效）；组级 GroupPub 发布（:34-46,234-239） | Close 排空 writeCh 剩余回写，预算超时 Warn 丢（`scheduler.go:139-159`） |
| notify.Listener | "notify"（`internal/notify/listener.go:123`） | 事件驱动（阻塞 WaitForNotification） | 独立单连接 LISTEN c3api_invalidate；断线指数退避 1s→30s + 重连全量刷新 | Close 取消 + 等 goroutine（阻塞点均响应 ctx，`internal/notify/listener.go:161-175`） |
| invalidate.Debouncer | "invalidate"（`internal/invalidate/invalidate.go:230`） | 事件驱动单 goroutine | 200ms 去抖窗口 + 后沿语义（执行期新变更立即再执行，`invalidate.go:262-302`）；重载串行不重叠；**规则重载 Background ctx 不随请求取消**（:347-354，B4-4：规则快照无周期兜底，事件驱动全量重载必须无条件完成） | Close nil（停机不补最后 flush——DB 权威，`invalidate.go:249`） |
| pricing.SyncWorker | "pricing-sync"（`internal/pricing/worker.go:74`，构造 `worker.go:63`） | 启动异步一次 + cron | `price_sync_cron`（默认 `0 3 * * *`）gronx 调度；每轮现读 settings；**三线 sync**（文本+image+function 价，`worker.go:126-128`）；非法 cron 1h 重试；观测面 running/lastSync（:73-74,98,172） | 无资源需排空，Close nil（`internal/pricing/worker.go:90`） |
| cmd/server.authSync | "auth-sync"（`cmd/server/auth_sync.go:38`） | ticker 60s | 周期全量 Reload auth 快照（NOTIFY 丢失兜底，`auth_sync.go:13-16`）；**per-attempt 30s 超时**（:26,80-84，B4-2：DB 挂起不卡死循环）+ **托管 goroutine**（:57,70，B4-3：panic 不崩进程）+ 观测面 running/lastReload/failures/lastFailure（:44-48，失败不前移 lastReload :93） | Close nil（循环随 ctx 退出，`auth_sync.go:64`） |
| rule.RuleEngine | "rule-engine"（`internal/rule/worker.go:15`） | 事件队列 | 有界 channel 满则丢弃（dropped 计数 + **阈值告警边沿回落**——每风暴恰好一次，`worker.go:103-122`；resetDropWarnIfDrained :71-75） | Flush 同步排空（测试/优雅关闭用，`worker.go:52-64`） |

**flush_workers 分片语义**（`config.example.toml:36-39` + `cmd/server/main.go:113`）：`flush_workers=8`（默认 8——多角度压测 C 项实证 2026-08-12：多用户水线告警清零、pending 斜率降 25-40%、QPS/CPU 零回退；受 db.max_conns 余量约束）——批内按 userID 取模分片（明细/额度共用：`internal/billing/flusher.go:332-342` + `internal/usage/usage.go:244-250`，bucket 机制已删），**同 key 恒同桶**（分片确定性）；分片并行非常驻 goroutine（每批新建，wg.Wait 收尾），**不是**常驻 worker。

## 9. 事件流（main 现状链）

```mermaid
flowchart LR
    SVC["管理面落库成功<br/>service/scheduler 发布点"] -->|"SELECT pg_notify('c3api_invalidate',$1)"| PG[("PostgreSQL")]
    PG -->|"NOTIFY（紧凑 JSON Change）"| L["每实例 Listener<br/>internal/notify/listener.go:239-258"]
    L -->|"Src 自播跳过"| D["装配侧 Dispatcher<br/>cmd/server/dispatcher.go:72-110"]
    D -->|"Apply → Mark 合并脏状态 +<br/>scope 分发快照注册表"| INV["invalidate.Debouncer<br/>200ms 窗口 + 后沿"]
    INV -->|"reloadAll 合并重载"| R1["auth Reload"]
    INV --> R2["balances Reload"]
    INV --> R3["sched InvalidateAll / InvalidateGroup"]
    INV --> R4["clients InvalidateAll"]
    INV --> R5["settings ReloadSettings"]
    INV --> R6["rules ReloadRules"]
    R5 -->|"ScopeSettings 精确重载"| R1
```

- **发布**（`internal/notify/publisher.go:99-128`）：DB 写成功后 `Publish`；载荷守卫——marshal >6KB 丢 Groups 降级 Templates（full 重载，`publisher.go:36-38,74-82`）；**计费扣费路径绝不发布 NOTIFY**（每 flush 即风暴，`publisher.go:16`）；scheduler 状态回写成功后发组级 NOTIFY（`scheduler.go:226-232` + `cmd/server/dispatcher.go:17-25` adapter）。
- **监听**（`internal/notify/listener.go:85-91,239-258`）：独立单连接（非池连接——池连接会被 idle 回收导致订阅静默丢失，`listener.go:66-79`）；消费 → Unmarshal → Src 自播跳过（`listener.go:250-252`）→ `Dispatcher.Apply`；断线指数退避重连（1s→30s，`listener.go:278-287`）+ 连接成功立即 `FullRefresh`（覆盖断连期间丢失，`listener.go:218-223`，是否跳过五路由 dispatcher bootLoaded 裁决）。
- **分发映射**（`cmd/server/dispatcher.go:72-110`）：Users → auth+余额全量；Templates → sched 全量 + clients 失效；Groups(±Clients) → sched 组级定向（upstream_key 变更带 clients 失效）；Clients 独立 → 仅客户端工厂失效；Multipliers → 余额倍率定向；Keys → auth 全量；Settings → 同步 ReloadSettings + **注册表 ScopeSettings 精确重载**（#36 预算即时重算，`dispatcher.go:93-106,114-124`）；Rules → 规则表全量重载（重载清窗口计数，全实例同步语义）。
- **去抖**（`internal/invalidate/invalidate.go:129,262-302`）：Mark 路径零锁零 DB（atomic CAS 合并 + 非阻塞唤醒）；200ms 窗口自首次变更计时，到点 flush 一次合并重载；后沿语义（完成后再脏立即再执行）。读端永不阻塞：重载单 goroutine 串行。
- **多实例广播语义**：每实例一个 Listener，同一 NOTIFY 全实例接收 → 各自去抖窗口执行（含发布实例自身，自播仅跳过 Src==自己）；全实例最终收敛（幂等重载 + DB 权威）。
- **启动双刷（E2）**：main 侧 `snapReg.ReloadAll` 统一首刷（`main.go:443-456`，全成功置位 bootLoaded）→ dispatcher 首连 FullRefresh CAS 消费后跳过五路仅补 ReloadSettings（`dispatcher.go:41-49,136-155`）；部分失败不置位 → 首连仍全量。
- **停机链**：10s shutdown 预算（`main.go:500`）→ `Shutdown(2s)` 优雅窗口（:509-516）→ `Close` 强断（:517-519）→ waitForInflight 100ms 轮询（:520,541-550）→ `wm.Shutdown` 反向排空（:521）；httpSrv ReadTimeout=upstream_timeout / IdleTimeout=60s / 不设 WriteTimeout（`main.go:477-491`）。

## 10. 多实例一致性

- **并发扣费**（`internal/repository/billing_repo.go:92-139`）：`DeductAndLog` 单事务——① FEFO 临时额度按 expires_at 升序逐行条件更新（`amount >= take` 行级防并发透支，NULL 最后即 NULLS LAST）；② 余额条件更新（`balance >= remain`），0 行 → 无条件扣允许透支，再 0 行 = 用户不存在跳过扣减仍插日志；③ 事务内回读 balanceAfter。行锁仲裁跨实例天然串行（`docs/superpowers/plans/2026-08-10-multi-instance-design.md` §1 表）。
- **同 user 恒同桶串行分片**（`internal/billing/flusher.go:96,301-330`）：按 userID 取模分片 → 实例内同 user 串行；跨实例靠 DB 行锁。
- **NOTIFY 跨实例**（§9 全链）：实例 ID = **hostname-pid-nonce**（crypto/rand 6B 随机，`cmd/server/main.go:118-130` 装配 + `instanceSrc` :526-536）——B4-1/p2-05 修复：容器化多实例同 hostname、pid namespace 各自 pid 1 → 纯 hostname-pid 互相碰撞 → 互把对方 NOTIFY 当自播跳过 → 失效静默全灭；自播判等 = 全串相等 `ch.Src == l.cfg.Src`（`listener.go:250-251` 注释）。
- **额度预算分摊**（`internal/proxy/gate.go:51-87`）：`budget = consumed + ceil(remaining_eff/N)`，N 存 DB settings `cluster.instances`（`internal/domain/settings.go:36-39`，config 文件可漂移故 DB 是唯一共识源）；N 变更走 settings NOTIFY → 装配侧重调 `SetInstancesProvider` 即时重算（`cmd/server/main.go:355`）；组 RPM 同款 `ceil(rpm/N)`（`internal/proxy/limit.go:14-16`）。
- **分区 DROP 幂等**（`internal/repository/partition.go:301-307,358,421-426`）：IF NOT EXISTS / IF EXISTS + 撞名 42P07/42710/23505（`isDuplicateObject` :301-307）+ **42P01 stale-DROP 窗口**（`isMissingObject` :318-324、`isBootstrapRaceError` :329-331——并发实例基于过期"未分区"判定 DROP 误删对方刚建表，由最后执行 DROP 的实例补建收敛，评审 I-1 已接受）；retention DROP 需 ACCESS EXCLUSIVE 锁与在途插入串行（`internal/usage/retention.go:60-64` 评审 I-3 注记）。
- **失效分发 B4 兜底面**：auth-sync 60s 兜底新增 per-attempt 30s 超时（`auth_sync.go:21-26,80-94`，B4-2）+ 托管 goroutine（:37-40,57,70，B4-3）；规则重载 Background ctx 不随请求取消（`invalidate.go:347-354`，B4-4）。
- **已知接受的竞态**（`docs/superpowers/plans/2026-08-10-multi-instance-design.md` §R2）：NOTIFY 重复投递 → mark 幂等合并；`UpdateAccountStatus` 并发 → last-writer-wins；stats Upsert 同桶累加精确；规则种子双写 → 唯一约束幂等；pricing sync 每实例独立 cron 重复 fetch（v1 接受）。

## 11. API 面

路由清单（详细契约见 `docs/admin-api.md` + `openapi/openapi.yaml`，不重复）：

| 路由 | 鉴权 | 说明 |
|---|---|---|
| `GET /healthz` | 无 | inflight/goroutines/heap（`internal/server/server.go:58-66`） |
| `/api/admin/*` | 静态 admin token OR platform_admin JWT（`internal/server/server.go:68-79` + `middleware.go:38-67`） | 管理面（chi Handle，:73-78） |
| `/api/user/*` | register/login 公开，其余 RequireJWT（`internal/handler/user/router.go:22-40`） | 用户面 |
| `Mount("/")` | AI key 鉴权（proxy） | 8 端点：chat/anthropic/responses + WS + images×2 + search + models（`internal/proxy/router.go:19-58`） |
| `/assets/*`、`/favicon.svg`、`/`、SPA fallback | 无 | 网关内嵌 web/dist（`internal/server/server.go:97-126` + `cmd/server/embed.go:14`） |

- admin 组（`internal/handler/api.gen.go`，openapi 生成；Handler 路由区 :4228，BaseURL `/api/admin`）：`/accounts`（含批量 batch-delete/batch-update、`{id}/ext`、`{id}/groups`）、`/groups`（含 assignments）、`/users`（含 `{id}/groups`）、`/temp-balances`、`/templates`（含 batch、`{id}/ext`）、`/keys`、`/pricing`（含 `/pricing/sync`；PUT/DELETE 的 model 在 query）、`/image-price`、`/function-prices`（价格表三件套）、`/rules`、`/settings`、`/redemption-codes`（含 batch-deactivate、`{id}/deactivate`、`{id}/uses`）、`/usage_logs`、`/err_logs`、`/stats`、`/overview`、`/users-top`、`/ops/workers`（ops tag 契约化并入管理面，`internal/handler/ops.go:62`）。
- user 组（`internal/handler/user/api.gen.go:1303`，路径自带 `/api/user` 前缀）：`/api/user/auth/login|register|me`、`/api/user/keys`（含 `{id}`、`{id}/rotate`）、`/api/user/groups`、`/api/user/usage_logs`、`/api/user/err_logs`、`/api/user/stats`、`/api/user/redemptions`、`/api/user/temp-balances`、`/api/user/auth/change-password`。
- AI 组（`internal/proxy/router.go:19-58`）：`POST /v1/chat/completions`、`POST /v1/responses`（upgrade → WS）、`GET /v1/responses`（仅 upgrade 放行，否则 405）、`POST /v1/messages`、`POST /v1/images/generations`、`POST /v1/images/edits`、`POST /v1/alpha/search`（codex CLI web search 独立编排，`forward_search.go:59`）、`GET /v1/models`（快照读零 DB，`models.go:19`）。

## 12. 配置面

`config.example.toml` 段落 → 模块映射（对照真实文件，`cmd/server/main.go` 消费，现 564 行）：

| 段落 | 消费模块 | 关键项 |
|---|---|---|
| `[server]` | server.NewServer + http.Server | addr/read_header_timeout/max_header_bytes |
| `[log]` | logx.New | level/output |
| `[admin]` | server 静态 token | token |
| `[auth]` | jwtauth.Issuer | jwt_secret（`C3API_AUTH_JWT_SECRET` 亦可） |
| `[db]` | repository.OpenPG | dsn/max_conns（20 = billing 8 + stats 8 worker + 余量）；**F1 OpenPG 自动补丁**：lock_timeout=5s 会话级 + 计费扣费 per-query 10s 超时 + MaxConnLifetime=30m 滚动轮换——DSN 无需手工配置，用户 DSN 显式同名参数时尊重不覆盖（`config.go:56-61` + `main.go:134` 注释） |
| `[proxy]` | proxy.New | max_body_size/max_inflight/upstream_timeout/upstream_stream_timeout/failover_attempts/usage_capture |
| `[upstream]` | httpx.TransportConfig | 连接池参数（max_idle_conns 8192 / per_host 2048 / force_http2 / idle_conn_timeout 90s / dial_timeout 10s，`config.example.toml:20-26`） |
| `[limit]` | fixedWindowLimiter | group_key_rpm（0 = 关）；**cooldown_429/backoff_* 已移除**（配置含这些键将启动失败，规则引擎接管） |
| `[scheduler]` | scheduler.Config | default_max_concurrency/sync_interval |
| `[usage]` | usage.Recorder + StatsAggWorker + ErrLogWorker + RetentionWorker | batch_size/flush_interval/log_retention_days=30/quota_flush_interval/flush_workers=8/stats_agg_interval（默认 5m，0=禁用聚合）/errlog_queue_size=4096/errlog_batch_size=500/errlog_flush_interval=500ms/errlog_retention_days=7/stats_retention_days=180 |
| `[billing]` | billing.NewFlusher + BillingHooks | enabled（**默认开启**——2026-08-15 用户裁决，`config.example.toml:60,69` `enabled = true`；注：`internal/config/config.go:139` 代码默认值仍 false 未同步）/flush_interval=1s/balance_refresh_interval=10s/flush_workers=8 |

- 必填校验（`internal/config/config.go` Load 末尾 validate）：admin.token、auth.jwt_secret、db.dsn 缺失/占位符即 fatal（占位符精确匹配拒绝：change-me/change-me-too/dev-admin-token/dev-jwt-secret-for-local）。
- 分区/保留/倍率等策略参数在 **DB settings 表**而非 config（`internal/domain/settings.go:10-38`）：signup 默认资源、price_source_url/price_sync_cron、service_tier_policy_*、cluster.instances。

## 13. 架构决策记录（ADR）

每条 = 用户裁决/评审定稿 + **为什么** + 来源锚点（代码注释位置或 plan 文件名）。

1. **三词原则（性能 / 边界 / 优雅）**——所有方案取舍的评判标准。为什么：压测驱动演化（53k/s 目标）中每个"快"都要有边界（哪些路径不碰）与优雅（停机/失败不丢语义）。来源：`docs/superpowers/plans/2026-08-11-errlog-task.md:5`。
2. **不计费不入 usage_logs**——usage_logs 成员资格按**放行路径语义（error_type ∈ {none, abort}）**判定，与 cost 无关。为什么：cost>0 判定会漏掉免费分组（倍率 0 成功行）与 0 token 成功行（空响应）；失败行（4xx/5xx/network）不入 usage_logs（P2a 拒绝风暴教训：每请求一条明细即无界积压与写放大源头），错误审计归 err_logs。来源：`internal/proxy/forward.go:377-391`（routeLog 注释）+ `docs/superpowers/plans/2026-08-11-errlog-task.md:12`。
3. **表三分**——usage_logs（计费明细 30 天）/ err_logs（错误明细 7 天）/ usage_stats（聚合统计 180 天），三表独立保留期。为什么：错误审计与计费明细生命周期/查询面不同，混表使两边互相拖累（瘦身 + 短保留）。来源：`docs/superpowers/plans/2026-08-11-errlog-task.md:25` + `internal/repository/partition.go:22-31`。
4. **分区 DROP 不 DELETE**——保留清理全部 DROP TABLE O(1)（比逐行 DELETE 快 5~6 个量级）。为什么：50k 并发量级 usage_logs ~4.3 亿行/天，逐行 DELETE 不可行；PG DELETE 不释放空间（usage_stats 180 天清理必须 DROP）。来源：`internal/repository/partition.go:22-31` + `internal/usage/retention.go:62` + `config.example.toml:55-56` 注释。
5. **双队列豁免采样**——err_logs 按来源（provenance）分队列：豁免队列（abort/failover 已计费错误）恒落盘，普通队列（401/429/402/400/404 拒绝 + 组限流）风暴采样丢弃。为什么：不可按 error_type 推断来源（Err429/ErrBilling/ErrAuth 在拒绝类与双轨类同时出现）；已计费错误审计价值最高。来源：`internal/usage/errlog.go:10-16` + `docs/superpowers/plans/2026-08-11-errlog-task.md:10,14`。
6. **快照注册表边界（#13 已合并）**——注册表不接管模块周期 ticker、不做数据缓存、不进入请求热路径。为什么：避免双 reload 竞争与热路径锁；快照数据形态与周期刷新保持各模块自管。来源：`internal/snapshot/snapshot.go:9-19` 包注释（用户拍板 2026-08-11）。
7. **单 worker 批量落盘（err_logs）**——无多 worker 并行必要。为什么：DB 写是瓶颈，写速率由 BatchSize/FlushInterval 钉死有界，采样兜底防积压。来源：`docs/superpowers/plans/2026-08-11-errlog-task.md:23`。
8. **usage_logs 瘦身**——去 error_message + status_code（保留 error_type，值域收敛 none/abort）。为什么：错误排障列由 err_logs 承载（status_code integer + error_message），明细表瘦身降写放大。**2026-08-15 align 补列机制删除后无"存量库"概念**（全新建库），原"bootstrap 只加不减幂等"兼容面随之失效。来源：`internal/ent/schema/usagelog.go:33-39` + `internal/ent/schema/errlog.go:12,35`。
9. **锁顺序一致化防死锁（P3）**——批量 upsert 批内按 model 排序 + 40P01 重试。为什么：多实例并发同批 model 取锁顺序交错 → deadlock detected（压测启动期偶发）；排序消除主因，重试兜底残余。来源：`internal/repository/pricing_repo.go:62-66`（sort.SliceStable）+ `internal/repository/litellm_upsert.go:19,28,61,92-96`（litellmExecBatchWithRetry / isDeadlock）。
10. **ent migrate 跳过分区表**——分区 DDL 由 bootstrap 独占管理。为什么：atlas 对分区表 diff 规划期必失败（真实 PG 实测，ent v0.14.6 + atlas v0.36.2 + PG18）。来源：`internal/repository/partition.go:570-582`。
11. **基准后新增裁决（2026-08-14/15 批次）**——① billing 默认开启反转（2026-08-15 用户裁决，`config.example.toml:60,69`）；② 使用量统计离线聚合化（2026-08-14 spec，新 worker stats-agg + stats_agg_watermark 表）；③ image_price per-million 口径破坏性变更（2026-08-14）；④ stats_flush_interval→quota_flush_interval 改名（2026-08-15，不向后兼容）；⑤ /stats + /user/stats USD 口径 + TTFT 重写（2026-08-14）；⑥ 账号级 base_url（2026-08-14）；⑦ align 补列全家删除（2026-08-15，存量库兼容面消灭，全表从零创建）。

## 14. 性能基准

- **目标与实测（w3 轮，2026-08-11）**（`docs/superpowers/plans/2026-08-11-w3-loadtest.md` §一，压测机（内部环境，IP 存部署清单） / 24 逻辑 CPU / 62GB，DB postgres:18 同机，loadtest 工具交错跑 + 每请求 CPU 同口径）：
  - 目标 **53k/s**；实测 10k 并发 **56.3k** total QPS（billing off）/ 50.8k（billing on）、20k **53.3k**、25k **59.9k**——均 ≥ 历史峰值 52.3k，无吞吐塌陷（`docs/superpowers/plans/2026-08-11-w3-loadtest.md:31-33`）。
  - 429 为种子 key 池在途门禁分摊（20k 并发 28.3%、25k 37.7%），非缺陷。
  - **时效注记**：上述数字为 w3 轮历史事实，基准后另有新轮次未收录（2026-08-13）：resp HTTP 10k 并发 33.5k→35.9k QPS、每请求 CPU -15~-17%；resp-ws 2000 连接 86k rounds/s、每轮 CPU 114µs、0 错误（`2026-08-13-resp-loadtest-retest.md`）；sdk-wiring chat 基线 51.5k QPS（落带 46.6-53.9k）、images 直连 70.0k/169µs、SDK 非流式 45-47k、流式 41k/344µs（`2026-08-13-sdk-wiring-loadtest.md`）。与 w3 数字不冲突（51.5k 落带含 w3 值），引用时勿当作最新。
- **转换成本数据**：见 §5（off 基线 52.3k / 232.8µs；on 差距收敛 -10.9% QPS / +17.8% CPU；pprof 转换路径 22.6%→8.5%）。W4 strip 预筛 ≈1.5% QPS / ~1% CPU 差异（`docs/superpowers/plans/2026-08-11-w3-loadtest.md:65`）。
- **风暴教训**（压测 2026-08-11 修复链，`internal/repository/pricing_repo.go`/`internal/billing/flusher.go`/`internal/repository/partition.go` 注释 + `docs/superpowers/plans/2026-08-11-errlog-task.md`）：
  - **P1 分区漂移**：price 快照列/ttft_ms 合入后旧分区表缺列 → 新二进制连旧库 INSERT 42703 全停；当时修复 = bootstrap 幂等补列 ALTER（`partition.go:83-95,357-362`）——**该机制已删（2026-08-15）**，现以**全新建库**消除存量漂移面（无补列路径）。
  - **P2 巨批**：单用户积压 1M+ 行 → 单事务 2000+ 分片串行 8 分钟（xact_age 08:02 实证）+ 堆涨 4.6GB；修复 = 单用户拆事务 ≤10k 行/事务（`internal/billing/flusher.go:34-39`）。
  - **P3 缺名帧**：非规范上游缺 `event:` 行 → 流式转换断流；修复 = data `type` 字段推断事件名（`internal/protoconv/protoconv.go:167-175`）。
  - **P2a 拒绝风暴**：单 key 限流 161k req/s → 60s 冲至 9.8M pending 行 / RSS 7.5GB；修复 = 拒绝行不入 usage_logs 明细 + err_logs 有界队列采样（`internal/proxy/forward.go:347` recordRejected）+ 积压续传循环 `backlogDrainBudget`（`internal/billing/flusher.go:41-51`）。
  - **P2b 停机截断**：O1 复测在途批次 Background ctx 令停机拖至分钟级；修复 = baseCtx 可取消 + Close 预算约束截断 Warn（`internal/billing/flusher.go:95-99,228-283`）。
- 测试基座：业务/集成测试全量真实 PostgreSQL（独立库命名 `c3api_test_*` 避开竞争）；pgxmock v4 仅存单测桥接与历史保留（`internal/repository/repository_test.go:21,33,43` + `pg_account_groups_test.go:24`），不用于业务/集成测试。

## 15. 给 agent 的纪律清单

改动前逐条对照：

1. **改热路径**（proxy 包）：先读 §3——热路径零 DB 零 per-request 锁；开关关闭 = 快照读 + 分支、开启 = 预筛零解析；投递永不阻塞；每加一步检查是否引入锁/DB/分配。改完必须跑 `go test ./internal/proxy/... -race` + 压测复测（转换 on/off 交错对比，见 §14 方法论）。
2. **加快照/改快照字段**：读 §7——快照形态 atomic 整表换入或 RWMutex 整体换 map；刷新源三选一（invalidate 事件 / 周期 ticker / 启动同步），新增字段必须接一个刷新源；**新快照走 snapshot.Registry 注册**（`cmd/server/main.go:248-263` Register + :443 ReloadAll 统一首刷，scope 分发经 dispatcher :266-271）——注册表只持元数据，不接管 ticker/不缓存/不进热路径；快照数据实现与刷新源仍各模块自管。
3. **加表**：ent schema 新文件（snake_case 命名勿混）+ repository 子仓库 + service/handler 接线；**分区表**（usage_logs/err_logs/usage_stats 同型）必须同时改 `internal/repository/partition.go` 列事实源 + **在场/取反断言锚测试**（`internal/repository/partition_internal_test.go:47` `TestUsageLogColumnDefsMatchCreateDDL`——DDL↔事实源列集合相等 + 新列在场 + 旧列取反；align 补列删除后保留的防漂移职责，头注释 :9-11）+ bootstrap 调用 + retention 配置。
4. **加 worker**：实现 `worker.Worker` 契约（幂等 Start/Close、Close 未 Start 安全）→ `cmd/server/main.go` 注册（注意注册顺序决定反向排空次序：产生流量的 worker 后注册先排空，旁观者最后注册最先关）→ 有背压面时声明背压/丢弃/观测语义（水线或丢弃计数）。
5. **接线（跨包发布/回调）**：读 §9——跨层一律接口化 + cmd/server 装配侧粘合（notify 不 import invalidate、scheduler 不 import notify 是硬约束）；NOTIFY 变更必须进 dispatcher 映射表 + 载荷守卫（>6KB 降级 full）。
6. **计费改动**：读 §10 + ADR 2/3——扣费恒条件 UPDATE（FEFO + 行锁），预检只是软门禁；usage_logs 只收放行路径行；billing.enabled=false 时 `invBalances` 必须保持 nil 接口（防 typed-nil panic，`cmd/server/main.go:217-221`）。
7. **测试基座**：repository/service 测试 = 真实 PostgreSQL（独立库命名 `c3api_test_<任务名>` 避开并行竞争），不用 sqlite/pgxmock（pgxmock 仅存单测桥接）；必须 `-race` + gofmt 干净。
8. **流程**：SDD 三件套——spec 先行（`docs/superpowers/plans/` 当日日期命名）→ spec review 通过才派实现 → 实现 → 内容评审（验收标准核对锚点/裁决清单）→ 合并 main（--no-ff）→ 清理 worktree/分支。commit 中文一句话。
9. **文档纪律**：本文档锚点随代码漂移——改代码位置时同步更新 file:line；合并时若 main 已前进需重核锚点（spec 事实纪律 2）。
