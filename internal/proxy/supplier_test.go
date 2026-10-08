// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/supplier"
)

// TestEarnOfExactFloor A2：以 math/big 独立计算 floor(cost*b/10000) 为 oracle，
// 覆盖 cost × bp 全矩阵（5×5），并断言无钳制（MaxInt64 不缩小）。
func TestEarnOfExactFloor(t *testing.T) {
	costs := []int64{0, 1, 10000, 9999, math.MaxInt64}
	bps := []int{0, 1, 1000, 9999, 10000}
	for _, cost := range costs {
		for _, bp := range bps {
			got := earnOf(cost, bp)
			want := bigFloorEarn(cost, bp)
			require.Equal(t, want, got, "earnOf(%d,%d)", cost, bp)
			if cost >= 0 {
				require.LessOrEqual(t, got, cost, "earn <= cost（非负域）")
			}
		}
	}
}

// TestEarnOfNegativeCost A2①：负 cost 真实调用纯函数并断言 == 0。
func TestEarnOfNegativeCost(t *testing.T) {
	require.Equal(t, int64(0), earnOf(-1, 7000))
	require.Equal(t, int64(0), earnOf(math.MinInt64, 10000))
	require.Equal(t, int64(0), earnOf(-10000, 1000))
}

// TestEarnOfZeroBp bp<=0 ⇒ 0。
func TestEarnOfZeroBp(t *testing.T) {
	require.Equal(t, int64(0), earnOf(100000, 0))
	require.Equal(t, int64(0), earnOf(100000, -5))
}

// TestEarnOfFullBp A2③：share_bp=10000 ⇒ earn==cost（非负域，恒等短路，无钳制）。
func TestEarnOfFullBp(t *testing.T) {
	for _, cost := range []int64{1, 9999, 10000, 123456789, math.MaxInt64} {
		require.Equal(t, cost, earnOf(cost, shareBpFull))
	}
}

// bigFloorEarn oracle：floor(cost*bp/10000)（cost<=0/bp<=0 ⇒ 0）。
func bigFloorEarn(cost int64, bp int) int64 {
	if cost <= 0 || bp <= 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(cost), big.NewInt(int64(bp)))
	n.Div(n, big.NewInt(10000))
	return n.Int64()
}

// TestSupplierViewOwnerShare 视图归属/分成率读取 + 单指针换代。
func TestSupplierViewOwnerShare(t *testing.T) {
	snap := supplier.NewSupplierSnapshot(time.Minute)
	snap.Store(map[int64]int64{1: 100, 2: 200}, map[int64]int{100: 7000, 200: 0}, time.Unix(1000, 0))
	v := snap.Load()
	require.NotNil(t, v)
	uid, ok := v.Owner(1)
	require.True(t, ok)
	require.Equal(t, int64(100), uid)
	require.Equal(t, 7000, v.ShareBp(100))
	// 显式 0 与「无行」区分：uid 200 显式 0。
	require.Equal(t, 0, v.ShareBp(200))
	// 未装载 uid 也返回 0（装配时须已注入默认，此处仅验读取）。
	_, ok = v.Owner(999)
	require.False(t, ok)
	// 视图 nil（未 Store）安全。
	require.Nil(t, (*supplier.SupplierSnapshot)(nil).Load())
}

// TestSupplierSnapshotObs 三态可观测。
func TestSupplierSnapshotObs(t *testing.T) {
	snap := supplier.NewSupplierSnapshot(time.Minute)
	now := time.Unix(2000, 0)
	obs := snap.Obs(now)
	require.False(t, obs.Loaded)
	require.Equal(t, int64(0), obs.LastSuccessUnixMs)
	require.Equal(t, int64(-1), obs.StaleAgeMs)
	require.True(t, snap.Stale(now), "从未成功 = 陈旧")

	snap.Store(nil, nil, now)
	obs = snap.Obs(now)
	require.True(t, obs.Loaded)
	require.Equal(t, now.UnixMilli(), obs.LastSuccessUnixMs)
	require.Equal(t, int64(0), obs.StaleAgeMs)
	require.False(t, snap.Stale(now))

	obs = snap.Obs(now.Add(2 * time.Minute))
	require.Equal(t, int64(120000), obs.StaleAgeMs)
	require.True(t, snap.Stale(now.Add(2*time.Minute)))
}

