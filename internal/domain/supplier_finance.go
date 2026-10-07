// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// SupplierFinance 是「选中账号时捕获」的请求级不可变财务上下文
// （spec 2026-10-09 §4.2/§4.6）。归属随选中固定：收尾 routeLog 只按本上下文
// 落账，**不再回查 owner**——账号转属/删除在途期间不改动已选请求的归属。
//
//   - Ready=true：账号带供应商归属且财务快照已就绪（owner 命中）——UID/Bp/Rev
//     为该请求的有效归属与分成率。
//   - Ready=false：平台自有账号（无归属）或财务快照未就绪。**二者不可区分**
//     ⇒ 供给准入（§4.6）必须保证「带归属但未就绪」的账号不入调度；热路径
//     把 Ready=false 编码为「无归属/无收益」（credited=true）。
type SupplierFinance struct {
	Ready bool
	UID   int64
	Bp    int
	Rev   int64
}
