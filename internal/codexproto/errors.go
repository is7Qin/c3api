// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package codexproto

// ErrorKind codex 额度查询失败的分类。**纯值**，不携带任何 HTTP/UI 语义：
// 把 Kind 映射成展示词（`auth_expired` / `upstream_unavailable`）是消费方
// （handler）的事，本包不认识那些词。
type ErrorKind int

const (
	// ErrKindAuthExpired 凭据失效（上游明确宣告凭证死亡）。
	ErrKindAuthExpired ErrorKind = iota + 1
	// ErrKindUpstream 网络 / 5xx 等上游错误（含"上游拒绝但未宣告死亡"）。
	ErrKindUpstream
)

// Error codex 额度查询失败的类型化错误。**实现方**（internal/sdkbridge）产出它，
// **消费方**（internal/handler）用 errors.As 判 Kind——双方因此都不必认识对方的包。
//
// 为什么是类型而不是哨兵（与 internal/pricingresult.FetchError 同 idiom）：
//
//   - 判"一类错误"用 As，判"一个具体原因"用 Is。分类是三档（凭据失效/上游/其它）
//     的**判定**，不是"是不是那一个特定错误"；
//   - 类型能携带底层原因（Err），哨兵不能；
//   - 消费方因此不必认识实现方的任何变量名——实现方换哨兵、加包装层都不影响分类。
//
// 与旧哨兵的关系：实现方在产出本类型的同时**继续**用 %w 把既有哨兵串进链里
// （sdkbridge.ErrAuthExpired / ErrUpstream），所以"仍按哨兵判定"的老调用方与老
// 测试零改动；errors.As 与 errors.Is 相互独立，两条判定互不依赖。
//
// ⚠ 本类型**不**包装 serviceerr.ErrUpstream，也不该包装：那个哨兵被
// codex_login / payment 等多处共享生产，拿它当谓词会把无关上游故障误标成
// "codex 用量上游错误"。「上游不可用」的 HTTP 状态映射归实现方决定。
type Error struct {
	Kind ErrorKind
	Err  error
}

func (e *Error) Error() string { return "codex usage: " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }
