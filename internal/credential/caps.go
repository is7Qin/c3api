// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package credential

// Channel 凭据的执行通道：回答「这个类型由谁鉴权」。
//
// 只有一个枚举（不设私有 channel + 导出 Channel 两套，避免来回转换）。
type Channel uint8

const (
	// ChannelStaticKey 静态 upstream_key 直取。**同时是零值**：表外（非法）类型
	// 也落在这里，但那不是 fallback——proxy 的 credentialFor 会先按 Valid() 拒绝
	// 并显式报错。
	ChannelStaticKey Channel = iota
	// ChannelCodex sdkbridge 的 Codex 入口（oauth / pat）。
	ChannelCodex
	// ChannelBPS sdkbridge 的 BPS 入口。
	ChannelBPS
)

// 能力表拆成**两张**，按「这一列的事实属于哪条轴」切分：
//
//	channelCapsTable  键 = Channel ⇒ 通道级事实，「与通道矛盾的状态」在类型上不可表达
//	typeCapsTable     键 = Type    ⇒ 类型级事实（类型→通道的身份映射 + 与通道无函数关系的列）
//
// 为什么拆：这几位原先散落在 credential.go 的 11 个逐类型 switch 里，加一个类型时漏改
// 任何一处都**静默**落 default——编译器不报错，生产走错分支。收成一张表之后，「漏掉」
// 变成「表里没这一行」：caps_test.go 的 golden 会红。
//
// 为什么还要拆成两张：一张 `map[Type]caps` 允许每个类型各自声明一个「账号 ext 是否可用」
// 位，于是「通道是 static、却声称账号 ext 可用」这种自相矛盾的行在类型上是**可写**的，
// 只能靠逐行肉眼核对。把这一列搬到 `map[Channel]…` 之后，同一通道的全部类型共用一格，
// 矛盾状态不可表达；剩下的两列（validTplExt / supportsContinuation）与通道无函数关系，
// 才是真正需要逐类型声明的东西。
//
// 加列前先回答：它是否由通道唯一决定？是 ⇒ 进 channelCapsTable；否 ⇒ 进 typeCapsTable。

// channelCaps 是**通道级**事实：键就是 Channel，每个通道恰好一格。
//
// 今天只有一列；它的存在本身就是「通道决定的位不许逐类型声明」这条规则的载体。
type channelCaps struct {
	// validAcctExt 该通道的账号 ext 子表可用——即非静态通道（今天：codex + bps）。
	// 它是通道属性：同一通道的所有类型对「账号记录能不能挂 ext」不可能有不同答案。
	validAcctExt bool
}

// channelCapsTable 是通道级事实的唯一真相。**每个合法 Channel 都必须有一行**：
// caps_test.go 的 TestChannelCapsTableCoversEveryChannel 断言键集合恰好是三个
// Channel，并额外断言 typeCapsTable 里出现的每个 ch 在这里都有行——漏一行的后果是
// ValidAccountExt 静默恒 false（零值），而不是报错。
var channelCapsTable = map[Channel]channelCaps{
	ChannelStaticKey: {validAcctExt: false},
	ChannelCodex:     {validAcctExt: true},
	ChannelBPS:       {validAcctExt: true},
}

// typeCaps 是**类型级**事实：与通道无函数关系的列 + 类型→通道的身份映射。
type typeCaps struct {
	// ch 该类型走的执行通道。它在这里不是「通道派生的能力位」，而是**身份**：
	// 每个类型恰有一个通道，ChannelOf 是它的只读投影。通道自身的能力位在
	// channelCapsTable 里，按 ch 查表，不在这一行重复声明。
	ch Channel

	// validTplExt 模板 ext 子表可用（与 channel 无函数关系）。
	//
	// 必须独立成列：api_key / responses-special / custom 三者 ch 同为
	// ChannelStaticKey，但只有 api_key 没有模板 ext 子表。
	validTplExt bool

	// supportsContinuation 该类型能否用 previous_response_id 做**服务端**续接。
	//
	// 它是**逐 Type 能力位，不是通道投影**：通道回答「这个类型由谁鉴权」，能力回答
	// 「这个类型能不能续接」。今天两者取值恰好一致（codex 通道 = true，其余 = false），
	// 但那是当前事实，不是不变量——一个未来的 codex 通道类型（换一种上游鉴权、
	// 却读不回 response id）必须能把这一列单独设成 false。调度层的计划判定读的正是
	// 这一列（见 scheduler.PlanContinuationIncompatible），任何把它写成
	// `ChannelOf(t) == ChannelCodex` 的「简化」都会把两条轴重新焊死。
	//
	// 迁到拆分后的表里时它**留在 typeCapsTable**：deliberate——放进 channelCapsTable
	// 就等于承认「能力 = 通道」，而 TestContinuationCapabilityIsNotChannel 用一个
	// codex 通道 + 不支持续接的替身把这条路堵死。
	supportsContinuation bool
}

// typeCapsTable 是类型级事实的唯一真相：类型集合与这两列只在这里声明一次，方法只做
// 只读投影。包级只读表 ⇒ 热路径纯查表，无构造、无新增分配。
//
// **unknown 语义**：表外类型（含空串）在两张表里都查不到 ⇒ `ok` 为 false ⇒ 全部布尔
// 谓词恒 false（含 UsesStaticKey：ChannelOf 的零值正是 ChannelStaticKey，少了那个
// `ok &&` 守卫，非法类型就会被当成「静态 Key」放行）。拆表不得放松这一点，见
// TestPredicatesOnUnknownType。
//
// 新增类型必须补一行并同步 caps_test.go 的 golden（golden 写死了全矩阵，漏改即红）；
// 新增 Channel 必须补一行 channelCapsTable（覆盖率由结构测试钉住）。
var typeCapsTable = map[Type]typeCaps{
	TypeAPIKey:           {ch: ChannelStaticKey},
	TypeResponsesSpecial: {ch: ChannelStaticKey, validTplExt: true},
	TypeCustom:           {ch: ChannelStaticKey, validTplExt: true},
	TypeCodexOAuth:       {ch: ChannelCodex, validTplExt: true, supportsContinuation: true},
	TypeCodexPAT:         {ch: ChannelCodex, validTplExt: true, supportsContinuation: true},
	TypeBPS:              {ch: ChannelBPS, validTplExt: true},
}

// ChannelOf 返回该类型走的执行通道。
//
// 这是「一个凭据类型怎么分发」的唯一答案：调用方按通道分支，不再按类型谓词分支
// （internal/proxy 里由 AST 门禁禁止直接调 IsCodex/IsBPS/IsResponsesProvider）。
//
// 未知/非法类型 → ChannelStaticKey（零值），与原先 if/else-if/else 的 else 分支严格
// 等价。这不是静默 fallback：credentialFor 对非法类型先按 Valid() 显式报错，不会拿
// 静态 key 路径把它洗白。
func ChannelOf(t Type) Channel { return typeCapsTable[t].ch }
