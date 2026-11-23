// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// EffectiveBaseURL 返回一次派发的生效上游 baseURL。它是**唯一**的优先级定义：
// scheduler 的计划键/叶子派生与路由候选指纹都从这里取，任一处再手写一遍优先级
// 都会让"编译器认为变化了"与"判据认为没变"分叉（见 scheduler/static_key.go 的
// newSnapshotStatic 注释）。
//
// 契约（与收口前三处调用逐字节一致，不得自创分支）：
//
//  1. tpl != nil 且 tpl.CredentialType 是 BPS ⇒ BPSDefaultEndpoint。
//     **账号覆盖被忽略**：BPS 的端点由 provider 拥有（BPSBaseURLAllowed 也只接受
//     空或这个固定值），账号级 base_url 不得把它改道。
//  2. acc.BaseURL != nil 且 *acc.BaseURL != "" ⇒ 账号值。
//     这一条**含 tpl == nil**：无模板的账号（scheduler 快照允许零键形状）仍可
//     带账号级覆盖，收口前就是这条语义，收口不许丢。
//  3. tpl != nil ⇒ tpl.BaseURL（模板值）。
//  4. 否则 ""。
//
// acc 必须非 nil（两处调用点都传 &acc / 账号对象本身）；tpl 可为 nil。
func EffectiveBaseURL(tpl *Template, acc *Account) string {
	if tpl != nil && tpl.CredentialType.IsBPS() {
		return BPSDefaultEndpoint
	}
	if acc.BaseURL != nil && *acc.BaseURL != "" {
		return *acc.BaseURL
	}
	if tpl != nil {
		return tpl.BaseURL
	}
	return ""
}
