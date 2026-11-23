// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package credential 是凭据类型的词表与能力表：Type 的取值集合，加上 capsTable
// 声明的执行通道——「一个类型意味着什么、由谁鉴权」只在这里回答一次。
//
// 它**不承载凭据取值**：静态通道的类型（api_key / responses-special / custom）由
// internal/proxy 的 credentialFor 按 UsesStaticKey() 直读
// scheduler.Selection.UpstreamKey；codex / bps 的凭据是复合结构（token 组、
// profile、prepared 句柄），由 sdkbridge 的 prepared 入口承载——单字符串的取凭据
// 契约表达不了句柄。
//
// 正交原则：鉴权头名由请求格式侧决定（openai → Authorization: Bearer、
// anthropic → x-api-key，aiclient 现状已按格式分头），与凭据类型无关。
package credential

import (
	"errors"
	"sort"
)

// Type 账号凭据类型：按生态命名（api_key = 静态 Key + 模板 BaseURL，现状语义）。
type Type string

const (
	// TypeAPIKey 默认类型：静态 Key（调度 Selection 携带的 UpstreamKey）。
	TypeAPIKey Type = "api_key"
	// TypeResponsesSpecial Responses 特殊需求模板（图像 tool 剥离等开关）；
	// 只支持 resp / resp-ws 格式（service 校验），配置挂 template_ext。
	TypeResponsesSpecial Type = "responses-special"
	// TypeCodexOAuth Codex OAuth 账号池模板/账号；配置挂 ext 子表，鉴权
	// 接 SDK ws。
	TypeCodexOAuth Type = "codex-oauth"
	// TypeCodexPAT Codex PAT 账号池模板/账号；配置挂 ext 子表，鉴权接 SDK ws。
	TypeCodexPAT Type = "codex-pat"
	// TypeCustom 高级自定义：静态 Key + 逐格式自定义上游路径（配置挂
	// template_ext.upstream_paths）。支持全部 6 种格式；路径未配置时回落官方
	// 默认端点，因此行为与 api_key 在未配置路径时完全一致。
	TypeCustom Type = "custom"
	// TypeBPS BPS desktop OAuth account. BPS owns its endpoint, profile and
	// OAuth credential through account_ext; it is deliberately not Codex.
	TypeBPS Type = "bps"
)

// Valid 类型是否在能力表内（模板主列 credential_type 全量：api_key 四格式任意；
// 生态类型按通道能力做格式校验）。未知类型（含空串）→ false。
func (t Type) Valid() bool {
	_, ok := capsTable[t]
	return ok
}

// 其余谓词（ValidTemplateExt / ValidAccountExt / IsCodex / IsBPS /
// IsResponsesProvider / UsesStaticKey）形状一律是「先查表存在性，再用 c.ch 判定」，
// 那个 `ok &&` 不能省：表外类型的 ChannelOf 是零值 ChannelStaticKey，若派生直接写
// ChannelOf(t) == ChannelStaticKey，UsesStaticKey() 对非法类型就从 false 变 true——
// service/account.go:45,221 拿它决定 upstream_key 是否必填，且**没有** Valid() 守卫。
// 表外恒 false 是这些谓词对外的既有契约（见 TestPredicatesOnUnknownType）。

// ValidTemplateExt 模板 ext 子表可用类型（api_key 类型模板无 ext 行——主列已
// 表达静态 Key 语义）。它是表里唯一不与 ch 同源的位。
func (t Type) ValidTemplateExt() bool {
	c, ok := capsTable[t]
	return ok && c.validTplExt
}

// ValidAccountExt 账号 ext 子表可用类型——即非静态通道（今天：codex oauth/pat + bps）。
func (t Type) ValidAccountExt() bool {
	c, ok := capsTable[t]
	return ok && c.ch != ChannelStaticKey
}

// IsCodex codex 号池类型（oauth / pat）：鉴权走 SDK；请求格式限定 resp/resp-ws。
func (t Type) IsCodex() bool {
	c, ok := capsTable[t]
	return ok && c.ch == ChannelCodex
}

