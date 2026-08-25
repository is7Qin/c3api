// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/redisx"
)

// --- 账号并发份额+借用测试（spec conc-share-borrow-account §3 表格 A1-A8；
// A8 = -race 门禁本身，全家族在 race 下跑） ---
//
// 测试基座：miniredis + redisx.Open（dogfood 全仓唯一构造点纪律）；复用同包
// newTestScheduler/tpl/acc 装配调度器。等待一律 require.Eventually 轮询谓词或
// 同步 tick 直调（同包私有方法），零 sleep。视图陈旧边界不靠真实时钟推进——
// 手工构建/老化 clusterView（at 直接置过去），字段陈旧用回填 ts 直写 HASH，
// 全部确定性。

// fixedN 固定 N 的 InstancesProvider 测试桩。
type fixedN int

func (f fixedN) ClusterInstances() int { return int(f) }

// varN 可变 N 的 InstancesProvider 测试桩（N 动态变化场景）。
type varN struct{ v atomic.Int32 }

func (d *varN) ClusterInstances() int { return int(d.v.Load()) }

// newConcTestRedis miniredis 基座（proxy/concsync_test.go 同款）。
func newConcTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	return mr, c
}

// concChatAcc 组 10 内 chat 格式 "gpt-4o" 模型的账号快捷构造。
func concChatAcc(id, maxConc int) *domain.Account {
	return acc(int64(id), tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"}), maxConc)
}

// concSelect 组 10 的选号快捷入口。
func concSelect(s *Scheduler) (*Selection, error) {
	return s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
}

// concSetCur 直写账号在途计数（模拟在途，绕开 Select）。
func concSetCur(s *Scheduler, accID int64, v int64) {
	s.store.byID.Load().(map[int64]*accountSnapshot)[accID].concurrency.Store(v)
}

// concCur 读账号在途计数。
func concCur(s *Scheduler, accID int64) int64 {
	return s.store.byID.Load().(map[int64]*accountSnapshot)[accID].concurrency.Load()
}

// A1 结构短路：N=1 时 share=limit → 超份额分支数学上不可达，Select/Release
// 全路径零 Redis 命令（含视图在场时——判定是纯内存读）。worker 在场但未启动。
func TestAccConcN1StructuralShortCircuit(t *testing.T) {
	mr, c := newConcTestRedis(t)
	s := newTestScheduler(t, []*domain.Account{concChatAcc(1, 4)})
	s.SetInstancesProvider(fixedN(1))
	NewConcSyncWorker(s, c, "inst-a", nil) // 构造零副作用（未启动）

	base := mr.CommandCount()
	for range 4 { // 占满限额 4：share=limit，全部 fast-path
		sel, err := concSelect(s)
		require.NoError(t, err)
		require.Equal(t, int64(1), sel.AccountID)
	}
	_, err := concSelect(s)
	require.ErrorIs(t, err, ErrNoAvailable, "N=1 第 5 笔超限拒绝（与 main 分支同点）")

	// 视图在场（新鲜）：判定走内存读，超份额分支依旧数学不可达、同点拒绝
	s.concView.Store(&clusterView{accounts: map[int64]concSnap{
		1: {total: 4, selfLast: 4, at: time.Now()},
	}})
	_, err = concSelect(s)
	require.ErrorIs(t, err, ErrNoAvailable, "N=1 视图路径同样同点拒绝")

	for range 4 {
		s.Release(1)
	}
	require.Zero(t, concCur(s, 1), "Release 净零")
	require.Zero(t, mr.CommandCount()-base, "Select/Release 全路径零 Redis 命令（公理 2 钉死）")
}

// A2a share 公式矩阵：floor / max(1) / N=1 恒等。
func TestConcShareFormulaMatrix(t *testing.T) {
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

// A2b N 从 3→1→3 变化即时生效且在途继承：N 只是现读除数，无模式无转换。
// 超份额无视图时 fail-open 按「全额 limit」本地判定放行至真上限；有新鲜视图
// 时才按对账聚合判定。
func TestConcShareDynamicNInflightInheritance(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{concChatAcc(1, 9)})
	n := &varN{}
	n.v.Store(3)
	s.SetInstancesProvider(n)

	for range 3 { // N=3 share=3：占满 fast-path 份额
		_, err := concSelect(s)
		require.NoError(t, err)
	}
	for range 6 { // 超份额、无视图 → fail-open 全额本地（4..9 ≤ 9 放行）
		_, err := concSelect(s)
		require.NoError(t, err, "fail-open 全额本地判定放行")
	}
	_, err := concSelect(s)
	require.ErrorIs(t, err, ErrNoAvailable, "真上限 9 兜底：第 10 笔拒绝")

	// N→1 即时生效：share=limit=9，但真上限兜底不变（在途 9 继承，全拒）
	n.v.Store(1)
	for range 2 {
		_, err := concSelect(s)
		require.ErrorIs(t, err, ErrNoAvailable, "在途继承：满载时 N 切换不放行")
	}
	require.Equal(t, int64(9), concCur(s, 1), "在途计数不受 N 切换影响")

	// 释放 6 笔 → 在途 3；N→3：share=3 恰满，超份额借位经新鲜单实例视图
	// （effective≈L_now）放行至真上限
	for range 6 {
		s.Release(1)
	}
	s.concView.Store(&clusterView{accounts: map[int64]concSnap{
		1: {total: 3, selfLast: 3, at: time.Now()},
	}})
	sel, err := concSelect(s)
	require.NoError(t, err, "超份额借位：effective=3−3+4=4 < 9")
	require.Equal(t, int64(1), sel.AccountID)
	s.Release(1)
}

// A3 扫描语义四态（spec §1.1 判定嵌入扫描循环——借位拒绝=换号，不是拒流）：
// 借位放行 / 视图满换号 / 全员视图满 ErrNoAvailable / 陈旧视图 fail-open。
// 两账号场景断言与加权轮询游标相位无关（任一访问序收敛同一结果）。
func TestClusterViewScanSemantics(t *testing.T) {
	fresh := func(total, selfLast int64) concSnap {
		return concSnap{total: total, selfLast: selfLast, at: time.Now()}
	}
	staled := func(total, selfLast int64) concSnap {
		return concSnap{total: total, selfLast: selfLast,
			at: time.Now().Add(-concViewStale - time.Second)}
	}

	// 态 1 借位放行：单账号在途恰达份额（cur=share=2），新鲜视图对账放行借位。
	s1 := newTestScheduler(t, []*domain.Account{concChatAcc(1, 4)})
	s1.SetInstancesProvider(fixedN(2)) // share 2
	concSetCur(s1, 1, 2)
	s1.concView.Store(&clusterView{accounts: map[int64]concSnap{1: fresh(2, 2)}})
	sel, err := concSelect(s1)
	require.NoError(t, err, "借位放行：effective=2−2+3=3 < 4")
	require.Equal(t, int64(1), sel.AccountID)
	s1.Release(1)

	// 态 2 视图满换号：账号 1 超份额且视图满（eff=5≥4）被跳过 → 选到账号 2
	// （fast-path 份额内，与扫描起点无关）。
	s2 := newTestScheduler(t, []*domain.Account{concChatAcc(1, 4), concChatAcc(2, 4)})
	s2.SetInstancesProvider(fixedN(2))
	concSetCur(s2, 1, 2)
	concSetCur(s2, 2, 1)
	s2.concView.Store(&clusterView{accounts: map[int64]concSnap{1: fresh(4, 2)}})
	sel, err = concSelect(s2)
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID, "视图满候选被跳过换下一候选")
	s2.Release(2)

	// 态 3 全员视图满 → ErrNoAvailable：账号 1 本地达真上限、账号 2 超份额且
	// 视图满（eff=4−2+3=5 ≥ 4）——借用拒绝是换号，换无可换即拒绝。
	s3 := newTestScheduler(t, []*domain.Account{concChatAcc(1, 4), concChatAcc(2, 4)})
	s3.SetInstancesProvider(fixedN(2))
	concSetCur(s3, 1, 4)
	concSetCur(s3, 2, 2)
	s3.concView.Store(&clusterView{accounts: map[int64]concSnap{2: fresh(4, 2)}})
	_, err = concSelect(s3)
	require.ErrorIs(t, err, ErrNoAvailable)

	// 态 4 陈旧 fail-open：同态 3 的"仅账号 1 有本地余量"布局，新鲜视图下
	// 账号 1 借位被拒 → ErrNoAvailable；视图老化后 fail-open 按「全额 limit」
	// 本地判定放行账号 1（唯一差异 = 视图陈旧性）。
	s4 := newTestScheduler(t, []*domain.Account{concChatAcc(1, 4), concChatAcc(2, 4)})
	s4.SetInstancesProvider(fixedN(2))
	concSetCur(s4, 1, 2)
	concSetCur(s4, 2, 4)
	s4.concView.Store(&clusterView{accounts: map[int64]concSnap{
		1: fresh(4, 2), 2: fresh(4, 4),
	}})
	_, err = concSelect(s4)
	require.ErrorIs(t, err, ErrNoAvailable, "新鲜视图：账号 1 借位被拒、账号 2 达真上限")
	s4.concView.Store(&clusterView{accounts: map[int64]concSnap{
		1: staled(4, 2), 2: staled(4, 4),
	}})
	sel, err = concSelect(s4)
	require.NoError(t, err, "陈旧视图 fail-open：账号 1 全额本地可选（3 ≤ 4）")
	require.Equal(t, int64(1), sel.AccountID)
	s4.Release(1)
}

// A3 补充——concAllows 判定边界：公式两侧、无视图/条目缺失/陈旧 fail-open。
func TestClusterViewJudgmentBoundaries(t *testing.T) {
	// 无视图：fail-open 全额本地（lnow ≤ limit 放行 / > limit 拒绝）
	require.True(t, concAllows(nil, 1, 6, 6))
	require.False(t, concAllows(nil, 1, 6, 7))

	stale := time.Now().Add(-concViewStale - time.Second)
	view := &clusterView{accounts: map[int64]concSnap{
		1: {total: 7, selfLast: 3, at: time.Now()},
		9: {total: 7, selfLast: 3, at: stale},
	}}
	// 新鲜条目：effective = 7−3+L_now
	require.True(t, concAllows(view, 1, 6, 1), "effective=5 < 6 放行")
	require.True(t, concAllows(view, 1, 6, 0), "effective=4 < 6 放行")
	require.False(t, concAllows(view, 1, 6, 2), "effective=6 ≥ 6 拒绝")
	// 条目缺失（他账号有视图、本账号无）→ fail-open
	require.True(t, concAllows(view, 99, 6, 6), "缺失条目全额本地")
	require.False(t, concAllows(view, 99, 6, 7))
	// 条目陈旧 → fail-open（不采信旧聚合）
	require.True(t, concAllows(view, 9, 6, 6), "陈旧条目全额本地（非对账判定）")
	require.False(t, concAllows(view, 9, 6, 7))
}

