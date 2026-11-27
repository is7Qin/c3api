# tools/e2e — e2e harness（build tag e2e）

**定位**：端到端测试只测「真实二进制 + 真实 PG/Redis」。所有外部资源与进程生命周期归
共享 harness（`harness_*.go`），场景文件只写配置、请求与断言。新增 e2e 前先读
`.omo/plans/2026-11-27-e2e-harness-refactor-spec.md`（本地，不入 git）。

## 场景边界（boundary gate 机械守门）

场景 `*_test.go`（非 harness 白名单文件）禁止：

- exec 外部进程 / `go build` 被测二进制（server/fakeupstream 由 harness 起）；
- `CREATE/DROP DATABASE`、字面量 `-migrate`（建库与迁移唯一入口是 harness）；
- docker 调用 / 固定容器名 / 固定回环端口字面量（只允许 `127.0.0.1:0` 与 httptest）、
  固定 e2e 库名、计算型固定端口、场景自建监听（httptest 除外）；
- `cmd.Env = ...` / `os.Setenv`（子进程 env 由 harness deny-list 独占）；
- `t.Parallel`（e2e 串行）。

场景允许：`run.pg` / `run.redisClient` / `run.flushRedis`、in-process httptest 上游、
业务断言。需要 stop/start 基础设施的场景用 `e2eRunOptions{DedicatedServices:true}` +
`run.services.StopService/StartService`（不得出现 docker CLI 调用或 base 容器名）。

## 运行

```bash
# 前置：base PG/Redis（shared 模式）或 docker compose v2+（dedicated 模式）
docker compose -p deploy -f deploy/test-compose.yml up -d --wait
export TEST_DATABASE_URL="postgres://postgres:c3api@localhost:15432/postgres?sslmode=disable"
export C3API_REDIS_ADDR="127.0.0.1:16379"
export E2E_LOG_DIR=/tmp/e2e-logs   # 失败 artifact：gateway/fakeupstream/compose 日志

# 全套（串行；~5 分钟）。CI 同款命令：go test -tags e2e -count=1 -timeout 20m ./tools/e2e
go test -tags e2e -count=1 -timeout 20m ./tools/e2e

# 单场景（逐个 -run，串行；e2e 本机不要并行跑多个 go test）
go test -tags e2e -run '^TestBillingE2E$' ./tools/e2e -count=1 -v -timeout 900s
```

普通 test job（不带 `-tags e2e`）跑 $E2E/$R 之外的两个 gate + 平台/registry 单测：
`go test ./tools/e2e` —— 修改 tools/e2e 任何文件后必须绿。

## 资源隔离（每 run）

- **PG**：`newE2ERun` 在 `TEST_DATABASE_URL` 上新建唯一库 `c3api_e2e_<hosthash>_<pid>_<nano>_<rand>`，
  cleanup `DROP DATABASE ... WITH (FORCE)`；绝不 drop 非 `c3api_e2e_` 前缀库。
- **Redis**：逻辑库 `1..15` 预留文件（`os.TempDir()/e2e-redis-db-<n>.lock`）整个 run 持有；
  **禁止设置 `E2E_REDIS_DB`**——它只用于手动单 run 调试（1..15、写 registry
  `manual_override`），CI 与并发验收禁止。
- **端口**：server/fakeupstream/专用栈全部动态；场景不允许端口字面量。
- **构建/迁移**：每 run 只 build 一次（server+fakeupstream），run 级 lazy migrate 只执行一次
  （首个常驻 gateway 前）；`ExpectExit` 不能作为首个 gateway。
- **temp/log**：`os.TempDir()/e2e-run-*`、`E2E_LOG_DIR`（或 `os.TempDir()/e2e-logs`，
  不随测试进程结束删除，失败 artifact 可收）。

## 基础设施所有权

- shared（默认）：只读 base PG/Redis，隔离靠唯一库/逻辑库；`StopService` 必须返回错误。
- dedicated（`DedicatedServices:true`）：`testdata/dedicated-compose.yml` 渲染成本 run 的
  自包含 compose 副本（唯一 project `e2e-<uuid8>`、预分配显式宿主端口、healthcheck）。
  `docker compose stop/start` **保端口**（空 HostPort 每次 start 会重新分配，故 harness 渲染
  显式端口并在 start 后校验映射）；cleanup 用 `down -v`。共享 base 永不被 mutate。
- registry：`os.TempDir()/e2e-registry/run-<uuid>.json` 先写、state 机 与 child_pids 原子更新；
  kill -9 后下一次 run 的启动期 stale sweep 会强杀孤儿（Windows Job Object
  KILL_ON_JOB_CLOSE / Unix 进程组）再回收库/锁/专用栈；保守策略：活 PID/不确定/异 host/
  坏记录/starting|services_up|spawning 状态一律不回收（宁可泄漏不可误删，>24h 人工清）。

## CI job（.github/workflows/ci.yml 的 e2e）

- `fetch-depth: 0`（assertion/boundary gate 依赖 BASE commit，strict 模式不允许 skip）；
- `docker compose -p deploy -f deploy/test-compose.yml up -d --wait` 起 base；
- env：`TEST_DATABASE_URL=...:15432/postgres?sslmode=disable`、`C3API_REDIS_ADDR=127.0.0.1:16379`、
  `E2E_LOG_DIR=$RUNNER_TEMP/e2e-logs`；
