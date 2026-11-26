// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pricingresult

// FetchError 价格源拉取失败。实现方（internal/pricing）在 fetch 失败时返回它
// （Err 里放底层原因）；消费方用 errors.As 判定，**不依赖任何哨兵变量名**。
//
// 为什么是类型而不是哨兵：
//   - 类型能携带上下文（底层原因），哨兵不能；
//   - 消费方（handler）因此不必认识实现方（pricing）的任何变量；
//   - 与 net.Error / os.PathError 同 idiom：判「一类错误」用 As，判「一个具体原因」用 Is。
//
// 为什么它不包装 serviceerr.ErrUpstream：
//
//	本包是**结果值**，不该知道 HTTP 状态码映射那套东西。而且 ErrUpstream 是
//	被 codex_login / payment 等多处共享生产的通用哨兵——拿它当谓词会让任何
//	一环的上游故障都被误标成「价格同步失败」。所以「拉取失败是上游不可用」
//	这条语义归**实现方**（pricing 知道自己在打网络）：由 pricing 决定要不要
//	再包一层 ErrUpstream 供 httpface 的通用映射使用。
//
// ⚠ 承重边界（写下来防止下一代重演）：handler 的 502 分支只认本类型，
// 与 pricing 是否额外包 ErrUpstream **无关**（As 与 Is 相互独立）。别把
// 「不包装 ErrUpstream 就红」当门禁——它不是承重的，为它造门禁是假严谨。
type FetchError struct{ Err error }

func (e *FetchError) Error() string { return "price fetch failed: " + e.Err.Error() }
func (e *FetchError) Unwrap() error { return e.Err }
