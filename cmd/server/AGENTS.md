# cmd/server — 组合根

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲
> 装配边界（`main.go` + `wiring_*.go`）。

## 职责 / 入口

- 装配分 6 个阶段（`wiring_{infra,health,notify,billing,codex,run}.go`，每文件一个
  构造函数），只允许"后阶段依赖前阶段"；`main.go` 只剩 flag/config/两个 protoconv
  sink/`run(ctx)`，配置 → PG/Redis/worker → repository → service → handler/router →
  HTTP/WS → 优雅退出这条链由阶段顺序表达。
- 阶段 2–5 **不再收整个 `*xxxFace`**，而是各收一个窄结构（`healthDeps` /
  `notifyDeps` / `billingDeps` / `codexDeps`）：字段集就是本阶段真实读到的字段，
  在 `main.go` 的调用点一行行投影出来。新增 infra 产物**默认对所有阶段不可见**，
  要让它可见必须改窄结构（`stage_deps_test` 会钉住字段数）。
- 唯一允许的前向引用是 `infraFace.supplierSnap`（billing 阶段写入、retention 回调
  运行期读）。窄结构里它必须是 `supplierSnapshotSlot`（保存字段地址）而不是
  `*SupplierSnapshot`：按值投影会在 billing 之前拷贝到 nil，磁盘满是保护静默失效。
  其余符号不许晚绑定，禁止服务定位器/聚合根。
- `defer` 只留在 `main`/`run` 这一层（阶段函数零 defer）：阶段返回后仍持有句柄，
  只有进程级生命周期能保证"最后一步释放"。
- 探测装配：`accountprobe.New(clients, egressPool)` 只传执行依赖（aiclient
  Factory + 出口池）；准入（4+16）与 30s 总期限在 `internal/service`，不在本层
  创建 prober 超时/信号量。
- 依赖显式顺序构造；禁止事后 `Set*` 回填（构造环结构性消除）。
- worker 注册序 = 反序排空序；改顺序前读注释。
- Codex 会话簿：`convBook := codexsdk.NewConversationBook(redisstore.New(rdb))` 恒构造（与
  续接 store 同一个 `rdb`，连接池生命周期归 redisx），经 `proxy.Deps.Conversations` 注入；
  `convW := proxy.NewConversationWorker(convBook, log, …)` 在 `managedWorkers` 里紧跟
  `contBindW`（`worker_order_test` 断言紧邻），反向停机时在 rec/errlogW 之前最后 Flush。
  其 `Stats()` 经 `statsProviders` 进 `/api/admin/ops/workers`（`ops_stats_wiring_test`）。

## 不变量

- 不在本层做业务判定/上游错误分类；错误归一在 service/probe/handler。
- 日志只经 `pkg/logx`；常驻 goroutine 只经 `internal/worker`。
- 生成物 `go:embed dist` 只由构建脚本更新；不手改。
- 关闭路径必须使用 `context.WithoutCancel` 的边界与现有总 deadline 语义。

## 验证

```bash
go build ./...
go vet ./...
go test ./internal/service/ -run TestProbe -count=1
```

- **BPS 适配器**：在 billing 阶段构造一次（failure handler 的 repos/sched/latch/pub/log
  都已就绪），经 `codexDeps.bps` 复用给数据面与探测；`probeBPS` 也在 `wiring_billing.go`
  内，**不得使用 `defer`**（关停链测试统计该文件的 defer）。
