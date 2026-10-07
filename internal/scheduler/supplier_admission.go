// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// SupplierAdmission 供给准入门（spec 2026-10-09 §4.6，I1）：报告「带供应商
// 归属的账号」当前是否具备就绪的财务上下文。它是一份**内存中发布的小契约**
// （账号 → 财务上下文就绪），**非每请求查库**。
//
// nil（未装配）= 功能关闭态 ⇒ 全部账号照常调度（G1：关闭态零成本）。装配后，
// 预留谓词对带归属账号追加本门：未就绪 ⇒ 拒绝，使该账号**暂不入调度**
// （平台自有账号 ownerUID == 0 恒放行，不受影响）。这样「未知」不再接流量，
// 也就不会产生被丢弃的收益（不把「未知」折叠成资金零值）。
type SupplierAdmission interface {
	// AdmitSupplierAccount 报告账号在 ownerUID 归属下是否可入选调度。
	// ownerUID == 0（平台自有）恒 true；带归属账号在财务快照未就绪或该归属
	// 未落在快照中时返回 false。
	AdmitSupplierAccount(accountID, ownerUID int64) bool
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
// 带归属时经准入门判定；未装配门（nil）恒放行。
func (s *Scheduler) admitSupplier(accountID, ownerUID int64) bool {
	adm := s.supplierAdmission.Load()
	if adm == nil {
		return true
	}
	return (*adm).AdmitSupplierAccount(accountID, ownerUID)
}