// IsBPS reports the independent BPS OAuth provider. It intentionally does not
// alias IsCodex: BPS has different identity, profile and transport semantics.
func (t Type) IsBPS() bool {
	c, ok := capsTable[t]
	return ok && c.ch == ChannelBPS
}

// IsResponsesProvider reports providers backed by a Responses-native SDK.
func (t Type) IsResponsesProvider() bool {
	c, ok := capsTable[t]
	return ok && (c.ch == ChannelCodex || c.ch == ChannelBPS)
}

// SupportsContinuation reports whether this type can use previous_response_id for
// **server-side** continuation.
//
// 这是**逐 Type 能力位**，不是通道投影：通道回答「由谁鉴权」，能力回答「能不能续接」。
// 今天 codex 通道恰好为 true、其余 false，但那是当前取值，不是不变量——未来的 codex
// 通道类型可以不支持续接（换一种鉴权方式、上游读不回 response id）。调度层的计划判定
// 读的正是这一位（scheduler 的续接前置判定），任何把它改回 `ChannelOf(t)==ChannelCodex`
// 的「简化」都会把两条轴重新焊死，见 caps.go 的 supportsContinuation 注释。
//
// 表外类型 → false（`ok &&` 守卫，与其余谓词同契约，见 TestPredicatesOnUnknownType）。
// 该取值在调度层是**保守方向**：未知类型被当作"可能不支持续接" ⇒ 计划进入 fail-closed
// 的续接解析分支。
func (t Type) SupportsContinuation() bool {
	c, ok := capsTable[t]
	return ok && c.supportsContinuation
}

// IdentityGroup returns the credential types that share one upstream identity
// namespace with t. Codex OAuth and PAT authenticate the same upstream Codex
// account, so they remain one dedup group (a second row for the other Codex
// credential type is a conflict). BPS is an independent provider with its own
// endpoint, quota and device profile: the same ChatGPT e-mail/account may
// legitimately exist as both a Codex account and a BPS account, so BPS only
// deduplicates against BPS.
// 表外类型（非法）走 default：ChannelOf 的零值是 ChannelStaticKey，所以它们只与
// 自己同组——身份去重不会把非法类型并进任何真实类型。
func (t Type) IdentityGroup() []Type {
	switch ChannelOf(t) {
	case ChannelCodex:
		return []Type{TypeCodexOAuth, TypeCodexPAT}
	case ChannelBPS:
		return []Type{TypeBPS}
	default:
		return []Type{t}
	}
}

// UsesStaticKey reports providers that require the account upstream_key——即静态通道。
func (t Type) UsesStaticKey() bool {
	c, ok := capsTable[t]
	return ok && c.ch == ChannelStaticKey
}

// KnownTypes 返回能力表内**全部**类型的规范序（字典序）副本；每次调用返回新切片，
// 调用方改不动权威表。
//
// 为什么导出这张表：跨包要问的是一句**集合**问题——"每个已知类型都在
// domain.allowedFormats 里有政策行吗"。格式门是逐 Type 的表驱动判定，而
// FormatAllowedFor 对表外类型**放行**（没有政策行 = 没有额外限制）。于是"新类型加了
// caps 行、忘了 allowedFormats 行"会让格式门对该类型**全放行**，而两张表各自的
// golden（caps_test.go 的矩阵、format_policy_test.go 的矩阵）都是绿的——没有任何
// 一处会红。这个缺口的机械兜底只能由外部消费者枚举已知类型来写：
// internal/domain 的 TestEveryKnownTypeHasFormatPolicyRow 就是那个消费者。
func KnownTypes() []Type {
	out := make([]Type, 0, len(capsTable))
	for t := range capsTable {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ErrUnsupported 该凭据类型（或承载它的格式/传输）在调用点不受支持。它是**跨包
// sentinel**，与凭据取值无关，不得当成删除分发层时的连带残留清掉：
//   - internal/sdkbridge/bps.go 用 %w 包它（BPS 不接受的格式、非 BPS 的凭据类型）；
//   - cmd/server/wiring_billing.go 用 errors.Is 判它（BPS 探测的凭据不可用 → 400）。
var ErrUnsupported = errors.New("credential: unsupported credential type")