// A4 对账收敛：占用 ≤1 tick 出现在视图；绝对值覆盖写杀漂移；EXPIRE 续期/TTL 自灭。
func TestAccConcReconciliationConverges(t *testing.T) {
	mr, c := newConcTestRedis(t)
	s := newTestScheduler(t, []*domain.Account{concChatAcc(1, 8)})
	s.SetInstancesProvider(fixedN(2))
	w := NewConcSyncWorker(s, c, "inst-a", nil)
	ctx := context.Background()

	_, err := concSelect(s)
	require.NoError(t, err)
	w.tick(ctx) // 同步一 tick：占用即出现在视图（≤1 tick 收敛）
	cv := s.concView.Load()
	require.NotNil(t, cv)
	require.Equal(t, int64(1), cv.accounts[1].selfLast, "在途 1 上报")
	require.GreaterOrEqual(t, cv.accounts[1].total, int64(1))

	// 绝对值覆盖写：再占 3 笔 → 下个 tick selfLast=4（非增量累加 1+3）
	for range 3 {
		_, err := concSelect(s)
		require.NoError(t, err)
	}
	w.tick(ctx)
	cv = s.concView.Load()
	require.Equal(t, int64(4), cv.accounts[1].selfLast, "每 tick 绝对值覆盖写（杀漂移）")

	// EXPIRE 续期：键带 TTL；worker 停摆 + FastForward 过期窗 → 键自灭（防遗弃累积）
	ttl, err := c.TTL(ctx, concAccountPrefix+"1").Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 15*time.Second, "EXPIRE 16s 已续期")
	raw, err := c.HGet(ctx, concAccountPrefix+"1", "inst-a").Result()
	require.NoError(t, err)
	require.Regexp(t, `^4 \d{13}$`, raw, "value 形如 \"<L> <unixmilli>\"")
	mr.FastForward(concKeyTTL + time.Second)
	cnt, err := c.Exists(ctx, concAccountPrefix+"1").Result()
	require.NoError(t, err)
	require.Zero(t, cnt, "全体消亡后键 EXPIRE 自灭")
}

// A4 补充——对账聚合：陈旧字段剔除（ts 早于 now−4s 不计入）+ selfLast 取本次上报值。
// 幽灵字段用回填 ts 直写 HASH，全部确定性。
func TestAccConcAggregationFreshnessAndSelfLast(t *testing.T) {
	_, c := newConcTestRedis(t)
	s := newTestScheduler(t, []*domain.Account{concChatAcc(7, 10)})
	w := NewConcSyncWorker(s, c, "inst-self", nil)

	// 他实例新鲜字段 + 幽灵陈旧字段（ts 回填 5s 前）直写 HASH
	nowMS := time.Now().UnixMilli()
	require.NoError(t, c.HSet(context.Background(), concAccountPrefix+"7",
		"inst-other", strconv.FormatInt(3, 10)+" "+strconv.FormatInt(nowMS, 10)).Err())
	require.NoError(t, c.HSet(context.Background(), concAccountPrefix+"7",
		"ghost", strconv.FormatInt(9, 10)+" "+strconv.FormatInt(nowMS-int64((concFieldStale+time.Second)/time.Millisecond), 10)).Err())

	concSetCur(s, 7, 2) // 在途 > 0 才上报 → 本实例字段入聚合
	w.tick(context.Background())

	cv := s.concView.Load()
	require.NotNil(t, cv, "tick 后视图换入")
	snap, ok := cv.accounts[7]
	require.True(t, ok)
	require.Equal(t, int64(3+2), snap.total, "新鲜字段求和（自身 2 + 他实例 3），幽灵 9 陈旧剔除")
	require.Equal(t, int64(2), snap.selfLast, "selfLast = 本次上报值（精化基准）")

	// 判定精化端到端：N=2 share=5，L_now=3 → effective=3−2+3=4 < 10 放行；
	// 推到真上限边缘 → effective=5−2+10=13 ≥ 10 拒绝
	s.SetInstancesProvider(fixedN(2))
	sel, err := concSelect(s)
	require.NoError(t, err)
	require.Equal(t, int64(7), sel.AccountID)
	concSetCur(s, 7, 9) // 推到真上限边缘（绕开 Select 直写）
	_, err = concSelect(s)
	require.ErrorIs(t, err, ErrNoAvailable, "effective=5−2+10=13 ≥ 10 拒绝")
	require.Equal(t, int64(9), concCur(s, 7), "拒绝笔不占槽")
}

