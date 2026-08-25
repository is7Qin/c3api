// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/redisx"
)

// --- 并发门份额+借用测试（spec conc-share-borrow-gate §2 表格 T1-T6；T7=-race 门禁） ---
//
// 测试基座：miniredis + redisx.Open（dogfood 全仓唯一构造点纪律）；等待一律
// require.Eventually 轮询谓词或同步 tick 直调（同包私有方法），零 sleep。
// 视图陈旧边界不靠真实时钟推进——手工构建/老化 clusterView（at 直接置过去），
// 字段陈旧用回填 ts 直写 HASH，全部确定性。

// dynN 可变 N 的 InstancesProvider 测试桩（fakeInstances 是固定值，N 切换场景需可变）。
type dynN struct{ v atomic.Int32 }

func (d *dynN) ClusterInstances() int { return int(d.v.Load()) }

// newTestGateRedis miniredis 基座（discovery_test.go 同款）。
func newTestGateRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	return mr, c
}

// newConcAuth 构造已 Reload 的 Auth（keys 快照就位 → gate 计数器与受限元数据可查）。
func newConcAuth(t *testing.T, keys map[string]domain.KeyMeta) *Auth {
	t.Helper()
	a := NewAuth(noopKeyLoader{keys: keys}, noopUserLoader{}, nil)
	require.NoError(t, a.Reload(context.Background()))
	return a
}

// T1 结构短路：N=1 时 share=limit → 超份额分支数学上不可达，acquire/release/
// quotaExhausted/deductQuota 全路径零 Redis 命令（含视图在场时——判定是纯内存
// 原子读）。worker 未启动（请求路径本就不含 worker；构造亦零副作用）。
func TestConcN1StructuralShortCircuit(t *testing.T) {
	mr, c := newTestGateRedis(t)
	meta := domain.KeyMeta{KeyID: 1, UserID: 1, UserMaxConc: 4, HasQuota: true, Quota: 100}
	a := newConcAuth(t, map[string]domain.KeyMeta{"h": meta})
	g := a.gate
	g.SetInstancesProvider(fakeInstances(1))
	NewConcSyncWorker(a, c, "inst-a", nil) // 构造零副作用（未启动）

	base := mr.CommandCount()
	lvl, ok := g.acquire(meta)
	require.True(t, ok)
	require.Equal(t, 1, lvl)
	for range 3 { // 占到 user 限额 4（份额=limit，全部 fast-path）
		lvl, ok = g.acquire(meta)
		require.True(t, ok)
		require.Equal(t, 1, lvl)
	}
	lvlOver, okOver := g.acquire(meta)
	require.False(t, okOver, "N=1 第 5 笔超限拒绝（与 main 分支同点）")
	require.Zero(t, lvlOver)
	require.Equal(t, int64(4), g.store.Load().users[1].Load(), "拒绝笔计数已回滚")

	// 视图在场（新鲜）：判定走内存原子读，依旧零 Redis、同点拒绝
	g.cluster.Store(&clusterView{
		users: map[int64]concSnap{1: {total: 4, selfLast: 4, at: time.Now()}},
		keys:  map[int64]concSnap{},
	})
	_, okBorrow := g.acquire(meta)
	require.False(t, okBorrow, "N=1 视图路径同样同点拒绝")

	g.release(meta, lvl)
	g.quotaExhausted(meta)
	g.deductQuota(1, 7)
	require.Zero(t, mr.CommandCount()-base, "全路径零 Redis 命令（公理 2 钉死）")
}

// T2a share 公式矩阵：floor / max(1) / N=1 恒等。
func TestShareFormulaMatrix(t *testing.T) {
	cases := []struct {
		limit, n, want int
	}{
		{limit: 9, n: 3, want: 3},  // 整除 floor
		{limit: 10, n: 3, want: 3}, // 非整除 floor（Σ份额 ≤ limit）
		{limit: 5, n: 1, want: 5},  // N=1 恒等（结构短路前提）
		{limit: 7, n: 7, want: 1},
		{limit: 2, n: 4, want: 1}, // limit<N 退化形态 max(1)
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, concShare(tc.limit, tc.n),
			"share(%d,%d)", tc.limit, tc.n)
	}
}

