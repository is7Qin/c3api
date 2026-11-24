# pkg/egressapi — 出口的值类型（中立层）

> 根 `AGENTS.md` 覆盖项目级约定。本文件只讲本包的边界与不变量。

## 为什么有这个包

P5-3-A 把出口的**值类型**从 `pkg/egress`（实现层）搬到这里，理由是方向：调度器的核心
路由只该依赖"出口**是什么**"，不该依赖"出口**怎么被建出来**"。搬走之后，
`internal/scheduler` 的 `Config.Egress *egress.Pool` 仍在（那是 P5-3-B 的端口化），但
快照、静态键与资源键都只认本包的类型。

**硬边界（无豁免）**：本包只 import stdlib，**不得** import 任何 `internal/*`（
`internal/architecture` 的硬规则，pkg 侧没有 legacy 通道），也**不得** import
`pkg/egress`——那会立刻成环。

## 职责 / 入口

- `spec.go`：`Spec`（scheme/host/port/认证，零值 = 直连）、`Key()` 决策摘要、
  `Addr()`、`String()`（脱敏）。**规范化不在本包**：唯一构造点是 `pkg/egress.New`
  （scheme 小写、host 去 IPv6 方括号）——它的归一化与 `Key()` 的稳定性是同一件事，
  搬它属于 `pkg/egress` 整体收缩那一步。
- `flavour.go`：`Flavour` 闭集 + `FlavourHTTP` / `FlavourNoCompression` / `NumFlavours`。
  数组下标必须是**具名常量**（内联后与读字段同价，见设计 §2）。
- `client.go`：`Client{Spec, clients[NumFlavours]}` + `NewClient` + `HTTP()` /
  `NoCompression()` / `CloseIdle()`。
- `resourcekey.go`：`ResourceKey(*Client)`，直连 → 零值，代理 → `Spec.Key()`。

## 不变量

- **槽位表归 `Client` 所有**：构造只有 `NewClient`（按值拷入槽位数组），关闭只有
  `CloseIdle`（遍历全部槽位）。新增口味时填充与关闭的两处循环都自动覆盖——"加了口位
  忘了关连接池"因此写不出来。
- **`NewClient` 生产侧只许 `pkg/egress.Pool`（`NewPool` / `For`）调用**：槽位表里装
  什么（proxy URL、transport 克隆、`DisableCompression`、连接配额）是实现侧的结论。
  测试可以直接写 `egress.Client{Spec: spec}`（槽位全 nil，accessor 返回 nil）。
- 零值 `Client` = 直连，全部读取面对 nil 安全（`CloseIdle` 对 nil 接收者也安全）。
- 本包的类型被 `pkg/egress` 用**类型别名**转出（兼容层，见
  `pkg/egress/compat.go` 的移除条件）；别名是同一类型，不是包装。

## 验证

```bash
go test ./pkg/egressapi/ ./pkg/egress/ -count=1
go list -f '{{join .Imports "\n"}}' ./pkg/egressapi   # 只许 stdlib
```
