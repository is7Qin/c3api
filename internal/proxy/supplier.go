// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// 供应商收益热路径（spec 2026-10-09 §4）：收益额纯整数计算（earnOf），收尾按
// **选中账号时捕获**的财务上下文（scheduler.Selection 携带的 domain.SupplierFinance）
// 盖章 `usage_logs` 三收益列（stampSupplier）——不在此回查归属视图。
//
// 视图/快照（SupplierView/SupplierSnapshot）与供给准入的**实现归属**在
// internal/supplier 包（ViewSink/ViewLoader/SupplierSnapshot）；组合根把同一快照同时
// 接到 loader 与 scheduler。proxy 仅保留收益计算与落账适配，不再拥有快照类型。
// 关闭态（enabled=false）快照未装配，收尾字段保持出生定态 credited=true。

import (
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/supplier"
)

// shareBpFull 分成率满值（10000 = 100%）。
const shareBpFull = 10000

// earnOf 供应商收益精确整数计算（spec §4.5）：
//
//	earn = (cost/10000)*bp + (cost%10000)*bp/10000   // floor
//
// floor 是应付账款的安全方向（永不多付）。**对全部合法 int64 精确、无钳制**
// （cost/10000 ≤ MaxInt64/10000、cost%10000 < 10000，各自 *b（b ≤ 10000）均
// ≤ MaxInt64）——**删除旧 earnMulCap 钳制**（它会在超大 cost 下静默缩小应付）。
// 入口守卫 cost<=0 / bp<=0；bp>=10000 恒等短路（对齐 applyMultiplier 的
// m==10000 形态）。
func earnOf(cost int64, shareBp int) int64 {
	if cost <= 0 || shareBp <= 0 {
		return 0
	}
	if shareBp >= shareBpFull {
		return cost
	}
	b := int64(shareBp)
	return (cost/10000)*b + (cost%10000)*b/shareBpFull
}

// stampSupplier 收尾盖章 `usage_logs` 三收益列（出生定态；§4.2 四象限）。
// 归属/分成只取自**选中账号时捕获的**财务上下文（sel.SupplierFinance）——不在此
// 回查 owner（视图换代/删除后收尾不再 Load/查 owner；转属/删除在途不改归属）。
// 未携带 Ready 财务（关闭态未装配 / 未归属 / sel 为 nil）→ uid 空、earn 0、
// credited true（新行不入收益索引）。
func (p *Proxy) stampSupplier(sel *scheduler.Selection, l *domain.UsageLog) {
	if l == nil {
		return
	}
	if sel != nil {
		if fin := sel.SupplierFinance; fin.Ready {
			l.SupplierUserID = fin.UID
			if l.Cost > 0 && fin.Bp > 0 {
				l.SupplierEarnMillis = earnOf(l.Cost, fin.Bp)
			}
		}
	}
	// 出生定态：只对已正确算出的 earn 分类（零收益/未归属/关闭态 ⇒ true）。
	l.SupplierCredited = l.SupplierEarnMillis <= 0
}

// SetSupplierSnapshot 注入供应商视图快照（main 装配；nil = 未装配/关闭态——
// 热路径字段保持出生定态 credited=true）。归属写入/刷新由 main 的 ticker 触发
// Store（§6.4）。快照类型归属 internal/supplier（ViewSink/准入同处）。
func (p *Proxy) SetSupplierSnapshot(s *supplier.SupplierSnapshot) {
	p.supplier = s
}

// SupplierSnapshotRef 返回已装配的供应商快照（nil = 未装配；ops 面读取）。
func (p *Proxy) SupplierSnapshotRef() *supplier.SupplierSnapshot {
	return p.supplier
}