// T2b N 从 3→1→3 变化即时生效且在途继承：N 只是现读除数，无模式无转换。
// 超份额无视图时 fail-open 按「全额 limit」本地判定放行至真上限；有新鲜视图
// 时才按对账聚合判定。
func TestShareDynamicNInflightInheritance(t *testing.T) {
	a := newConcAuth(t, map[string]domain.KeyMeta{
		"h": {KeyID: 1, UserID: 1, UserMaxConc: 9},
	})
	g := a.gate
	n := &dynN{}
	n.v.Store(3)
	g.SetInstancesProvider(n)
	m := domain.KeyMeta{KeyID: 1, UserID: 1, UserMaxConc: 9}

	for range 3 { // N=3 share=3：占满 fast-path 份额
		lvl, ok := g.acquire(m)
		require.True(t, ok)
		require.Equal(t, 1, lvl)
	}
	// 超份额、无视图 → fail-open 全额本地（4..9 ≤ 9 放行）
	for range 6 {
		lvl, ok := g.acquire(m)
		require.True(t, ok, "fail-open 全额本地判定放行")
		require.Equal(t, 1, lvl)
	}
	_, ok := g.acquire(m)
	require.False(t, ok, "真上限 9 兜底：第 10 笔拒绝")

	// N→1 即时生效：share=limit=9，但真上限兜底不变（在途 9 继承，全拒）
	n.v.Store(1)
	for range 2 {
		_, ok := g.acquire(m)
		require.False(t, ok, "在途继承：满载时 N 切换不放行")
	}
	require.Equal(t, int64(9), g.store.Load().users[1].Load(), "在途计数不受 N 切换影响")

	// 释放 6 笔 → 在途 3；N→3：share=3 恰满，超份额借位经新鲜单实例视图
	// （effective≈L_now）放行至真上限
	for range 6 {
		g.release(m, 1)
	}
	g.cluster.Store(&clusterView{users: map[int64]concSnap{1: {total: 3, selfLast: 3, at: time.Now()}}})
	lvl, ok := g.acquire(m)
	require.True(t, ok, "超份额借位：effective=3−3+4=4 < 9")
	g.release(m, lvl)
}

// T3 视图判定边界：total−selfLast+L_now 公式两侧、无视图/条目缺失/视图陈旧
// fail-open 全额本地。
func TestClusterViewJudgmentBoundaries(t *testing.T) {
	g := newConcurrencyGate(nil)

	// 无视图：fail-open 全额本地（lnow ≤ limit 放行 / > limit 拒绝）
	require.True(t, g.concAllows(false, 1, 6, 6))
	require.False(t, g.concAllows(false, 1, 6, 7))

	stale := time.Now().Add(-concViewStale - time.Second)
	fresh := map[int64]concSnap{1: {total: 7, selfLast: 3, at: time.Now()}}
	aged := map[int64]concSnap{1: {total: 7, selfLast: 3, at: stale}}

	// 新鲜视图：effective = 7−3+L_now
	g.cluster.Store(&clusterView{users: fresh, keys: map[int64]concSnap{}})
	require.True(t, g.concAllows(false, 1, 6, 1), "effective=5 < 6 放行")
	require.True(t, g.concAllows(false, 1, 6, 0), "effective=4 < 6 放行")
	require.False(t, g.concAllows(false, 1, 6, 2), "effective=6 ≥ 6 拒绝")
	// 条目缺失（他对象有视图、本对象无）→ fail-open
	require.True(t, g.concAllows(false, 99, 6, 6), "缺失条目全额本地")
	require.False(t, g.concAllows(false, 99, 6, 7))
	// 视图陈旧 → fail-open（不采信旧聚合）
	g.cluster.Store(&clusterView{users: aged, keys: map[int64]concSnap{}})
	require.True(t, g.concAllows(false, 1, 6, 6), "陈旧视图全额本地（非对账判定）")
	require.False(t, g.concAllows(false, 1, 6, 7))
	// key 层同款
	require.True(t, g.concAllows(true, 1, 4, 4))
	require.False(t, g.concAllows(true, 1, 4, 5))
}