// A5 fail-open 结构性质（spec §3/验收 §3）：miniredis 关闭 → tick 失败（errs
// 为确定性信号）、视图冻结不换入 → 陈旧后自动退化全额放行（真上限内无
// ErrNoAvailable 风暴）；同端口恢复 → ≤ 数 tick 换入新视图回归共识。
func TestAccConcFailOpenOnRedisOutageAndRecover(t *testing.T) {
	mr := miniredis.NewMiniRedis()
	require.NoError(t, mr.StartAddr("127.0.0.1:0"))
	t.Cleanup(func() { mr.Close() })
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })

	s := newTestScheduler(t, []*domain.Account{concChatAcc(1, 6)}) // N=2 → share 3
	s.SetInstancesProvider(fixedN(2))
	w := NewConcSyncWorker(s, c, "inst-a", nil)
	require.NoError(t, w.Start(t.Context()))
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	for range 3 { // 份额内占用进入视图
		_, err := concSelect(s)
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool { // ≤1 tick：视图建立
		cv := s.concView.Load()
		return cv != nil && time.Since(cv.accounts[1].at) < concViewStale
	}, 3*time.Second, 10*time.Millisecond, "视图建立")

	sel, err := concSelect(s)
	require.NoError(t, err, "超份额借位：effective=3−3+4=4 < 6")

	addr := mr.Addr() // Close 后不可读，先取
	mr.Close()
	// 冻结证明：连续两个失败 tick 之间视图指针不变（确定性信号，无 sleep）
	errs1 := w.errs.Load()
	require.Eventually(t, func() bool { return w.errs.Load() >= errs1+1 },
		8*time.Second, 20*time.Millisecond, "tick 失败可观测（冻结语义进入）")
	vFrozen := s.concView.Load()
	errs2 := w.errs.Load()
	require.Eventually(t, func() bool { return w.errs.Load() >= errs2+1 },
		8*time.Second, 20*time.Millisecond, "第二个失败 tick")
	require.Same(t, vFrozen, s.concView.Load(), "故障期视图冻结（不换入新快照）")

	// 手工老化冻结视图（模拟 4s 真实时钟流逝——不 sleep）
	aged := &clusterView{accounts: make(map[int64]concSnap, len(vFrozen.accounts))}
	for id, sn := range vFrozen.accounts {
		sn.at = time.Now().Add(-concViewStale - time.Second)
		aged.accounts[id] = sn
	}
	s.concView.Store(aged)

	// 陈旧 → fail-open 全额本地：真上限 6 内继续放行（无 ErrNoAvailable 风暴），
	// 第 7 笔拒绝（本地真上限兜底不受故障影响）
	for range 2 {
		_, err := concSelect(s)
		require.NoError(t, err, "fail-open 全额放行（≤6）")
	}
	_, err = concSelect(s)
	require.ErrorIs(t, err, ErrNoAvailable, "真上限兜底：7 > 6 拒绝")
	s.Release(sel.AccountID)
	s.Release(1)
	s.Release(1)
	require.Equal(t, int64(3), concCur(s, 1), "回到持 3 笔基态")

	// 同端口重启 → 连接池自动重连，≤数 tick 换入新视图回归共识
	restarted := miniredis.NewMiniRedis()
	require.NoError(t, restarted.StartAddr(addr))
	t.Cleanup(func() { restarted.Close() })
	require.Eventually(t, func() bool {
		cv := s.concView.Load()
		return cv != nil && cv != aged && time.Since(cv.accounts[1].at) < concViewStale
	}, 8*time.Second, 10*time.Millisecond, "恢复后新视图换入")
	require.Eventually(t, func() bool { // 绝对值覆盖写收敛到当前在途 3
		return s.concView.Load().accounts[1].selfLast == 3
	}, 3*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(0), w.errs.Load(), "成功 tick 清错误态")

	// 共识回归实证：他实例报 3 → 聚合 6 → 第 4 笔借位被拒（不再 fail-open 放行）
	require.NoError(t, c.HSet(context.Background(), concAccountPrefix+"1",
		"inst-b", "3 "+strconv.FormatInt(time.Now().UnixMilli(), 10)).Err())
	require.Eventually(t, func() bool { return s.concView.Load().accounts[1].total == 6 },
		3*time.Second, 10*time.Millisecond, "他实例字段进聚合")
	_, err = concSelect(s)
	require.ErrorIs(t, err, ErrNoAvailable, "effective=6−3+4=7 ≥ 6 借位被拒（共识恢复）")
	s.Release(1)
	s.Release(1)
	s.Release(1)
}

// A6 Release 形状 + CAS 封顶竞态：Select/Release 路径零 Redis 命令；并发双借
// 不过真上限（CAS 天然封顶，无需新锁）——8 goroutine 并发风暴下在途峰值恒 ≤ 4。
func TestAccConcReleaseShapeAndBorrowCapRace(t *testing.T) {
	mr, c := newConcTestRedis(t)
	s := newTestScheduler(t, []*domain.Account{concChatAcc(1, 4)})
	s.SetInstancesProvider(fixedN(2))
	NewConcSyncWorker(s, c, "inst-a", nil) // 客户端在场但未启动：请求路径零命令

	base := mr.CommandCount()
	sel, err := concSelect(s)
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	s.Release(1)
	require.Zero(t, concCur(s, 1), "Release 净零")
	require.Zero(t, mr.CommandCount()-base, "Select/Release 路径零 Redis 命令")

	// 并发双借封顶竞态（A8 -race 家族载体）：无视图 fail-open 下并发争抢，
	// 在途峰值恒 ≤ 真上限 4——借用放行落入既有 CAS(cur, cur+1)，双借同时过
	// limit−1 时第二个 CAS 必败（换下一候选/重扫，不拒流不超限）。
	var inflight, peak atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				sel, err := concSelect(s)
				if err != nil {
					continue // 满载拒绝：合法形态（保守多拒）
				}
				cur := inflight.Add(1)
				for {
					p := peak.Load()
					if cur <= p || peak.CompareAndSwap(p, cur) {
						break
					}
				}
				s.Release(sel.AccountID)
				inflight.Add(-1)
			}
		}()
	}
	wg.Wait()
	require.LessOrEqual(t, peak.Load(), int64(4), "CAS 封顶：并发双借不过真上限")
	require.Zero(t, inflight.Load(), "风暴后全部释放")
	require.Zero(t, concCur(s, 1), "计数器净零（Select 成功数 == Release 数）")
}

