# PROJECT KNOWLEDGE BASE — c3api

**Generated:** 2026-08-24 · **Commit:** b8a6477(F2 合入) · **Branch:** main
**Deep-dive memory:** `.omo/memory/MEMORY.md`（主题蒸馏 + 历史裁决，本地不入 git）

## OVERVIEW

自托管 AI 网关：单 Go 二进制（内嵌 React 控制台）统一暴露六种请求格式——OpenAI Responses（REST+WS）、Anthropic Messages、Chat Completions、Images、Codex search、模型列表。无状态网关 + PostgreSQL 18 唯一依赖；跨实例失效走 NOTIFY `c3api_invalidate`。Beta 状态：schema/config 无迁移路径，升级=全新部署。

## STRUCTURE

```
cmd/server/       # 组合根：main.go 全部装配顺序；dispatcher(NOTIFY→invalidate)；auth_sync worker；go:embed dist
internal/
  proxy/          # 热路径：guardPipeline→failoverLoop→caller_*；不变量：零 DB、零每请求锁
  handler/        # /api/admin(oapi-codegen 生成路由) + /api/user(JWT) + httpface(错误写出收敛)
  service/        # 业务逻辑；errors 叶子包=5 哨兵错误；publish/reload 用 WithoutCancel
  repository/     # pgx+ent 数据访问；日分区引导(usage_logs/err_logs/usage_stats)
  ent/            # GENERATED——禁止手改
  scheduler/      # 选号快照；copy-modify-Store 不可变视图
  rule/           # 规则引擎；Kind 五值 ok/429/4xx/5xx/network
  billing/        # 计费游标消费者(F2 ledger-cursor)：Cost 纯函数、Balances 快照、FlusherStats(lag 族)
  usage/          # usage_logs 唯一写点(InsertBatch)+在线配额累加+watermark 离线聚合
  pricing/ notify/ invalidate/ credential/ sdkbridge/ protoconv/ domain/ config/ worker/ snapshot/ auth/ server/
pkg/              # 可复用层：aiclient(SDK 工厂)、sserelay(SSE 中继)、httpx、logx、cryptox
web/              # React+TS+Vite+shadcn 控制台（见 web/AGENTS.md）
tools/            # e2e(build tag e2e)、loadtest(+setup)、fakeupstream
openapi/ deploy/ scripts/build.sh   # 无 Makefile
```

## WHERE TO LOOK

| 任务 | 位置 |
|---|---|
| 改 AI 格式路径 | `internal/proxy/forward_*.go`(HTTP 面) → `caller_*.go`(上游策略) → `pipeline.go`(守卫骨架) |
| 协议转换 | `internal/protoconv`（chat↔resp/mess 四方向全网格；仅 chat→resp 是 gjson 字节级） |
| 上游客户端/鉴权注入 | `pkg/aiclient`（Factory 双缓存；rawPostCT 零透传契约） |
| 新增管理端点 | `openapi/openapi.yaml` → 重生成 → `internal/handler/*.go` 实现 → `web` 跑 `pnpm gen:api` |
| 扣费链路 | `internal/proxy/forward.go`(routeLog 出生定态) → `internal/repository/billing_cursor.go`(游标消费) → `internal/billing`(Cost 纯函数/Balances 快照) |
| 定价 | `internal/pricing`（换算系 ×1e5 与 ×1e11 禁混用） |
| 用量/统计 | `internal/usage`（日分区+watermark 离线聚合） |
| 规则/限流/选号 | `internal/rule` + `internal/scheduler` |
| 配置项 | `config.example.toml` + `internal/config/config.go`（fail-fast 校验） |

## CODE MAP（热路径核心符号）