// T3b 对账聚合：陈旧字段剔除（ts 早于 now−4s 不计入）+ selfLast 取本次上报值。
// 同步直调 tick（确定性，无时钟推进依赖）；ghost 字段用回填 ts 直写 HASH。
func TestConcAggregationFreshnessAndSelfLast(t *testing.T) {
	_, c := newTestGateRedis(t)
	meta := domain.KeyMeta{KeyID: 7, UserID: 7, KeyMaxConc: 10}
	a := newConcAuth(t, map[string]domain.KeyMeta{"k": meta})
	g := a.gate
	w := NewConcSyncWorker(a, c, "inst-self", nil)

	// 他实例新鲜字段 + 幽灵陈旧字段（ts 回填 5s 前）直写 HASH
	nowMS := time.Now().UnixMilli()
	require.NoError(t, c.HSet(context.Background(), concKeyPrefix+"7",
		"inst-other", strconv.FormatInt(3, 10)+" "+strconv.FormatInt(nowMS, 10)).Err())
	require.NoError(t, c.HSet(context.Background(), concKeyPrefix+"7",
		"ghost", strconv.FormatInt(9, 10)+" "+strconv.FormatInt(nowMS-int64((concFieldStale+time.Second)/time.Millisecond), 10)).Err())

	// 本地在途 2（受限层级在途 > 0 才上报）→ 本实例字段入聚合
	g.store.Load().keys[7].Store(2)
	w.tick(context.Background())

	cv := g.cluster.Load()
	require.NotNil(t, cv, "tick 后视图换入")
	snap, ok := cv.keys[7]
	require.True(t, ok)
	require.Equal(t, int64(3+2), snap.total, "新鲜字段求和（自身 2 + 他实例 3），幽灵 9 陈旧剔除")
	require.Equal(t, int64(2), snap.selfLast, "selfLast = 本次上报值（精化基准）")

	// 判定精化端到端：N=2 share=5，L_now=3 → effective=3−2+3=4 < 10 放行；
	// 推到真上限边缘 → effective=5−2+10=13 ≥ 10 拒绝
	g.SetInstancesProvider(fakeInstances(2))
	lvl, okAq := g.acquire(domain.KeyMeta{KeyID: 7, UserID: 7, KeyMaxConc: 10})
	require.True(t, okAq)
	require.Equal(t, 3, lvl, "user 排行位恒置 + key 位（UserMaxConc=0 不限流仍计数）")
	g.store.Load().keys[7].Store(9) // 推到真上限边缘
	lvl2, okAq2 := g.acquire(domain.KeyMeta{KeyID: 7, UserID: 7, KeyMaxConc: 10})
	require.False(t, okAq2, "effective=5−2+10=13 ≥ 10 拒绝")
	require.Zero(t, lvl2)
	require.Equal(t, int64(9), g.store.Load().keys[7].Load(), "拒绝笔回滚净零")
}

// T4 对账收敛：fast-path 占用 ≤1 tick 出现在视图；绝对值覆盖写杀漂移；EXPIRE 续期。
func TestConcReconciliationConverges(t *testing.T) {
	mr, c := newTestGateRedis(t)
	meta := domain.KeyMeta{KeyID: 1, UserID: 1, UserMaxConc: 8}
	a := newConcAuth(t, map[string]domain.KeyMeta{"h": meta})
	g := a.gate
	g.SetInstancesProvider(fakeInstances(2))
	w := NewConcSyncWorker(a, c, "inst-a", nil)

	ctx := context.Background()
	_, ok := g.acquire(meta)
	require.True(t, ok)
	w.tick(ctx) // 同步一 tick：占用即出现在视图（≤1 tick 收敛）
	cv := g.cluster.Load()
	require.NotNil(t, cv)
	require.Equal(t, int64(1), cv.users[1].selfLast, "在途 1 上报")
	require.GreaterOrEqual(t, cv.users[1].total, int64(1))

	// 绝对值覆盖写：再占 3 笔 → 下个 tick selfLast=4（非增量累加 1+3）
	for range 3 {
		if _, ok := g.acquire(meta); !ok {
			t.Fatal("acquire 应成功")
		}
	}
	w.tick(ctx)
	cv = g.cluster.Load()
	require.Equal(t, int64(4), cv.users[1].selfLast, "每 tick 绝对值覆盖写（杀漂移）")

	// EXPIRE 续期：键带 TTL；worker 停摆 + FastForward 过期窗 → 键自灭（防遗弃累积）
	ttl, err := c.TTL(ctx, concUserPrefix+"1").Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 15*time.Second, "EXPIRE 16s 已续期")
	raw, err := c.HGet(ctx, concUserPrefix+"1", "inst-a").Result()
	require.NoError(t, err)
	require.Regexp(t, `^4 \d{13}$`, raw, "value 形如 \"<L> <unixmilli>\"")
	mr.FastForward(concKeyTTL + time.Second)
	cnt, err := c.Exists(ctx, concUserPrefix+"1").Result()
	require.NoError(t, err)
	require.Zero(t, cnt, "全体消亡后键 EXPIRE 自灭")
}

