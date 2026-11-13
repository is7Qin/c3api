# pkg/egress — 出口规格与出口 client 池

> 根 `AGENTS.md` 覆盖项目级约定。本文件只讲出口池（`Spec`/`Key` 纯值层 + `Pool`
> client 注册表）的职责与不变量。

## 职责 / 入口

- `spec.go`：`Spec`（scheme/host/port/认证，零值 = 直连）、`Key()` 决策摘要、
  `ResourceKey(*Client)`。纯值层：不碰数据库、不建连接、不认识业务实体。
- `pool.go`：`Pool` 是**进程级持久**的出口 client 注册表。
  - `NewPool(base)`：base 为直连底座（生产 = main 里与 aiclient Factory 同一个
    client）；构造期一次由 base 派生 Codex 底座（`codexClientOf`）。
  - `Direct()` 直连单例；`For(spec)` 按 `Spec.Key()` 复用或克隆（同代理、独立连接池）；
    `Detach(active)` 摘除不再引用的 client；`CloseClients` 锁外关空闲连接。
- `Client{Spec, HTTP, Codex}`：`HTTP` 给通用上游；`Codex` 是同一出口由底座派生的
  **关闭自动压缩**副本（`DisableCompression=true`，Go 默认会加
  `Accept-Encoding: gzip`，官方 Codex 客户端不发），自有连接池、同 Spec 同代理。

## 不变量

- 热路径只读快照上已解析好的 `*Client` 指针：0 次 map 读、0 分支、0 分配；`For`/
  `Detach` 只在快照期与管理面写后调用。
- 换出口必须换连接池（专属 Transport），否则 keep-alive 连接会跨出口复用。
- Codex 出站（sdkbridge HTTP/WS、probe 的 codex seam）只用 `Client.Codex`；生产里
  恒非 nil。底座 transport 类型未知（仅测试的改写 RoundTripper）时**不派生**，
  `Codex == HTTP`；不得照搬 `cloneTransport` 退回 `http.DefaultTransport` 的规则
  （会让测试流量绕过改写 transport 打真实上游）。
- `For` 对 HTTP 与 Codex 两个底座分别按 spec 克隆；`DisableCompression` 随 Clone
  保留。`CloseClients` 两份连接池都关（同一 client 只关一次）。
- **连接配额翻倍**：Codex 底座是独立连接池，`MaxConnsPerHost`/空闲连接等配额随 Clone
  原样继承、不与 `HTTP` 分摊——每个出口（直连与每个代理 spec）的实际连接上限是配置值
  的两倍。容量规划与上游/代理的连接数限制按两倍估算。
- `NewPool` 签名与 `httpx.TransportConfig` 不为 Codex 增加参数：调用方不必知道
  Codex 要关压缩，参数来源只有一处底座。
- 能力接口（`proxyCloner`、`compressionDisabler`）声明在本包，`pkg/httpx` 的
  `ShardedTransport` 凭结构实现；本包不 import httpx。

## 边界

- schema 白名单校验在管理面写路径，不在本包。
- 调度器快照只吃 `Key`/`*Client`；连接生命周期挂在快照 reconcile（`Detach` +
  `CloseClients`），不随单次请求变化。

## 验证

```bash
go test ./pkg/egress/ ./pkg/httpx/ -count=1
go test ./pkg/egress/ -run TestCodexTransportNoAutoGzip -count=1
```