| 符号 | 类型 | 位置 | 职责 |
|---|---|---|---|
| `guardPipeline` | method | internal/proxy/pipeline.go:43 | auth→quota→余额预检→两级并发门→限流 骨架 |
| `handleFormat` | method | internal/proxy/caller.go:126 | 4 种 REST 格式共享入口（scanKeys 单遍提取） |
| `UpstreamCaller` | iface | internal/proxy/caller.go:98 | 一格式一实现； owns 记录/failover 分类 |
| `relayWS` / `wsRelayTransport` | method/iface | internal/proxy/ws_relay.go:54/:35 | WS 双向中继合一；5 方法隔离 responses vs codex |
| `ConvertRequest`/`StreamMapper` | func/struct | internal/protoconv/protoconv.go:29/:84 | 请求转换四方向分发；流式逐帧零分配映射 |
| `Factory` / `rawPostCT` | struct/method | pkg/aiclient/aiclient.go:40/:205 | 模板级客户端缓存+URL 缓存；HTTP 面零透传 |
| `Relay`/`InferEventName` | func | pkg/sserelay/relay.go | 池化 SSE 中继；inLine 续片状态机 |
| `Codex` | struct | internal/sdkbridge/codex.go:27 | codex SDK 适配：账号级缓存+fatal 回调+轮换 |
| `DeductOnlyAndMark` / `markBilledExec` | method/func | internal/repository/billing_repo.go:80 / billing_cursor.go:178 | FEFO 扣减+billed 标记同事务原子；标记行数守卫堵锁丢失双扣 |

## CONVENTIONS

- **仅 chat→resp 方向是字节级(gjson)**，其余转换方向 map-based——优化不对称是有意的，别"统一"
- **方向命名双轨**：`ProtocolConvertXToY`=客户端说 X 上游说 Y；转换函数按数据流命名(`respToChatResponse`)
- **base_url 一律裸根**（拒绝含 `/v1`，防 `/v1/v1/...` 404）；openai 族自动补 `/v1`
- **配置 fail-fast**：未知键、时长<1ms、受控整数<1、占位密钥(change-me 等)全部启动即炸；改 TOML 键名=故意破坏性变更
- **测试**：testify **只 require 不 assert**；PG 测试真实 PostgreSQL（pgxmock 仅 5 个遗留/冒烟文件勿扩展：repository_test/rule_repo_test/pg_account_groups_test/signup_bootstrap_pg_test/notify publisher）；全仓 `t.Parallel()`=0 串行；固定 `time.Date(2026,8,...)` 注入时间；channel 屏障替代 sleep（参考 codex_test.go runWave）
- **前端**：pnpm only（本机须 `--config.node-linker=hoisted`）；schema.d.ts 由 `pnpm gen:api` 生成禁手改
- **commit**：标题一行英文(conventional 前缀)+正文中文；不擅自 push

## ANTI-PATTERNS（本项目明令禁止）

1. 业务代码直接 import zap——日志只走 `pkg/logx`（logx.go:5）
2. HTTP 面加任何客户端头透传——零透传是契约非遗漏（aiclient.go:201）；WS 面白名单外的头同理，Authorization/X-Api-Key 必剔（caller_responses_ws.go:398-416）
3. 未知账号类型静默 fallback 到 api_key——显式报错（forward.go:736）
4. 裸写 scheduler 已发布快照视图——必须 copy-modify-Store（scheduler.go:921）；disabled 账号不得被在途事件复活
5. 响应后副作用跑在请求 ctx 上——NOTIFY 发布/规则重载必须 `context.WithoutCancel`；但裸 WithoutCancel 无界，本地 Apply 包 30s 超时（setting.go:79）
6. 客户端断连当上游错误冷却账号——记 499，不进 failover 分类（forward.go:604）
7. 上游错误原文/内部 ID/key 明文透给客户端——未识别错误归一化 502 "upstream rejected request"（rule/engine.go:414）
8. 复用缓冲切片跨帧/跨回调保留——scanner/relay 缓冲仅回调期内有效（relay.go:23、codex.go:232）
9. 测试 time.Sleep 掩盖竞态——用屏障/watchdog；CI 无 -race，别赌
10. 手改生成物：`internal/ent/**`、`handler/*/api.gen.go`、`web/src/lib/api/schema.d.ts`
11. 破坏 usage_logs 单写点——INSERT 仅 usage flusher InsertBatch（billing worker 只 UPDATE 标记）；billed 出生定态仅 routeLog 一处判定（forward.go:420-423）；扣费标记必经 markBilledExec 守卫，绕开=丢并发双扣防御（billing_cursor.go:178）

