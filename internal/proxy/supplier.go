// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// 供应商收益热路径（spec 2026-10-09 §4）：归属与分成率来自**单个**
// atomic.Pointer 视图（G2 零 DB），收益额纯整数计算（earnOf）。关闭态
// （enabled=false）视图恒 nil，热路径仅一次 Load + nil 分支。

import (
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
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

// FinanceCtx 请求级不可变财务上下文（选中账号时捕获；收尾按捕获值落账，
// 非收尾重查 owner）。Ready=false 不等于平台自有——该供应商账号不得入选
// （供给准入，§4.6）。
type FinanceCtx struct {
	Ready bool
	UID   int64
	Bp    int
	Rev   int64
}

// SupplierView 供应商归属/分成率的**不可变值对象**（一次 Store 换代；
// 热路径只 Load 一次取一致视图——两个独立 atomic.Pointer 不构成原子换代）。
type SupplierView struct {
	owner    map[int64]int64 // account_id → supplier_user_id
	share    map[int64]int   // supplier_user_id → 生效 share_bp（已注入默认）
	revision int64
}

// NewSupplierView 构造视图。shareBpDefault 用于「无行 / share_bp IS NULL」
// 的显式默认注入（**不得依赖 map 零值**——Go map 零值 0 会把「无行」误判为
// 「显式 0」⇒ 首个供应商永不产生收益 ⇒ 永不触发自动建行，§2.3）。
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

// SupplierSnapshot 单原子指针快照（热路径单次 Load）+ 三态可观测（§4.4）。
type SupplierSnapshot struct {
	view               atomic.Pointer[SupplierView]
	loaded             atomic.Bool
	lastSuccessUnixMs  atomic.Int64 // 0 = 从未成功
	revisionCounter    atomic.Int64
	staleWarnThreshold time.Duration
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
	rev := s.revisionCounter.Add(1)
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

// SetSupplierSnapshot 注入供应商视图快照（main 装配；nil = 未装配/关闭态——
// 热路径字段保持出生定态 credited=true）。归属写入/刷新由 main 的 ticker 触发
// Store（§6.4）。
func (p *Proxy) SetSupplierSnapshot(s *SupplierSnapshot) {
	p.supplier = s
}

// SupplierSnapshotRef 返回已装配的供应商快照（nil = 未装配；ops 面读取）。
func (p *Proxy) SupplierSnapshotRef() *SupplierSnapshot {
	return p.supplier
}

// captureFinance 选中账号的财务上下文（**单次 Load**；G2：零 DB）。
// Ready=false 表示该账号无归属或视图未就绪。
func (p *Proxy) captureFinance(accountID int64) FinanceCtx {
	if p.supplier == nil {
		return FinanceCtx{}
	}
	v := p.supplier.Load()
	if v == nil {
		return FinanceCtx{}
	}
	uid, ok := v.Owner(accountID)
	if !ok {
		return FinanceCtx{}
	}
	return FinanceCtx{Ready: true, UID: uid, Bp: v.ShareBp(uid), Rev: v.Revision()}
}

// stampSupplier 收尾盖章 `usage_logs` 三收益列（出生定态；§4.2 四象限）。
// 归属命中即填 uid；earn 受 cost/bp 守卫；`credited = earn <= 0`（配置无关）。
// 关闭态（p.supplier 未装配）→ uid 空、earn 0、credited true（新行不入收益
// 索引，G1）。
func (p *Proxy) stampSupplier(l *domain.UsageLog) {
	if l == nil {
		return
	}
	if p.supplier != nil {
		if fin := p.captureFinance(l.AccountID); fin.Ready {
			l.SupplierUserID = fin.UID
			if l.Cost > 0 && fin.Bp > 0 {
				l.SupplierEarnMillis = earnOf(l.Cost, fin.Bp)
			}
		}
	}
	// 出生定态：只对已正确算出的 earn 分类（零收益/未归属/关闭态 ⇒ true）。
	l.SupplierCredited = l.SupplierEarnMillis <= 0
}
