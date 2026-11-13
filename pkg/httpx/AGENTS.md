# pkg/httpx — 上游 HTTP 底座构造与分片 transport

> 根 `AGENTS.md` 覆盖项目级约定。本文件只讲上游连接底座的构造参数与
> `ShardedTransport` 的克隆能力。

## 职责 / 入口

- `httpx.go`：`TransportConfig` → `NewTransport` / `NewClient`。生产只有 main 一处
  `NewClient`，它是 aiclient Factory 与出口池（`pkg/egress`）共用的唯一底座。
- `shard.go`：`ShardedTransport` 把上游并发摊到 N 个独立 `*http.Transport` 连接池
  （新请求优先给在飞最少的分片；只承诺选路，不承诺“1 分片 = 1 连接”）。
  - `CloneWithProxy(proxy)`：出口池按代理 spec 派生独立副本。
  - `CloneWithCompressionDisabled()`：逐片 `Clone()` 后设 `DisableCompression=true`，
    供出口池派生 Codex 出站底座。

## 不变量

- `Proxy` 为 nil 即显式直连，**不得**回退 `http.ProxyFromEnvironment`（HTTP_PROXY 会
  静默改道全部上游请求，含凭据）。
- 参数只有一处来源：Codex HTTP/WS 用的是出口池由同一底座派生的副本，连接配额等参数
  随 Clone 继承；不为 Codex 在 `TransportConfig` 加字段。
- 克隆出的分片**不与原分片共享连接池**；对关闭压缩的副本再 `CloneWithProxy`，
  `DisableCompression` 随 Clone 保留。
- 两个克隆方法返回 `http.RoundTripper`：出口池凭结构认领能力，不 import 本包。
- `countedBody.Close` 幂等且计数不为负（调用方存在重复 Close）。

## 边界

- 头策略（`relayDeny`、`Accept-Encoding` 剔除）在 `pkg/aiclient`，不在本包。
- 本包不知道 Codex、出口、账号；只提供底座与克隆能力。

## 验证

```bash
go test ./pkg/httpx/ -count=1
go test ./pkg/httpx/ -run TestShardCloneWithCompressionDisabled -count=1
```