// T5 fail-open 结构性质（spec §1.4/验收 §3）：miniredis 关闭 → tick 失败（errs
// 为确定性信号）、视图冻结不换入 → 陈旧后自动退化全额放行（真上限内无 429 风暴）；
// 同端口恢复 → ≤ 数 tick 换入新视图回归共识。
func TestConcFailOpenOnRedisOutageAndRecover(t *testing.T) {
	mr := miniredis.NewMiniRedis()
	require.NoError(t, mr.StartAddr("127.0.0.1:0"))
	t.Cleanup(func() { mr.Close() })
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })

	meta := domain.KeyMeta{KeyID: 1, UserID: 1, UserMaxConc: 6} // N=2 → share 3
	a := newConcAuth(t, map[string]domain.KeyMeta{"h": meta})
	g := a.gate
	g.SetInstancesProvider(fakeInstances(2))
	w := NewConcSyncWorker(a, c, "inst-a", nil)
	require.NoError(t, w.Start(t.Context()))
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	m := meta
	lvl1, ok := g.acquire(m)
	require.True(t, ok)
	lvl2, ok := g.acquire(m)
	require.True(t, ok)
	lvl3, ok := g.acquire(m)
	require.True(t, ok)
	require.Eventually(t, func() bool { // ≤1 tick：份额内占用进入视图
		cv := g.cluster.Load()
		return cv != nil && time.Since(cv.users[1].at) < concViewStale
	}, 3*time.Second, 10*time.Millisecond, "视图建立")

	borrowed, ok := g.acquire(m)
	require.True(t, ok, "超份额借位：effective=3−3+4=4 < 6")

	addr := mr.Addr() // Close 后不可读，先取
	mr.Close()
	// 冻结证明：连续两个失败 tick 之间视图指针不变（确定性信号，无 sleep）
	errs1 := w.errs.Load()
	require.Eventually(t, func() bool { return w.errs.Load() >= errs1+1 },
		8*time.Second, 20*time.Millisecond, "tick 失败可观测（冻结语义进入）")
	vFrozen := g.cluster.Load()
	errs2 := w.errs.Load()
	require.Eventually(t, func() bool { return w.errs.Load() >= errs2+1 },
		8*time.Second, 20*time.Millisecond, "第二个失败 tick")
	require.Same(t, vFrozen, g.cluster.Load(), "故障期视图冻结（不换入新快照）")

	// 手工老化冻结视图（模拟 4s 真实时钟流逝——不 sleep）
	aged := *vFrozen
	agedUsers := make(map[int64]concSnap, len(aged.users))
	for uid, s := range aged.users {
		s.at = time.Now().Add(-concViewStale - time.Second)
		agedUsers[uid] = s
	}
	aged.users = agedUsers
	g.cluster.Store(&aged)

	// 陈旧 → fail-open 全额本地：真上限 6 内继续放行（无 429 风暴），第 7 笔拒绝
	lvl5, ok := g.acquire(m)
	require.True(t, ok, "fail-open 全额放行（5 ≤ 6）")
	lvl6, ok2 := g.acquire(m)
	require.True(t, ok2, "fail-open 全额放行（6 ≤ 6）")
	lvl7, ok3 := g.acquire(m)
	require.False(t, ok3, "真上限兜底：7 > 6 拒绝")
	require.Zero(t, lvl7)
	g.release(m, borrowed)
	g.release(m, lvl5)
	g.release(m, lvl6)
	require.Equal(t, int64(3), g.store.Load().users[1].Load(), "回到持 3 笔基态")

	// 同端口重启 → 连接池自动重连，≤数 tick 换入新视图回归共识
	restarted := miniredis.NewMiniRedis()
	require.NoError(t, restarted.StartAddr(addr))
	t.Cleanup(func() { restarted.Close() })
	require.Eventually(t, func() bool {
		cv := g.cluster.Load()
		return cv != nil && cv != &aged && time.Since(cv.users[1].at) < concViewStale
	}, 8*time.Second, 10*time.Millisecond, "恢复后新视图换入")
	require.Eventually(t, func() bool { // 绝对值覆盖写收敛到当前在途 3
		return g.cluster.Load().users[1].selfLast == 3
	}, 3*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(0), w.errs.Load(), "成功 tick 清错误态")

	// 共识回归实证：他实例报 3 → 聚合 6 → 第 4 笔借位被拒（不再 fail-open 放行）
	require.NoError(t, c.HSet(context.Background(), concUserPrefix+"1",
		"inst-b", "3 "+strconv.FormatInt(time.Now().UnixMilli(), 10)).Err())
	require.Eventually(t, func() bool { return g.cluster.Load().users[1].total == 6 },
		3*time.Second, 10*time.Millisecond, "他实例字段进聚合")
	_, ok = g.acquire(m)
	require.False(t, ok, "effective=6−3+4=7 ≥ 6 借位被拒（共识恢复）")
	g.release(m, lvl1)
	g.release(m, lvl2)
	g.release(m, lvl3)
}

