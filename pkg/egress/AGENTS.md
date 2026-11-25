# pkg/egress — 出口 client 的实现层（池 + 装配）

> 根 `AGENTS.md` 覆盖项目级约定。本文件只讲出口池与装配（`Pool` / `NewPool` /
> `For` / `Detach` / `NewClient` / `CloseClients`）的职责与不变量。
>
> **值类型不在本包**：`Spec` / `Client` / `Flavour` / `ResourceKey` 的 canonical 定义
> 在 `pkg/egressapi`（P5-3-A 迁出，见那里的 `AGENTS.md`）。本包的 `compat.go` 只为既有
> 调用方保留同名**别名**（同一类型，不是包装），移除条件写在该文件里——别名不是终态。

## 职责 / 入口

- **规格构造点在 `pkg/egressapi.New`**（含归一化：scheme 小写、host 去 IPv6 方括号；
  `[::1]` 与 `::1` 必须同摘要）。归一化定义的就是 `Spec.Key` 的语义，故与 `Spec` 同居。
  本包 `compat.go` 只为既有调用方保留同名一行转发；**新调用点直接写 `egressapi.New`**。
- `pool.go`：`Pool` 是**进程级持久**的出口 client 注册表。
  - `NewPool(base)`：base 为直连底座（生产 = main 里与 aiclient Factory 同一个
    client）；构造期一次由 base 派生关闭自动压缩的底座（`codexClientOf`）。
  - `Direct()` 直连单例；`For(spec)` 按 `Spec.Key()` 复用或克隆（同代理、独立连接池）；
    `Detach(active)` 摘除不再引用的 client；`CloseClients` 锁外关空闲连接
    （逐个转交给 `Client.CloseIdle`，遍历口味只有一个实现点）。
  - `NewClient(base, spec)`：**一次性**出口 client（按值克隆底座 + 专属 Transport），
    **不进注册表**——探测这类非快照期动作专用，用完由调用方 `CloseIdleConnections`；
    代理 URL 与 `For` 共用 `proxyURL`（唯一实现点）。注意与
    `egressapi.NewClient(spec, slots)`（装配出口**句柄**）不是同一件事。
- `compat.go`：值类型的别名 + `ResourceKey` / `New` 一行转发。只许转发，不许在这里加逻辑。

## 不变量

- 热路径只读快照上已解析好的 `*egressapi.Client` 指针：0 次 map 读、0 分支、0 分配；
  `For`/`Detach` 只在快照期与管理面写后调用。
- 换出口必须换连接池（专属 Transport），否则 keep-alive 连接会跨出口复用。
- Codex 出站（sdkbridge HTTP/WS、probe 的 codex seam）只用 `Client.NoCompression()`；
  生产里恒非 nil。底座 transport 类型未知（仅测试的改写 RoundTripper）时**不派生**，
  `NoCompression() == HTTP()`；不得照搬 `cloneTransport` 退回 `http.DefaultTransport`
  的规则（会让测试流量绕过改写 transport 打真实上游）。
- `For` 对两个口味底座分别按 spec 克隆；`DisableCompression` 随 Clone 保留。槽位表由
  本包填充、由 `egressapi.Client.CloseIdle` 关闭：两份连接池都关（同一 client 出现在
  两个槽位就关两次，`CloseIdleConnections` 幂等）。
- **连接配额翻倍**：关闭压缩的底座是独立连接池，`MaxConnsPerHost`/空闲连接等配额随
  Clone 原样继承、不与 `HTTP()` 分摊——每个出口（直连与每个代理 spec）的实际连接上限
  是配置值的两倍。容量规划与上游/代理的连接数限制按两倍估算。
- `NewPool` 签名与 `httpx.TransportConfig` 不为该口味增加参数：调用方不必知道
  Codex 要关压缩，参数来源只有一处底座。
- 能力接口（`proxyCloner`、`compressionDisabler`）声明在本包，`pkg/httpx` 的
  `ShardedTransport` 凭结构实现；本包不 import httpx。

## 边界

- schema 白名单校验在管理面写路径，不在本包。
- 调度器快照只吃 `Key`/`*Client`；连接生命周期挂在快照 reconcile（`Detach` +
  `CloseClients`），不随单次请求变化。
- 规格的**形式**（哪些 scheme 合法、端口范围）是写面的事；本包只做归一化与装配。

## 验证

```bash
go test ./pkg/egress/ ./pkg/egressapi/ ./pkg/httpx/ -count=1
go test ./pkg/egress/ -run TestCodexTransportNoAutoGzip -count=1
```
