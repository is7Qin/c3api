// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package egress

import "github.com/is7qin/c3api/pkg/egressapi"

// 迁移期兼容层（P5-3-A）：出口的值类型已迁到 pkg/egressapi。**这是兼容层，不是
// 终态**——别名保留到 pkg/egress 的实现不再被当作类型使用为止（即 B 单把调度器的
// `Config.Egress *egress.Pool` 换成端口、其余调用方改指 egressapi 之后）。
//
// 为什么只能这样收敛：Go 的类型别名**带不了方法**，所以凡搬走的类型，它的全部方法
// 必须和类型本体一起搬（`Spec.Key/Addr/String`、`Client.HTTP/NoCompression`），
// 连带 `Client.clients` 槽位表与 `Flavour` 闭集也得同去（后者的常量下标只有槽位表
// 的持有者知道怎么解释）。剩下的问题只是"调用方怎么办"：`internal/domain`、
// `internal/proxy`、`internal/sdkbridge`、`internal/service`、`cmd/server` 与各自的
// 用例散着 60+ 处引用，而本单的边界是"搬类型 + 让调度器改指中立包"，不给它们制造
// 顺手重构的机会。故这里保留同名别名。
//
// 别名是**同一类型**，不是新定义、也不是包装：`egress.Spec` 与 `egressapi.Spec` 在
// 反射与类型断言下不可区分，`egress.New(...)` 造出的值可以直接喂给吃
// `egressapi.Spec` 的函数（反之亦然），`*egress.Client` 与 `*egressapi.Client` 是同
// 一个指针类型——所以既有调用点几行都不用改。
//
// 本文件**只许转发**：每个 type / const 都不能有自己的逻辑，func 只有一行（编译器
// 内联）。任何"顺手在这儿补一个方法/字段/常量"的改动都会让定义分叉，而分叉正是
// 这一层存在时要防止的事。同理，`Pool` / `NewPool` / `NewClient` / `CloseClients`
// **不在这里**——它们是实现，不是值类型，归属本包的 pool.go。
//
// 移除条件（可执行；条件未满足前**禁止删除**）：全仓（除 pkg/egress 自身）
// 对这六个名字的**真实类型引用为零**。"真实引用"的口径是：
//
//   - 只算代码：注释（`//` 与 `/* */`）、字符串字面量与 raw string 里出现的拼写**不算**——
//     注释里写 `egress.Client` 是合法的叙述；
//   - 只算 `egress.` 这个限定符：`egressapi.` / `*egress.Pool` / 字段名 `Egress` 均不算；
//   - 算在上面六个名字上；`New` 与实现符号（`Pool` / `NewPool` / `NewClient` /
//     `CloseClients`）生命周期另有归属，不在本条件内。
//
// **当前实测（基 ec731934）：真实引用 54 处**——`Client` 45 / `Spec` 8 / `ResourceKey` 1；
// 分布在：internal/sdkbridge 44（其中 20 在用例）、internal/proxy 4（**全在用例**）、
// internal/domain 3、internal/service 2、internal/api 1。主要集中在 sdkbridge：
// codex.go / call.go / bps.go 直接拿 `egress.Client` 当适配器类型用。
// 距离零还差这 54 处（占绝大多数），条件**远未满足**。
//
// 为什么把口径写死在“真实引用”而不是直接用 `rg` 的原始计数：原来那条命令把
// 注释也算进去（原始 70 命中 vs 真实 54），两个方向都会误导：一方面"命中更多所
// 以更不行"是错的推理；另一方面注释里写 `egress.Client` 是合法文档行为，
// 按原口径"零命中"永远不可能达到——那条件会永久把本文件锁死。
//
// 执行口径参考（计数时排除注释行与字符串；`rg` 本身做不到“排除字符串”，
// 故需要一次 AST/扁平化处理，而不是一场 `rg | wc -l`）：
//
//	rg -n 'egress\.(Spec|Client|Flavour|FlavourHTTP|FlavourNoCompression|ResourceKey)\b' --glob '!pkg/egress/**' -g '*.go'
//
// 然后把注释行（以 `//` 开头）与命中位于字符串内的行剔除；剩下为零时删掉本文件的
// type/const/forwarder。
type (
	// Spec 见 pkg/egressapi.Spec（同一类型）。
	Spec = egressapi.Spec
	// Client 见 pkg/egressapi.Client（同一类型）。
	Client = egressapi.Client
	// Flavour 见 pkg/egressapi.Flavour（同一类型）。
	Flavour = egressapi.Flavour
)

const (
	// FlavourHTTP / FlavourNoCompression 是 egressapi 里的**同一常量**（不是新值、
	// 不是新类型）：老调用方写 `egress.FlavourHTTP` 与写 `egressapi.FlavourHTTP` 得到
	// 的是同一个值。
	FlavourHTTP          = egressapi.FlavourHTTP
	FlavourNoCompression = egressapi.FlavourNoCompression

	// numFlavours 是闭集大小在**本包内部**的拼写：Pool 的底座表与派生循环按它开
	// 数组。不导出——外部只认口味常量；它跟着 egressapi.NumFlavours 走，不另立一份值。
	numFlavours = egressapi.NumFlavours
)

// ResourceKey 见 pkg/egressapi.ResourceKey（一行转发；内联后与直接调用同价）。
func ResourceKey(c *Client) [16]byte { return egressapi.ResourceKey(c) }
