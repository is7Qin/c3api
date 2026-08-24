// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package billing 计费核心：service_tier 归一化 + 价格矩阵纯函数 + 余额快照
// + 批量扣费 flusher（T2/T3）。扣费落库与请求路径分离。
package billing

import (
	"context"
	"sync/atomic"

	"github.com/is7qin/c3api/pkg/logx"
)

// AssignmentKey 用户-组关联键（T3.5 修正：用户专属倍率按组挂载——用户在不同
// 组可有不同倍率；EffectiveMultiplier 按 (userID, groupID) 查专属倍率）。
type AssignmentKey struct {
	UserID  int64
	GroupID int64
}

// BalanceLoader 余额 + 倍率快照数据源（repository.Repository 门面实现：
// LoadBalances 委托 Users，LoadGroupMultipliers / LoadAssignmentMultipliers
// 委托 Groups）。
type BalanceLoader interface {
	// LoadBalances 全量余额（毫分）。
	LoadBalances(ctx context.Context) (map[int64]int64, error)
	// LoadGroupMultipliers 全量组倍率（万分数；组默认 10000 恒在表内）。
	LoadGroupMultipliers(ctx context.Context) (map[int64]int, error)
	// LoadAssignmentMultipliers 全量用户-组专属倍率（万分数；仅
	// group_assignments.price_multiplier 非 NULL 行——缺失 = 未设置 → 用组倍率）。
	LoadAssignmentMultipliers(ctx context.Context) (map[AssignmentKey]int, error)
}

// multipliers 倍率快照（T3.5 价格倍率，万分数；并行快照与余额分离——Set 定向
// 刷新只动余额条目，不牵动倍率）。
type multipliers struct {
	assignments map[AssignmentKey]int // 仅设置了专属倍率的用户-组（存在 = 已设置）
	groups      map[int64]int         // 全部组（NOT NULL 默认 10000）
}

// Balances 余额只读快照（毫分；对齐 pricing 快照模式）：atomic.Pointer 换整表，
// 热路径零锁零分配。O1 优化：条目为 *atomic.Int64——Set 命中已存在条目原地
// Store（O(1) 零拷贝，不再整表拷贝换指针）。预检读滞后 ≤ BalanceRefreshInterval
// （多实例条件扣 DB 兜底）。
type Balances struct {
	loader BalanceLoader
	log    *logx.Logger
	snap   atomic.Pointer[map[int64]*atomic.Int64]
	mult   atomic.Pointer[multipliers]
}

// NewBalances 构造余额快照（初始空表——未 Reload 前预检全部 402 拒绝，安全侧；
// 倍率空表 → EffectiveMultiplier 默认 10000 ×1）。
func NewBalances(loader BalanceLoader, log *logx.Logger) *Balances {
	b := &Balances{loader: loader, log: log}
	m := make(map[int64]*atomic.Int64)
	b.snap.Store(&m)
	return b
}

// Reload 全量重载（启动同步 + BalanceRefreshInterval ticker + 管理面改余额/
// 倍率/Redeem 后 invalidate 调用）。失败 fail-safe：Warn + 保留旧快照（不替换，
// 预检继续用旧值，条件扣 DB 兜底）。余额/组倍率/assignment 倍率三路都成功才
// 整体换新——任一路失败三路都保留旧值（快照内自洽）。
//
// O1：全新条目整体原子换（O(n) 只在 Reload——管理面变更频率，非热路径）。
// Set×Reload 换指针竞态 = 良性丢更新：Set 持旧快照条目 Store 而 Reload 已换新
// 指针 → 该次更新不进新快照（DB 值权威，下次 Reload 收敛；快照读本就滞后 ≤
// BalanceRefreshInterval，不为此时序加锁误导）。
func (b *Balances) Reload(ctx context.Context) error {
	m, err := b.loader.LoadBalances(ctx)
	if err != nil {
		if b.log != nil {
			b.log.Warn("balance snapshot reload failed", logx.Error(err))
		}
		return err
	}
	gm, err := b.loader.LoadGroupMultipliers(ctx)
	if err != nil {
		if b.log != nil {
			b.log.Warn("group multiplier snapshot reload failed", logx.Error(err))
		}
		return err
	}
	am, err := b.loader.LoadAssignmentMultipliers(ctx)
	if err != nil {
		if b.log != nil {
			b.log.Warn("assignment multiplier snapshot reload failed", logx.Error(err))
		}
		return err
	}
	snap := make(map[int64]*atomic.Int64, len(m))
	for uid, bal := range m {
		v := &atomic.Int64{}
		v.Store(bal)
		snap[uid] = v
	}
	b.snap.Store(&snap)
	b.mult.Store(&multipliers{assignments: am, groups: gm})
	return nil
}

// ReloadMultipliers 定向刷新倍率快照（O2 接线矩阵：组倍率/assignment 专属倍率
// 变更 ≠ 余额变更，不走全量 Reload——避免 O(n) 用户余额重载）。组倍率小表 +
// assignment 专属倍率小表两路都成功才整体换新（快照内自洽）；失败 fail-safe：
// Warn + 保留旧倍率快照（错误 ≤ BalanceRefreshInterval ticker 兜底收敛）。
func (b *Balances) ReloadMultipliers(ctx context.Context) error {
	gm, err := b.loader.LoadGroupMultipliers(ctx)
	if err != nil {
		if b.log != nil {
			b.log.Warn("group multiplier snapshot reload failed", logx.Error(err))
		}
		return err
	}
	am, err := b.loader.LoadAssignmentMultipliers(ctx)
	if err != nil {
		if b.log != nil {
			b.log.Warn("assignment multiplier snapshot reload failed", logx.Error(err))
		}
		return err
	}
	b.mult.Store(&multipliers{assignments: am, groups: gm})
	return nil
}

// Set 扣费后定向刷新单用户余额（结算语句成功后调用）：已存在条目原地
// Store（O(1) 零拷贝——扣费频率 = flush 节奏）。缺失条目忽略：仅限已存在用户
// 的余额变更（PUT/Redeem/flush 回写，预检时已在快照内恒命中）；新用户创建
// 走全量 Reload 进快照（见 O2 接线矩阵）。
func (b *Balances) Set(uid, bal int64) {
	if m := b.snap.Load(); m != nil {
		if e := (*m)[uid]; e != nil {
			e.Store(bal)
		}
	}
}

// BalanceOf 快照读余额（毫分）：命中返回 (bal, true)（含 0）；缺失 → (0, false)
// （用户无快照 = 预检 402 拒绝，不按 0 放行）。
func (b *Balances) BalanceOf(uid int64) (int64, bool) {
	if m := b.snap.Load(); m != nil {
		if e := (*m)[uid]; e != nil {
			return e.Load(), true
		}
	}
	return 0, false
}

// EffectiveMultiplier 有效价格倍率（万分数，T3.5 修正：按组查序）：该用户在该
// 组的专属倍率（assignment 已设置，非 nil）→ 用户值；否则组倍率；均缺 →
// 10000（×1）。热路径零分配无锁：一次 atomic.Load + ≤2 次 map 查找，与
// BalanceOf 同级。m==10000 的恒等短路由调用方 applyMultiplier 承担（默认路径
// 逐指令等价）。
func (b *Balances) EffectiveMultiplier(userID, groupID int64) int {
	m := b.mult.Load()
	if m == nil {
		return 10000
	}
	if am, ok := m.assignments[AssignmentKey{UserID: userID, GroupID: groupID}]; ok {
		return am
	}
	if gm, ok := m.groups[groupID]; ok {
		return gm
	}
	return 10000
}