// TestStampSupplierQuadrants A1③/A3 出生定态四象限（关闭态 / 无归属 / 有归属
// 正收益 / 有归属零收益）。归属随选中固定：盖章只读 sel.SupplierFinance。
func TestStampSupplierQuadrants(t *testing.T) {
	// 关闭态（未装配快照）：uid 0、earn 0、credited true。
	off := &Proxy{}
	l := &domain.UsageLog{AccountID: 7, Cost: 100000}
	off.stampSupplier(nil, l)
	require.Equal(t, int64(0), l.SupplierUserID)
	require.Equal(t, int64(0), l.SupplierEarnMillis)
	require.True(t, l.SupplierCredited, "关闭态 credited=true（新行不入索引）")

	// 有归属（uid=100，bp=7000）正收益：earn=floor(100000*7000/10000)=70000。
	p := &Proxy{}
	sel := &scheduler.Selection{AccountID: 7, SupplierFinance: domain.SupplierFinance{Ready: true, UID: 100, Bp: 7000, Rev: 1}}
	l = &domain.UsageLog{AccountID: 7, Cost: 100000}
	p.stampSupplier(sel, l)
	require.Equal(t, int64(100), l.SupplierUserID)
	require.Equal(t, int64(70000), l.SupplierEarnMillis)
	require.False(t, l.SupplierCredited)

	// 有归属零收益（cost=0）：uid=100、earn 0、credited=true。
	l = &domain.UsageLog{AccountID: 7, Cost: 0}
	p.stampSupplier(sel, l)
	require.Equal(t, int64(100), l.SupplierUserID)
	require.Equal(t, int64(0), l.SupplierEarnMillis)
	require.True(t, l.SupplierCredited)

	// 无归属（平台自有号）：Ready=false ⇒ uid 0、earn 0、credited true。
	ownless := &scheduler.Selection{AccountID: 8}
	l = &domain.UsageLog{AccountID: 8, Cost: 100000}
	p.stampSupplier(ownless, l)
	require.Equal(t, int64(0), l.SupplierUserID)
	require.True(t, l.SupplierCredited)

	// bp=0 显式：有归属但零收益。
	zeroBp := &scheduler.Selection{AccountID: 7, SupplierFinance: domain.SupplierFinance{Ready: true, UID: 100, Bp: 0, Rev: 1}}
	l = &domain.UsageLog{AccountID: 7, Cost: 100000}
	p.stampSupplier(zeroBp, l)
	require.Equal(t, int64(100), l.SupplierUserID)
	require.Equal(t, int64(0), l.SupplierEarnMillis)
	require.True(t, l.SupplierCredited)
}

// TestAdmitSupplierAccount 供给准入门（§4.6）：平台自有在视图**不含**旧归属时
// 放行；带归属仅当视图就绪且归属一致时放行，并返回同一视图的财务上下文。
func TestAdmitSupplierAccount(t *testing.T) {
	// 未装配快照（nil）：平台自有放行（零值上下文）、带归属拒绝。
	var nilSnap *supplier.SupplierSnapshot
	fin, ok := nilSnap.AdmitSupplierAccount(7, 0)
	require.True(t, ok)
	require.False(t, fin.Ready)
	_, ok = nilSnap.AdmitSupplierAccount(7, 100)
	require.False(t, ok)

	snap := supplier.NewSupplierSnapshot(time.Minute)
	// 未装载视图 ⇒ 带归属拒绝、平台自有放行。
	_, ok = snap.AdmitSupplierAccount(7, 100)
	require.False(t, ok)
	_, ok = snap.AdmitSupplierAccount(7, 0)
	require.True(t, ok)

	snap.Store(map[int64]int64{7: 100}, map[int64]int{100: 7000}, time.Unix(0, 0))
	fin, ok = snap.AdmitSupplierAccount(7, 100)
	require.True(t, ok, "归属一致 + 就绪 ⇒ 放行")
	require.True(t, fin.Ready)
	require.Equal(t, int64(100), fin.UID)
	require.Equal(t, 7000, fin.Bp)
	_, ok = snap.AdmitSupplierAccount(7, 200)
	require.False(t, ok, "归属不一致 ⇒ 拒绝")
	_, ok = snap.AdmitSupplierAccount(9, 100)
	require.False(t, ok, "该账号未落在快照 ⇒ 拒绝")
	// 视图仍含旧归属 100 时按平台自有（ownerUID==0）必须拒绝（发布屏障 §4.6.3：
	// 归属换代未发布前不得把平台流量记为旧 uid）。
	_, ok = snap.AdmitSupplierAccount(7, 0)
	require.False(t, ok, "视图仍含旧归属 ⇒ 平台自有不得抢跑")

	// 换代后旧归属消失 ⇒ 平台自有放行。
	snap.Store(map[int64]int64{}, map[int64]int{}, time.Unix(1, 0))
	_, ok = snap.AdmitSupplierAccount(7, 0)
	require.True(t, ok)
}

// TestAdmitSupplierAccountCapacityBlocked spec §3.11 处置：容量不足置位后，带归属
// 账号一律拒绝（停供应商新流量），平台自有账号不受影响；恢复后复位。
func TestAdmitSupplierAccountCapacityBlocked(t *testing.T) {
	snap := supplier.NewSupplierSnapshot(time.Minute)
	snap.Store(map[int64]int64{7: 100}, map[int64]int{100: 7000}, time.Unix(0, 0))

	// 置位：带归属拒绝，平台自有放行。
	require.False(t, snap.CapacityBlocked())
	snap.SetCapacityBlocked(true)
	require.True(t, snap.CapacityBlocked())
	_, ok := snap.AdmitSupplierAccount(7, 100)
	require.False(t, ok, "容量不足 ⇒ 停供应商新流量（带归属拒绝）")
	_, ok = snap.AdmitSupplierAccount(9, 0)
	require.True(t, ok, "平台自有供给不受供应商磁盘预算影响")

	// 复位：恢复准入。
	snap.SetCapacityBlocked(false)
	_, ok = snap.AdmitSupplierAccount(7, 100)
	require.True(t, ok)

	// nil 安全。
	var nilSnap *supplier.SupplierSnapshot
	nilSnap.SetCapacityBlocked(true)
	require.False(t, nilSnap.CapacityBlocked())
}