// A7 继承回归：reload/rebuild 计数继承（O-2 修正）不影响上报与判定——继承后
// 上报读当前 byID 快照天然安全，借位判定按继承值正确工作。
func TestAccConcInheritedCounterReporting(t *testing.T) {
	_, c := newConcTestRedis(t)
	s := newTestScheduler(t, []*domain.Account{concChatAcc(1, 6)})
	s.SetInstancesProvider(fixedN(2)) // share 3
	w := NewConcSyncWorker(s, c, "inst-a", nil)
	ctx := context.Background()

	concSetCur(s, 1, 3) // 模拟在途 3（恰达份额）
	w.tick(ctx)
	cv := s.concView.Load()
	require.NotNil(t, cv)
	require.Equal(t, int64(3), cv.accounts[1].selfLast, "上报值 = 当前在途")

	// reload 重建：O-2 继承——实例指针不变计数连续，Runtime 可见
	require.NoError(t, s.InvalidateAllSync())
	rt, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, int64(3), rt.Concurrency, "reload 后计数继承（O-2 不回退）")

	// 继承后上报判定正确：tick 读重建后的 byID，仍报 3；借位判定按其工作
	w.tick(ctx)
	cv = s.concView.Load()
	require.Equal(t, int64(3), cv.accounts[1].selfLast, "重建后上报继承值")
	sel, err := concSelect(s)
	require.NoError(t, err, "借位放行：effective=3−3+4=4 < 6（判定基于继承计数）")
	require.Equal(t, int64(1), sel.AccountID)

	// 视图聚合推满 → 借位被拒（继承计数 + 新鲜对账联合判定）
	concSetCur(s, 1, 4)
	s.concView.Store(&clusterView{accounts: map[int64]concSnap{
		1: {total: 6, selfLast: 4, at: time.Now()},
	}})
	_, err = concSelect(s)
	require.ErrorIs(t, err, ErrNoAvailable, "effective=6−4+5=7 ≥ 6 拒绝")

	for range 4 {
		s.Release(1)
	}
	require.Zero(t, concCur(s, 1), "净零收尾")
}

// worker 生命周期契约：nil client Start 安全 no-op（无视图=全额本地）；未 Start
// Close 安全；Start/Close 幂等；循环在跑（≤数 tick 出视图）。
func TestAccConcWorkerLifecycle(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{concChatAcc(1, 2)})
	wNil := NewConcSyncWorker(s, nil, "inst-nil", nil)
	require.NoError(t, wNil.Start(t.Context()), "nil client Start no-op")
	require.NoError(t, wNil.Close(context.Background()))
	require.Nil(t, s.concView.Load(), "nil client 恒无视图")

	_, c := newConcTestRedis(t)
	w := NewConcSyncWorker(s, c, "inst-a", nil)
	require.NoError(t, w.Close(context.Background()), "未 Start Close 安全")
	require.NoError(t, w.Start(t.Context()))
	require.NoError(t, w.Start(t.Context()), "Start 幂等")
	require.Equal(t, "account-conc-sync", w.Name())
	concSetCur(s, 1, 1)
	require.Eventually(t, func() bool {
		cv := s.concView.Load()
		return cv != nil && cv.accounts[1].selfLast == 1
	}, 3*time.Second, 10*time.Millisecond, "循环在跑（≤数 tick 出视图）")
	require.NoError(t, w.Close(context.Background()))
	require.NoError(t, w.Close(context.Background()), "Close 幂等")
}
