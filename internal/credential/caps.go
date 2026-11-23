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

// caps 是「一个凭据类型意味着什么」的全部答案。
//
// 为什么需要这张表：这些位原先散落在 credential.go 的 11 个逐类型 switch 里，
// 加一个类型时漏改任何一处都**静默**落 default——编译器不报错，生产走错分支。
// 收成一张表之后，「漏掉」变成「表里没这一行」：caps_test.go 的 golden 会红。
//
// 除 validTplExt 与 supportsContinuation 外全部能力位由 ch 派生（validAcctExt /
// usesAccountExt / fixedEndpoint = 非静态通道；usesStaticKey = 静态通道）。加列前
// 先回答：它是否独立于 channel？不独立就不要加——多一列就多一个能与 ch 矛盾的状态。
type caps struct {
	ch          Channel
	validTplExt bool // 模板 ext 子表可用（与 channel 无函数关系）
	// supportsContinuation 该类型能否用 previous_response_id 做**服务端**续接。
	//
	// 它是**逐 Type 能力位，不是通道投影**：通道回答「这个类型由谁鉴权」，能力回答
	// 「这个类型能不能续接」。今天两者取值恰好一致（codex 通道 = true，其余 = false），
	// 但那是当前事实，不是不变量——一个未来的 codex 通道类型（换一种上游鉴权、
	// 却读不回 response id）必须能把这一列单独设成 false。调度层的计划判定读的正是
	// 这一列（见 scheduler.PlanContinuationIncompatible），任何把它写成
	// `ChannelOf(t) == ChannelCodex` 的「简化」都会把两条轴重新焊死。
	supportsContinuation bool
}

// capsTable 是唯一的能力真相：类型集合与每个能力位只在这里声明一次，方法只做
// 只读投影。包级只读表 ⇒ 热路径纯查表，无构造、无新增分配。
//
// validTplExt 必须独立成列：api_key / responses-special / custom 三者 ch 同为
// ChannelStaticKey，但只有 api_key 没有模板 ext 子表 ⇒ 它与 ch 无函数关系。
//
// 新增类型必须补一行并同步 caps_test.go 的 golden（golden 写死了全矩阵，漏改即红）。
var capsTable = map[Type]caps{
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
func ChannelOf(t Type) Channel { return capsTable[t].ch }
