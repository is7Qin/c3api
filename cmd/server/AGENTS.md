# cmd/server — 组合根

> 根 `AGENTS.md` 覆盖项目级约定；`internal/AGENTS.md` 是分层地图。本文件只讲
> `main.go` 的装配边界。

## 职责 / 入口

- `main.go` 是唯一组合根：配置加载 → PG/Redis/worker → repository → service →
  handler/router → HTTP/WS → 优雅退出。
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
