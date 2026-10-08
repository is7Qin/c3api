// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import "github.com/is7qin/c3api/internal/domain"

// SupplierAdmission 供给准入门（spec 2026-10-09 §4.6，I1）：报告「带供应商
// 归属的账号」当前是否具备就绪的财务上下文，并返回**本次判定所用**的不可变
// 财务上下文。它是一份**内存中发布的小契约**（账号 → 财务上下文就绪），
// **非每请求查库**。
//
// nil（未装配）= 功能关闭态 ⇒ 全部账号照常调度（G1：关闭态零成本），财务上下文
// 为零值（Ready=false ⇒ 无归属/无收益）。装配后，预留谓词对带归属账号追加本门：
// 未就绪 ⇒ 拒绝，使该账号**暂不入调度**（不把「未知」折叠成资金零值）。
type SupplierAdmission interface {
	// AdmitSupplierAccount 报告账号在 ownerUID 归属下是否可入选调度，放行时
	// 返回**同一视图单次读取**的不可变财务上下文（§4.2：归属随选中固定，收尾
	// 不再回查 owner）。ownerUID == 0（平台自有）仅在视图**不含**该账号旧归属时
	// 放行（视图仍含 ⇒ 发布未换代，拒绝并等待 Reload，§4.6.3 发布屏障）；带归属
	// 账号仅在视图就绪且 owner 命中时放行，并返回 UID/Bp/Rev。
	AdmitSupplierAccount(accountID, ownerUID int64) (domain.SupplierFinance, bool)
}

// SetSupplierAdmission 装配供给准入门（main 在 proxy 构造后调用；nil = 关闭态）。
// 只在预留热路径的原子 Load 上读取，装配期一次写入即可。
func (s *Scheduler) SetSupplierAdmission(a SupplierAdmission) {
	if a == nil {
		s.supplierAdmission.Store(nil)
		return
	}
	s.supplierAdmission.Store(&a)
}

// admitSupplier 预留谓词的供给准入检查：读本账号归属（当前叶的静态事实），
// 带归属时经准入门判定并捕获财务上下文；未装配门（nil）恒放行（零值上下文）。
func (s *Scheduler) admitSupplier(accountID, ownerUID int64) (domain.SupplierFinance, bool) {
	adm := s.supplierAdmission.Load()
	if adm == nil {
		return domain.SupplierFinance{}, true
	}
	return (*adm).AdmitSupplierAccount(accountID, ownerUID)
}