## COMMANDS

```bash
# 本地开发（:18080 网关 + :5173 前端代理 /api）
export C3API_ADMIN_TOKEN=local-admin-token
export C3API_AUTH_JWT_SECRET=$(openssl rand -hex 16)
go run ./cmd/server -config config.toml
cd web && pnpm install --config.node-linker=hoisted && pnpm run dev

# 测试（镜像 CI；PG 测试需 TEST_DATABASE_URL，缺省 skip）
docker compose -f deploy/test-compose.yml up -d        # 本地 PG :15432
export TEST_DATABASE_URL="postgres://postgres:c3api@localhost:15432/c3api_test"
go vet ./... && go build ./... && go test -count=1 ./...

# 代码生成漂移检查（CI contract-gen 同款）
go generate ./internal/handler/ ./internal/ent/ && git diff --exit-code
cd web && pnpm gen:api && git diff --exit-code

# 构建（web/dist → cmd/server/dist → go:embed 单二进制）
bash scripts/build.sh
# 或手动： cd web && pnpm build && cp -r dist cmd/server/ && CGO_ENABLED=0 go build -o bin/server ./cmd/server

# E2E（手动跑，CI 不含；计费/分区/优雅退出改动后必跑）
TEST_DATABASE_URL="postgres://postgres:c3api@localhost:15432/postgres" \
  go test -tags e2e -run TestBillingE2E ./tools/e2e -v -timeout 600s

# 压测三件套
go run ./tools/fakeupstream -addr :9100 -chunks 100 -latency 20ms
go run ./tools/loadtest/setup -addr http://127.0.0.1:8080 -admin-token <tok> -upstream http://127.0.0.1:9100 -users 5000 -accounts 5000 -groups 20 -keys-out keys.txt
go run ./tools/loadtest -mode stream -addr http://127.0.0.1:8080 -key ck-xxx -concurrency 10000 -duration 5m
```

## NOTES（踩坑即事实）

- **端口分裂**：单测 PG=:5432(CI)/15432(本地 compose)，e2e 默认 :15432 的 postgres 库自建 c3api_e2e
- **worker 注册序=反序排空语义**：billFlusher 在 listener/authSync 之前注册（main.go:485-491，F2：首位注册→停机最后扫游标），动顺序前读注释
- **typed-nil 陷阱**：invBalances 声明为接口类型非具体指针（main.go:233，2026-08-10 真实 panic 事故）
- **循环依赖靠事后回填**：svc.SetLocalDispatcher / inv.SetSettings 是有意设计
- **ent 迁移跳过 3 张分区表**（atlas 无法 diff），分区由幂等 bootstrap 负责、失败即 fatal
- **chi v5.3.1 双 Mount panic**：/api/admin 用 Handle 不用 Mount；SPA NotFound 会传播进子路由需自行 404 API 前缀
- **http.Server 无 WriteTimeout**（SSE 保护）；上游传输 Proxy:nil 显式直连（HTTP_PROXY 防劫持 C2-1）
- **env 覆盖只替换第一个下划线**：C3API_DB_DSN→db.dsn；前缀必须大写
- **billing 默认开启**：空价格表=所有模型 402，需先 POST /api/admin/pricing/sync
- CI 无 `-race`、无 GOMAXPROCS、e2e 不进 CI——这三条是已知取舍不是遗漏