// T6 release 形状：release/两步回滚路径零 Redis 命令；I-3 回滚净零不变量在
// 份额配置下保持；位掩码契约不变。
func TestConcReleaseShapeAndRollbackNetZero(t *testing.T) {
	mr, c := newTestGateRedis(t)
	meta := domain.KeyMeta{KeyID: 1, UserID: 1, UserMaxConc: 4, KeyMaxConc: 1}
	a := newConcAuth(t, map[string]domain.KeyMeta{"h": meta})
	g := a.gate
	g.SetInstancesProvider(fakeInstances(2)) // user share 2 / key share 1
	NewConcSyncWorker(a, c, "inst-a", nil)   // 客户端在场但未启动：请求路径零命令

	base := mr.CommandCount()
	lvl1, ok := g.acquire(meta)
	require.True(t, ok)
	require.Equal(t, 3, lvl1)

	// 第 2 笔：user 层份额内（2≤2）；key 层超份额（casInc(1) 失败）→ 无视图
	// fail-open → concAllows(2≤1=false) → 两步回滚
	lvl2, ok := g.acquire(meta)
	require.False(t, ok)
	require.Zero(t, lvl2)
	snap := g.store.Load()
	require.Equal(t, int64(1), snap.users[1].Load(), "I-3：user 计数复原（2→1）")
	require.Equal(t, int64(1), snap.keys[1].Load(), "key 计数不动")

	g.release(meta, lvl1)
	require.Zero(t, snap.keys[1].Load(), "release 位掩码减")
	require.Zero(t, g.store.Load().users[1].Load(), "release 净零")
	require.Zero(t, mr.CommandCount()-base, "release/回滚路径零 Redis 命令")
}

// worker 生命周期契约：nil client Start 安全 no-op（无视图=全额本地）；未 Start
// Close 安全；Start/Close 幂等。
func TestConcWorkerLifecycle(t *testing.T) {
	a := newConcAuth(t, map[string]domain.KeyMeta{
		"h": {KeyID: 1, UserID: 1, UserMaxConc: 2},
	})
	wNil := NewConcSyncWorker(a, nil, "inst-nil", nil)
	require.NoError(t, wNil.Start(t.Context()), "nil client Start no-op")
	require.NoError(t, wNil.Close(context.Background()))
	require.Nil(t, a.gate.cluster.Load(), "nil client 恒无视图")

	_, c := newTestGateRedis(t)
	w := NewConcSyncWorker(a, c, "inst-a", nil)
	require.NoError(t, w.Close(context.Background()), "未 Start Close 安全")
	require.NoError(t, w.Start(t.Context()))
	require.NoError(t, w.Start(t.Context()), "Start 幂等")
	require.Equal(t, "conc-sync", w.Name())
	a.gate.store.Load().users[1].Store(1)
	require.Eventually(t, func() bool {
		cv := a.gate.cluster.Load()
		return cv != nil && cv.users[1].selfLast == 1
	}, 3*time.Second, 10*time.Millisecond, "循环在跑（≤数 tick 出视图）")
	require.NoError(t, w.Close(context.Background()))
	require.NoError(t, w.Close(context.Background()), "Close 幂等")
}