- `go test -tags e2e -count=1 -timeout 20m ./tools/e2e`，job `timeout-minutes: 35`；
- 失败：base compose logs + `E2E_LOG_DIR` upload-artifact；`always` 执行 compose down -v。
- dependabot 分支拿不到 secrets → ci.yml 的 test/e2e job 跳过，dependabot-ci.yml
  （`pull_request_target`）负责其 test（checkout 同样 `fetch-depth: 0`）。

## 单进程全套（CI 形态）与分钟边界竞态（P1 fix2）

- **CI 形态 = 单命令单进程全套**：`go test -tags e2e -count=1 -timeout 20m ./tools/e2e`
  （6 个业务 e2e 同进程串行）。**不改为逐 Test 独立进程**——独立进程只降低分钟边界
  竞态的触发概率，不消除 Test 内/产品视图内的竞态，还会掩盖真实缺口（诊断报告结论）。
- P1 fix2 只做**测试侧结构性修复**（不弱化/不删断言、不用 sleep/重试掩盖）：
  `TestIntelligentRoutingE2E` 的 4a→4b 收敛链把两个质量镜像分开等待与断言
  （见 `routing_e2e_test.go` 文件头「双镜像读/等待原语」）：
  - 镜像 A（durable）= `routing_quality_fact` 全时段、5s 级落库 → 只断言累计样本量；
    等待原语 `rtWaitQualityPredicate`。
  - 镜像 B（plan/窗口）= `[M-5m, M)` settled 分钟 + live 分钟、PG 半部每分钟才刷新、
    primary 还叠加 TTFT/区间带 → 只断言当前决策面；等待原语 `rtPlanPrimarySettledTry` /
    `rtPlanPrimarySettled`（连续两次非空且相同才算稳定）。
  - 4a 收敛门改为「durable ≥2 候选 n>=30」+ plan primary 稳定；4b(i) 轮询前有界等待
    primary 稳定，且**绝不在 leaders=[] 时进入 leader 交叉确认轮询**（该条件数学恒假，
    P1 全套 #1 的 300s 必超时就是此形）；leaders 为空时按编译语义「无 primary ⇒
    bp=10000 100% explore」分派到免交叉确认注活路径。
  - 4b(ii) 恢复后「全员协同」轮询每轮发 10 个 keepalive（与其它阶段同节奏）触发
    quality influx → PG flush → `RequestCompile`，预算 180s（≥3 个分钟边界），失败消息
    带 candidates/degraded/durable/window5m 计数。
- **产品侧 backlog（不在 e2e 内改生产代码）**：windowed quality provider 的 PG settled 半部
  只在 evaluated minute 前进时刷新（`internal/scheduler/quality_provider.go`），而刚 flush
  的分钟行会从 live 半部消失（`internal/quality/live_window.go`）——存在分钟级**可见性
  空洞**：该分钟在 M 内既不在 live 也不在缓存的 PG 里，编译视图瞬时少一个 bucket。
  修复方向（诊断 §4-B）：live source 增 flush watermark/epoch（watermark 前进即局部刷新
  settled），或 PG 窗读扩到 `[M-5m, M]` 并按 minute label 用 settled∩live 去重；并加
  「M 前进后 1s 内完成 bucket flush，随后编译仍看到该分钟」的回归测试 + ops 诊断面
  （SettledBoundary/live 分钟集合）。在此之前 e2e 用两镜像分离写法规避，不得把该抖动
  记成测试 flaky 或加 sleep 掩盖。

## 断言基线（防漂移）

- `testdata/assertion_baseline.json` 由 BASE/source commit **机械生成**：per-Test require 计数 +
  归一化 AST 指纹 multiset + t.Fatalf 计数；gate 用 `git show <source_commit>` 重算比对，
  同 commit 改 baseline 会红。
- 更新规则：**baseline/白名单变更必须单独 commit + 人工 review**（committed gate 的固有极限：
  同一 commit 同时改 gate 源码白名单 + 删断言可绕过）。重生成：
  `E2E_BASELINE_WRITE=1 go test ./tools/e2e -run TestAssertionBaselineGenerate`；
  `E2E_BASELINE_DUMP=1` 打印 per-Test 指纹/表达式供人工核对。
- 断言删除/改写白名单唯一事实源是 gate 源码 `assertionAllowedRemovals`（JSON 必须逐项一致），
  只允许作用于场景 `*_test.go`（harness/gate 文件不得自我豁免），每条须附 BASE evidence 且与
  被移除断言调用相关；「明显恒真/不可达补偿」会被反空断言/不可达检测拦下。
- 反空断言检测是「明显恒真兜底」，不宣称机械完备；新增断言请写成有期望值的 require。

## 边界与取舍

- **只支持单机**：e2e PG/Redis 与测试进程必须同 host；异 host registry 记录永不自动回收
  （人工清）。Redis 逻辑库上限 15 个并发 run。
- **性能 1.3x 仅报告验收**（对比 06e21762 的 6 e2e 中位 294.77s），不进 CI 硬时间门禁；
  CI 只做功能 PASS。每 run 构建一次、无新增 sleep。
- 不改生产代码/config schema；不新增外部 Go 依赖；testify 只用 `require`。
