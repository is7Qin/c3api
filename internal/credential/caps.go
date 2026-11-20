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
type caps struct {
	ch               Channel
	validTplExt      bool // 模板 ext 子表可用
	validAcctExt     bool // 账号 ext 子表可用
	usesAccountExt   bool // 秘密/profile 在 account_ext
	usesStaticKey    bool // 需要 account.upstream_key
	fixedEndpoint    bool // 自带上游端点
	responsesFormats bool // 可服务 openai-responses / -ws 两种格式
}

// capsTable 是唯一的能力真相：类型集合与每个能力位只在这里声明一次，方法只做
// 只读投影。包级只读表 ⇒ 热路径纯查表，无构造、无新增分配。
//
// 新增类型必须补一行并同步 caps_test.go 的 golden（golden 写死了全矩阵，漏改即红）。
var capsTable = map[Type]caps{
	TypeAPIKey: {
		ch:            ChannelStaticKey,
		usesStaticKey: true,
	},
	TypeResponsesSpecial: {
		ch:               ChannelStaticKey,
		validTplExt:      true,
		usesStaticKey:    true,
		responsesFormats: true,
	},
	TypeCustom: {
		ch:               ChannelStaticKey,
		validTplExt:      true,
		usesStaticKey:    true,
		responsesFormats: true,
	},
	TypeCodexOAuth: {
		ch:               ChannelCodex,
		validTplExt:      true,
		validAcctExt:     true,
		usesAccountExt:   true,
		fixedEndpoint:    true,
		responsesFormats: true,
	},
	TypeCodexPAT: {
		ch:               ChannelCodex,
		validTplExt:      true,
		validAcctExt:     true,
		usesAccountExt:   true,
		fixedEndpoint:    true,
		responsesFormats: true,
	},
	TypeBPS: {
		ch:               ChannelBPS,
		validTplExt:      true,
		validAcctExt:     true,
		usesAccountExt:   true,
		fixedEndpoint:    true,
		responsesFormats: true,
	},
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
