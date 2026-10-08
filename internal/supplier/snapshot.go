// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

// snapshot.go 供应商归属/分成率视图与其单原子指针快照（spec 2026-10-09 §4.3/§4.4/§4.6）。
// 归属本包：视图发布（ViewSink）、装载（ViewLoader）与准入（scheduler.SupplierAdmission）
// 的实现在此同处；组合根把本快照同时接到 loader 的 sink 与 scheduler 的 admission。
// 热路径（账务收尾）只经 proxy 的落账适配读取**已捕获**的财务上下文，不再回查本视图。

import (
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// SupplierView 供应商归属/分成率的**不可变值对象**（一次 Store 换代；
// 热路径只 Load 一次取一致视图——两个独立 atomic.Pointer 不构成原子换代）。
type SupplierView struct {
	owner    map[int64]int64 // account_id → supplier_user_id
	share    map[int64]int   // supplier_user_id → 生效 share_bp（已注入默认）
	revision int64
}

// NewSupplierView 构造视图。传入的 share 已由装载器（repository.LoadSupplierView）
// 对「无行 / share_bp IS NULL」显式注入默认分成率（§2.3）；本构造不依赖 map 零值
// ——Go map 零值 0 会把「无行」误判为「显式 0」⇒ 首个供应商永不产生收益 ⇒
// 永不触发自动建行。
func NewSupplierView(owner map[int64]int64, share map[int64]int, revision int64) *SupplierView {
	if owner == nil {
		owner = map[int64]int64{}
	}
	if share == nil {
		share = map[int64]int{}
	}
	return &SupplierView{owner: owner, share: share, revision: revision}
}

// Owner 返回账号归属供应商 uid（无归属 = false）。
func (v *SupplierView) Owner(accountID int64) (int64, bool) {
	if v == nil {
		return 0, false
	}
	uid, ok := v.owner[accountID]
	return uid, ok
}

// ShareBp 返回供应商生效分成率（未装载 = 0；装配时必须已注入默认）。
func (v *SupplierView) ShareBp(uid int64) int {
	if v == nil {
		return 0
	}
	return v.share[uid]
}

// Revision 视图代数。
func (v *SupplierView) Revision() int64 {
	if v == nil {
		return 0
	}
	return v.revision
}

// SupplierSnapshot 单原子指针快照（热路径单次 Load）+ 三态可观测（§4.4）；实现
// ViewSink（装载器换入）与 scheduler.SupplierAdmission（准入判定）。
type SupplierSnapshot struct {
	view               atomic.Pointer[SupplierView]
	loaded             atomic.Bool
	lastSuccessUnixMs  atomic.Int64 // 0 = 从未成功
	genSeq             atomic.Int64
	staleWarnThreshold time.Duration
	// capacityBlocked 数据盘容量不足（retention §3.11 处置）：置位后带归属账号
	// 准入拒绝（停供应商新流量），平台自有账号不受影响；恢复后复位。
	capacityBlocked atomic.Bool
}

// NewSupplierSnapshot 构造（staleWarnThreshold<=0 ⇒ 兜底 1m）。
func NewSupplierSnapshot(staleWarnThreshold time.Duration) *SupplierSnapshot {
	if staleWarnThreshold <= 0 {
		staleWarnThreshold = time.Minute
	}
	return &SupplierSnapshot{staleWarnThreshold: staleWarnThreshold}
}

// Store 装载并换代（**全部成功才调用一次**；失败保留旧视图 fail-safe）。
func (s *SupplierSnapshot) Store(owner map[int64]int64, share map[int64]int, now time.Time) {
	rev := s.genSeq.Add(1)
	s.view.Store(NewSupplierView(owner, share, rev))
	s.loaded.Store(true)
	s.lastSuccessUnixMs.Store(now.UnixMilli())
}

// Load 单次原子读（热路径唯一入口）。
func (s *SupplierSnapshot) Load() *SupplierView {
	if s == nil {
		return nil
	}
	return s.view.Load()
}

// SupplierSnapshotObs 视图可观测（ops 面；§4.4 三态：loaded / last_success /
// stale_age）。
type SupplierSnapshotObs struct {
	Loaded            bool
	Revision          int64
	LastSuccessUnixMs int64 // 0 = 从未成功
	StaleAgeMs        int64 // 距上次成功装载；从未成功 = -1
}

// Obs 返回快照可观测状态（now 注入便于测试）。
func (s *SupplierSnapshot) Obs(now time.Time) SupplierSnapshotObs {
	last := s.lastSuccessUnixMs.Load()
	obs := SupplierSnapshotObs{
		Loaded:            s.loaded.Load(),
		LastSuccessUnixMs: last,
		StaleAgeMs:        -1,
	}
	if v := s.view.Load(); v != nil {
		obs.Revision = v.Revision()
	}
	if last > 0 {
		obs.StaleAgeMs = now.UnixMilli() - last
	}
	return obs
}

// Stale 报告快照是否陈旧（从未成功 = 陈旧）。
func (s *SupplierSnapshot) Stale(now time.Time) bool {
	obs := s.Obs(now)
	if obs.LastSuccessUnixMs == 0 {
		return true
	}
	return time.Duration(obs.StaleAgeMs)*time.Millisecond > s.staleWarnThreshold
}

// SetCapacityBlocked 置/复位容量不足标记（retention §3.11 处置回调；组合根装配）。
// 置位 ⇒ AdmitSupplierAccount 对带归属账号拒绝（停供应商新流量），平台自有不受影响。
func (s *SupplierSnapshot) SetCapacityBlocked(blocked bool) {
	if s == nil {
		return
	}
	s.capacityBlocked.Store(blocked)
}

// CapacityBlocked 返回当前容量不足标记（ops/测试读取）。
func (s *SupplierSnapshot) CapacityBlocked() bool {
	if s == nil {
		return false
	}
	return s.capacityBlocked.Load()
}

// AdmitSupplierAccount 供给准入门实现（scheduler.SupplierAdmission，§4.6）：
// **单次 Load** 取一致视图，并返回本次判定所用的财务上下文（与准入同一视图，
// 消除「准入读 + 收尾再读」双读）。规则：
//   - 视图 nil（NotReady）⇒ 平台自有（ownerUID==0）放行（零值上下文）；带归属拒绝；
//   - 视图就绪：ownerUID==0 且视图**不含**该账号旧归属 ⇒ 放行（零值上下文）；
//     视图仍含旧归属（归属换代未发布）⇒ 拒绝，等 Reload（§4.6.3 发布屏障）；
//     带归属账号仅当视图 owner 命中该 ownerUID ⇒ 放行并返回 UID/Bp/Rev。
func (s *SupplierSnapshot) AdmitSupplierAccount(accountID, ownerUID int64) (domain.SupplierFinance, bool) {
	// 容量不足处置（spec §3.11）：停供应商新流量——带归属账号一律拒绝，平台自有
	// （ownerUID==0）不受影响（平台供给不依赖供应商磁盘预算）。
	if s != nil && ownerUID != 0 && s.capacityBlocked.Load() {
		return domain.SupplierFinance{}, false
	}
	v := s.Load() // nil 安全：未装配/未就绪 ⇒ 空视图
	if ownerUID == 0 {
		if _, ok := v.Owner(accountID); ok {
			// 视图仍把该账号记在旧归属下：发布未换代，暂不按平台自有入选
			// （否则会把平台流量记为旧 uid）。
			return domain.SupplierFinance{}, false
		}
		return domain.SupplierFinance{}, true
	}
	uid, ok := v.Owner(accountID)
	if !ok || uid != ownerUID {
		return domain.SupplierFinance{}, false
	}
	return domain.SupplierFinance{Ready: true, UID: uid, Bp: v.ShareBp(uid), Rev: v.Revision()}, true
}
