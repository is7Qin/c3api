// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// 计费游标消费者单测（F2 ledger-cursor，spec 2026-08-23 §四）：fake LedgerStore
// 覆盖正常消费链（billed 翻转 + 余额断言）、毒行二分隔离推进、cost=0 批量快速
// 标记、lag 护栏、Close 排空清空、会话锁互斥。PG 全链路归 repository 直调测试。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// —— fake LedgerStore ——

// fakeLedgerRow 游标行内存态（billed 翻转 + overdraft 回写断言面）。
type fakeLedgerRow struct {
	row       domain.LedgerRow
	createdAt time.Time
	billed    bool
	od        bool
}

// deductObs DeductOnlyAndMark 调用观测（分组/成本和断言面）。
type deductObs struct {
	userID, cost int64
	ids          []int64
}

// fakeLedgerStore 六方法全实现：rows 即游标真值（billed 翻转 = 退出游标），
// balances 模拟用户余额扣减（缺失用户 = quarantined 出口），failLeft 注入结构
// 错误（id → 剩余失败次数），failMark 注入标记面故障（整库故障形态）。全部
// 方法持锁——-race 下多 worker 并发消费安全。
type fakeLedgerStore struct {
	mu          sync.Mutex
	rows        map[int64]*fakeLedgerRow
	balances    map[int64]int64
	failLeft    map[int64]int
	failMark    bool // MarkBilledBulk 恒失败（整库故障注入）
	lockOK      bool // false → AcquireBillingLock 报错
	lockHeld    bool // 已持有 → ok=false（互斥面）
	fetches     int
	lagProbes   int
	endlessRows bool // 每次取批合成一行全新未标记行（周期预算回归——持续到达形态）
	endlessID   int64
	deductCalls []deductObs
	markCalls   [][]int64
	chunkCalls  [][]domain.LedgerGroup
}

func newFakeLedgerStore() *fakeLedgerStore {
	return &fakeLedgerStore{
		rows:     map[int64]*fakeLedgerRow{},
		balances: map[int64]int64{},
		failLeft: map[int64]int{},
		lockOK:   true,
	}
}

// seedRow 种子未标记行（billed=false 出生态；返回行副本）。
func (s *fakeLedgerStore) seedRow(id, userID, cost int64, createdAt time.Time) domain.LedgerRow {
	row := domain.LedgerRow{ID: id, UserID: userID, Cost: cost,
		Model: "gpt-4o", BillingTier: "auto", CallCount: 1, Format: "openai-chat"}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[id] = &fakeLedgerRow{row: row, createdAt: createdAt}
	return row
}

func (s *fakeLedgerStore) setBalance(userID, bal int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balances[userID] = bal
}

func (s *fakeLedgerStore) setFail(id int64, times int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failLeft[id] = times
}

func (s *fakeLedgerStore) holdLock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lockHeld = true
}

func (s *fakeLedgerStore) releaseLock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lockHeld = false
}

func (s *fakeLedgerStore) AcquireBillingLock(ctx context.Context) (release func(), ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lockOK {
		return nil, false, errors.New("injected lock acquire failure")
	}
	if s.lockHeld {
		return nil, false, nil
	}
	s.lockHeld = true
	return func() {
		s.mu.Lock()
		s.lockHeld = false
		s.mu.Unlock()
	}, true, nil
}

func (s *fakeLedgerStore) FetchUnbilledBatch(ctx context.Context, limit int) ([]domain.LedgerRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetches++
	ids := make([]int64, 0, len(s.rows))
	for id, r := range s.rows {
		if !r.billed { // D1 读取面：含 cost<=0 行，已在路由分叉
			ids = append(ids, id)
		}
	}
	if s.endlessRows { // 周期预算回归：合成全新未标记行（持续到达形态——无预算则 drainLoop 永不返回）
		s.endlessID++
		s.rows[s.endlessID] = &fakeLedgerRow{row: domain.LedgerRow{ID: s.endlessID,
			UserID: s.endlessID, Cost: 100, Model: "gpt-4o", BillingTier: "auto",
			CallCount: 1, Format: "openai-chat"}}
		ids = append(ids, s.endlessID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > limit {
		ids = ids[:limit]
	}
	out := make([]domain.LedgerRow, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.rows[id].row)
	}
	return out, nil
}

func (s *fakeLedgerStore) DeductOnlyAndMark(ctx context.Context, userID, cost int64, ids []int64) (balanceAfter int64, overdrafted, quarantined bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if n := s.failLeft[id]; n > 0 {
			s.failLeft[id] = n - 1
			return 0, false, false, errors.New("injected structural failure")
		}
	}
	s.deductCalls = append(s.deductCalls, deductObs{userID: userID, cost: cost, ids: append([]int64(nil), ids...)})
	if cost <= 0 { // 防御路径：纯标记不扣减（对齐 deductOnlyCore 契约）
		s.markLocked(ids, false)
		return 0, false, false, nil
	}
	before, exists := s.balances[userID]
	if !exists { // 用户缺失：跳过扣减仍标记全部 ids（不变量 #1 尾语义）
		s.markLocked(ids, false)
		return 0, false, true, nil
	}
	overdrafted = before < cost
	s.balances[userID] = before - cost
	s.markLocked(ids, overdrafted)
	return s.balances[userID], overdrafted, false, nil
}

// DeductGroupsAndMark chunk 单事务模拟（F2-opt D3；原子形态：任一组失败 =
// 整块零变动——结构错误预检先行、余额变更收集后统一应用）。逐组记录
// deductObs（与单组面同观测语义，flatten 断言面不变）+ chunkCalls 打包观测。
func (s *fakeLedgerStore) DeductGroupsAndMark(ctx context.Context, groups []domain.LedgerGroup) ([]domain.LedgerGroupOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunkCalls = append(s.chunkCalls, append([]domain.LedgerGroup(nil), groups...)) // 入口记录：失败尝试亦观测
	for _, g := range groups {                                                        // 结构错误预检：任一 id 注入失败 → 整块回滚形态
		for _, r := range g.Rows {
			if n := s.failLeft[r.ID]; n > 0 {
				s.failLeft[r.ID] = n - 1
				return nil, errors.New("injected structural failure")
			}
		}
	}
	outcomes := make([]domain.LedgerGroupOutcome, len(groups))
	pending := make(map[int64]int64, len(groups))
	for i, g := range groups {
		var cost int64
		ids := make([]int64, 0, len(g.Rows))
		for _, r := range g.Rows {
			cost += r.Cost
			ids = append(ids, r.ID)
		}
		s.deductCalls = append(s.deductCalls, deductObs{userID: g.UserID, cost: cost, ids: ids})
		if cost > 0 {
			before, exists := s.balances[g.UserID]
			if !exists { // 用户缺失：跳过扣减仍标记全部 ids（不变量 #1 尾语义）
				outcomes[i].Quarantined = true
			} else {
				outcomes[i].Overdrafted = before < cost
				pending[g.UserID] = before - cost
				outcomes[i].BalanceAfter = before - cost
			}
		}
	}
	if s.failMark {
		return nil, errors.New("injected bulk-mark failure (DB-wide)")
	}
	for uid, bal := range pending {
		s.balances[uid] = bal
	}
	for i, g := range groups {
		ids := make([]int64, 0, len(g.Rows))
		for _, r := range g.Rows {
			ids = append(ids, r.ID)
		}
		s.markLocked(ids, outcomes[i].Overdrafted)
	}
	return outcomes, nil
}

// markLocked 标记翻转（调用方持锁）：od 回写仅作用于本次翻转的行。
func (s *fakeLedgerStore) markLocked(ids []int64, od bool) {
	for _, id := range ids {
		if r, ok := s.rows[id]; ok && !r.billed {
			r.billed = true
			r.od = od
		}
	}
}

func (s *fakeLedgerStore) MarkBilledBulk(ctx context.Context, ids []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failMark {
		return errors.New("injected bulk-mark failure (DB-wide)")
	}
	s.markCalls = append(s.markCalls, append([]int64(nil), ids...))
	for _, id := range ids {
		if r, ok := s.rows[id]; ok {
			r.billed = true // 幂等：已标记静默跳过
		}
	}
	return nil
}

func (s *fakeLedgerStore) UnbilledLag(ctx context.Context) (time.Time, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lagProbes++
	var count int64
	oldest := time.Time{}
	for _, r := range s.rows {
		if r.billed {
			continue
		}
		count++
		if oldest.IsZero() || r.createdAt.Before(oldest) {
			oldest = r.createdAt
		}
	}
	return oldest, count, nil
}

// —— 观测访问器（测试断言面，持锁读） ——

func (s *fakeLedgerStore) isBilled(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[id].billed
}

func (s *fakeLedgerStore) overdraftOf(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[id].od
}

func (s *fakeLedgerStore) balanceOf(userID int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.balances[userID]
}

func (s *fakeLedgerStore) unbilledCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.rows {
		if !r.billed {
			n++
		}
	}
	return n
}

func (s *fakeLedgerStore) deductSnapshot() []deductObs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]deductObs(nil), s.deductCalls...)
}

func (s *fakeLedgerStore) markSnapshot() [][]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]int64(nil), s.markCalls...)
}

func (s *fakeLedgerStore) chunkSnapshot() [][]domain.LedgerGroup {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]domain.LedgerGroup(nil), s.chunkCalls...)
}

func (s *fakeLedgerStore) fetchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches
}

func (s *fakeLedgerStore) lagProbeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lagProbes
}

// —— 构造辅助 ——

// newFlusherWith 指定 store/worker 数/余额快照种子的构造（loader map 决定
// bal.Set 定向刷新的可见性——缺失条目 Set 忽略）。Reload 预灌快照对齐生产
// 装配序（main 启动期注册表 ReloadAll）；fakeBalLoader 无失败注入路径，错误
// 不可能（failAt 形态仅 balances_test 直构）。
func newFlusherWith(store *fakeLedgerStore, workers int, loader map[int64]int64) *Flusher {
	f := NewFlusher(FlushConfig{
		FlushInterval:          time.Hour,
		BalanceRefreshInterval: time.Hour,
		Workers:                workers,
	}, store, NewBalances(fakeBalLoader{m: loader}, nil), nil)
	_ = f.bal.Reload(context.Background())
	return f
}

// newTestLogger warn 级文件 logger（Warn/Error 断言用；Windows 上 zap 句柄不
// 释放，目录清理 best-effort）。
func newTestLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "flusher-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	out := filepath.Join(dir, "out.json")
	logger, err := logx.New("warn", out)
	require.NoError(t, err)
	return logger, out
}

// restoreLagThrottle 注入 lag 刷新节流阈值并在测试结束还原（D2 节流可测化，
// 形态对齐 inflightAbandonGrace 注入惯例）。
func restoreLagThrottle(t *testing.T, d time.Duration) {
	t.Helper()
	old := lagRefreshInterval
	lagRefreshInterval = d
	t.Cleanup(func() { lagRefreshInterval = old })
}

// —— 用例 ——

// TestFlusherConsumesAndMarksBilled 正常消费链：取批 → 按 user 分组各一笔事务
// （同 user 成本聚合）→ billed 翻转 + 余额精确扣减 + 余额快照定向刷新 +
// lastFlush/unbilledN 观测推进。
func TestFlusherConsumesAndMarksBilled(t *testing.T) {
	store := newFakeLedgerStore()
	r1 := store.seedRow(1, 1, 100, time.Now())
	r2 := store.seedRow(2, 1, 300, time.Now())
	r3 := store.seedRow(3, 2, 200, time.Now())
	store.setBalance(1, 1000)
	store.setBalance(2, 500)
	f := newFlusherWith(store, 4, map[int64]int64{1: 1000, 2: 500})

	n := f.consumeCycle(context.Background(), false)
	require.Equal(t, int64(3), n, "整批退出游标")

	calls := store.deductSnapshot()
	require.Len(t, calls, 2, "按 user 分组两笔事务")
	byUID := map[int64]deductObs{}
	for _, c := range calls {
		byUID[c.userID] = c
	}
	require.Equal(t, int64(400), byUID[1].cost, "同 user 成本聚合")
	require.Equal(t, []int64{r1.ID, r2.ID}, byUID[1].ids)
	require.Equal(t, int64(200), byUID[2].cost)

	require.True(t, store.isBilled(r1.ID), "billed 翻转")
	require.True(t, store.isBilled(r2.ID))
	require.True(t, store.isBilled(r3.ID))
	require.False(t, store.overdraftOf(r1.ID), "余额充足不透支")
	require.Equal(t, int64(600), store.balanceOf(1), "1000-400")
	require.Equal(t, int64(300), store.balanceOf(2))

	bal, ok := f.bal.BalanceOf(1)
	require.True(t, ok)
	require.Equal(t, int64(600), bal, "bal.Set 定向刷新余额快照")

	require.Greater(t, f.lastFlush.Load(), int64(0), "成功消费推进 lastFlush")
	require.Zero(t, f.unbilledN.Load(), "游标清空（lag 探测刷新）")
	require.Zero(t, f.quarantined.Load())
}

// TestFlusherOverdraftFlow 透支流：余额不足 → 无条件扣允许透支（负余额）+
// overdraft 回写行内（B2）。
func TestFlusherOverdraftFlow(t *testing.T) {
	store := newFakeLedgerStore()
	row := store.seedRow(1, 1, 400, time.Now())
	store.setBalance(1, 100)
	f := newFlusherWith(store, 1, map[int64]int64{1: 100})

	n := f.consumeCycle(context.Background(), false)
	require.Equal(t, int64(1), n)
	require.True(t, store.isBilled(row.ID))
	require.True(t, store.overdraftOf(row.ID), "透支回写 overdraft=true")
	require.Equal(t, int64(-300), store.balanceOf(1), "无条件扣允许透支")
	bal, ok := f.bal.BalanceOf(1)
	require.True(t, ok)
	require.Equal(t, int64(-300), bal, "负余额刷新进快照")
}

// TestFlusherPoisonIsolated 毒行二分隔离推进：含毒行的组整体失败 → 折半重试
// （无毒半原子推进）→ 单行毒行重试仍失败 → MarkBilledBulk 终极隔离（未扣费
// 写销 + QuarantinedRows 计数 + Error 日志）——游标永不卡死。
func TestFlusherPoisonIsolated(t *testing.T) {
	store := newFakeLedgerStore()
	logger, out := newTestLogger(t)
	poison := store.seedRow(1, 7, 100, time.Now())
	ok1 := store.seedRow(2, 7, 100, time.Now())
	ok2 := store.seedRow(3, 7, 100, time.Now())
	ok3 := store.seedRow(4, 7, 100, time.Now())
	store.setBalance(7, 1000)
	store.setFail(poison.ID, 1<<30) // 恒失败（大数近似持久毒）
	f := newFlusherWith(store, 1, map[int64]int64{7: 1000})
	f.log = logger

	n := f.consumeCycle(context.Background(), false)
	require.Equal(t, int64(4), n, "3 行扣费推进 + 1 行隔离退出 = 游标清空")

	require.True(t, store.isBilled(ok1.ID) && store.isBilled(ok2.ID) && store.isBilled(ok3.ID), "无毒行照常扣费标记")
	require.True(t, store.isBilled(poison.ID), "毒行被终极隔离标记（退出游标）")
	require.False(t, store.overdraftOf(poison.ID))
	require.Equal(t, int64(700), store.balanceOf(7), "毒行未扣费（写销该行计费）")
	require.Equal(t, int64(1), f.quarantined.Load(), "QuarantinedRows 计数")
	require.Zero(t, f.unbilledN.Load(), "游标清空（毒行不卡死）")

	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "poison row isolated without deduction")
	require.Contains(t, string(b), `"level":"error"`, "止损升级 Error 级（可观测）")
	require.Contains(t, string(b), `"usage_log_id":1`)
}

// TestFlusherTransientFailureRetried 单行瞬态失败消歧：len==1 重试一次成功 =
// 瞬态（DB 抖动），正常收尾不隔离不计数。
func TestFlusherTransientFailureRetried(t *testing.T) {
	store := newFakeLedgerStore()
	row := store.seedRow(1, 1, 100, time.Now())
	store.setBalance(1, 500)
	store.setFail(row.ID, 1) // 仅首次失败
	f := newFlusherWith(store, 1, map[int64]int64{1: 500})

	n := f.consumeCycle(context.Background(), false)
	require.Equal(t, int64(1), n)
	require.True(t, store.isBilled(row.ID), "重试成功正常收尾")
	require.Equal(t, int64(400), store.balanceOf(1), "恰扣一次（无重复扣费）")
	require.Zero(t, f.quarantined.Load(), "瞬态失败不计隔离")
}

// TestFlusherWholeGroupFailureReplays 整库故障形态：两半都失败且标记面同故障
// → 放弃本组本周期（行保持 unbilled 由 DB 天然重放），不误隔离不热旋。
func TestFlusherWholeGroupFailureReplays(t *testing.T) {
	store := newFakeLedgerStore()
	r1 := store.seedRow(1, 1, 100, time.Now())
	r2 := store.seedRow(2, 1, 100, time.Now())
	store.setBalance(1, 500)
	store.setFail(r1.ID, 1<<30)
	store.setFail(r2.ID, 1<<30)
	store.failMark = true // 标记面同故障（终极隔离也不可用）
	f := newFlusherWith(store, 1, map[int64]int64{1: 500})

	n := f.consumeCycle(context.Background(), false)
	require.Zero(t, n, "两半都失败 = 无进展")
	require.False(t, store.isBilled(r1.ID) || store.isBilled(r2.ID), "行保持 unbilled 下周期重放")
	require.Zero(t, f.quarantined.Load(), "整库故障不误隔离")
	require.Equal(t, int64(500), store.balanceOf(1), "零扣费（不丢不重——初值原样）")
}

// TestFlusherQuarantineMissingUser 用户缺失（不变量 #1 尾语义）：跳过扣减仍
// 标记全部 ids、quarantined=true → QuarantinedRows 计数 + Warn——毒用户不卡
// 游标。
func TestFlusherQuarantineMissingUser(t *testing.T) {
	store := newFakeLedgerStore()
	logger, out := newTestLogger(t)
	r1 := store.seedRow(1, 999999, 100, time.Now())
	r2 := store.seedRow(2, 999999, 200, time.Now())
	f := newFlusherWith(store, 1, map[int64]int64{})
	f.log = logger

	n := f.consumeCycle(context.Background(), false)
	require.Equal(t, int64(2), n, "整组标记退出游标")
	require.True(t, store.isBilled(r1.ID) && store.isBilled(r2.ID))
	require.Equal(t, int64(2), f.quarantined.Load(), "QuarantinedRows 计数")
	require.Greater(t, f.lastFlush.Load(), int64(0), "标记成功亦为成功消费周期")

	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "user missing, rows marked without deduction")
}

// TestFlusherZeroCostFastMark cost=0 快速路径（m4 CostZeroFastMark，F2-opt D1
// 内存路由形态）：免费/吸收态行同批取出 → 一次 MarkBilledBulk 纯标记，不进
// FEFO 机器（零资金移动）。
func TestFlusherZeroCostFastMark(t *testing.T) {
	store := newFakeLedgerStore()
	z1 := store.seedRow(1, 1, 0, time.Now())
	z2 := store.seedRow(2, 2, 0, time.Now())
	paid := store.seedRow(3, 1, 100, time.Now())
	store.setBalance(1, 1000)
	f := newFlusherWith(store, 1, map[int64]int64{1: 1000})

	n := f.consumeCycle(context.Background(), false)
	require.Equal(t, int64(3), n, "cost=0 两行纯标记 + cost>0 一行扣费")
	require.True(t, store.isBilled(z1.ID) && store.isBilled(z2.ID), "cost=0 批量标记")
	require.Equal(t, [][]int64{{z1.ID, z2.ID}}, store.markSnapshot(), "零价行单次 MarkBilledBulk（D1 路由）")
	calls := store.deductSnapshot()
	require.Len(t, calls, 1, "cost=0 不进 FEFO 机器")
	require.Equal(t, []int64{paid.ID}, calls[0].ids, "扣费事务只含 cost>0 行")
}

// TestFlusherChunkPackingAndDegradation chunk 打包与降级链（F2-opt D3）：多组
// 单例 chunk 常规推进；chunk 结构错误 → 组为单位折半 → 单例组走行级二分机制
// ——两层二分正交复用，无毒组照常推进。
func TestFlusherChunkPackingAndDegradation(t *testing.T) {
	t.Run("multi-group single chunk", func(t *testing.T) {
		store := newFakeLedgerStore()
		r1 := store.seedRow(1, 1, 100, time.Now())
		r2 := store.seedRow(2, 1, 200, time.Now())
		r3 := store.seedRow(3, 2, 300, time.Now())
		store.setBalance(1, 1000)
		store.setBalance(2, 500)
		f := newFlusherWith(store, 1, map[int64]int64{1: 1000, 2: 500}) // 同分片：两组一 chunk

		n := f.consumeCycle(context.Background(), false)
		require.Equal(t, int64(3), n)
		chunks := store.chunkSnapshot()
		require.Len(t, chunks, 1, "同分片连续组打包单 chunk")
		require.Len(t, chunks[0], 2, "chunk 含两个用户组")
		require.Equal(t, int64(700), store.balanceOf(1), "逐用户 Δ余额精确（1000−100−200）")
		require.Equal(t, int64(200), store.balanceOf(2))
		require.True(t, store.isBilled(r1.ID) && store.isBilled(r2.ID) && store.isBilled(r3.ID))
	})

	t.Run("chunk failure halves to singleton then row bisect", func(t *testing.T) {
		store := newFakeLedgerStore()
		logger, _ := newTestLogger(t)
		poison := store.seedRow(1, 9, 100, time.Now())
		okA := store.seedRow(2, 8, 100, time.Now())
		okB := store.seedRow(3, 7, 100, time.Now())
		store.setBalance(9, 1000)
		store.setBalance(8, 1000)
		store.setBalance(7, 1000)
		store.setFail(poison.ID, 1<<30) // 恒失败毒行居 chunk 首组
		f := newFlusherWith(store, 1, map[int64]int64{7: 1000, 8: 1000, 9: 1000})
		f.log = logger

		n := f.consumeCycle(context.Background(), false)
		require.Equal(t, int64(3), n, "毒组折半至单例隔离，邻组照常推进")
		chunks := store.chunkSnapshot()
		require.Len(t, chunks, 2, "降级链：整 chunk 首试 + 折半重试（单例组走单组面不再进 chunk）")
		require.Len(t, chunks[0], 3)
		require.Len(t, chunks[1], 2)
		require.True(t, store.isBilled(okA.ID) && store.isBilled(okB.ID), "邻组不受毒组拖累")
		require.True(t, store.isBilled(poison.ID), "毒行终极隔离退出游标")
		require.Equal(t, int64(1000), store.balanceOf(9), "毒行未扣费")
		require.Equal(t, int64(900), store.balanceOf(8))
		require.Equal(t, int64(900), store.balanceOf(7))
		require.Equal(t, int64(1), f.quarantined.Load())
	})
}

// TestFlusherLagGuardrailWarns lag 护栏：最老 unbilled 行距今超保留期 80% →
// 高声 Warn（边沿触发）；低于阈值回落复位告警边沿（真值照常刷新）。
func TestFlusherLagGuardrailWarns(t *testing.T) {
	restoreLagThrottle(t, 0) // 禁节流：多周期序列每周期探测刷新（节流行为归专测）
	store := newFakeLedgerStore()
	logger, out := newTestLogger(t)
	store.seedRow(1, 1, 100, time.Now().Add(-20*time.Hour)) // 阈值 = 24h×80% = 19.2h
	f := newFlusherWith(store, 1, map[int64]int64{1: 1})
	f.log = logger
	f.cfg.LogRetentionDays = 1

	store.holdLock() // 锁外观测：他实例消费时本实例 lag 护栏照常工作
	f.consumeCycle(context.Background(), false)
	require.Positive(t, f.lagMs.Load(), "lag 真值刷新")
	require.True(t, f.lagWarned.Load(), "越线置位告警边沿")
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	first := strings.Count(string(b), "retention guardrail")
	require.Equal(t, 1, first, "高声 Warn 落盘")
	require.Contains(t, string(b), `"log_retention_days":1`)

	f.consumeCycle(context.Background(), false) // 仍超阈值：边沿触发不重复刷屏
	require.NoError(t, logger.Sync())
	b, err = os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, first, strings.Count(string(b), "retention guardrail"), "边沿触发不刷屏")

	// 低于阈值：回落复位告警边沿（真值照常——行仍在游标内）
	store.mu.Lock()
	store.rows[1].createdAt = time.Now().Add(-1 * time.Hour)
	store.mu.Unlock()
	f.consumeCycle(context.Background(), false)
	require.Positive(t, f.lagMs.Load(), "行仍在游标内，lag 真值保持")
	require.False(t, f.lagWarned.Load(), "回落复位告警边沿")
}

// TestFlusherLagDisabled 护栏禁用（LogRetentionDays <= 0 对齐 retention 不删除
// 语义）：超龄行不告警，lag 真值照常刷新。
func TestFlusherLagDisabled(t *testing.T) {
	restoreLagThrottle(t, 0)
	store := newFakeLedgerStore()
	logger, out := newTestLogger(t)
	store.seedRow(1, 1, 100, time.Now().Add(-100*time.Hour))
	f := newFlusherWith(store, 1, map[int64]int64{1: 1})
	f.log = logger
	f.cfg.LogRetentionDays = 0

	store.holdLock()
	f.consumeCycle(context.Background(), false)
	require.Positive(t, f.lagMs.Load(), "护栏禁用不影响真值刷新")
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.NotContains(t, string(b), "retention guardrail")
}

// TestFlusherLagRefreshThrottle lag 刷新节流边沿三态（F2-opt D2）：首调必刷 /
// 节流窗内跳过 / Close 排空语境（drain=true）绕过节流强制刷新——防「陈旧
// unbilledN==0 × n>0」提前退出排空。
func TestFlusherLagRefreshThrottle(t *testing.T) {
	restoreLagThrottle(t, time.Hour) // 拉长窗口：第二调必落窗内
	store := newFakeLedgerStore()
	store.seedRow(1, 1, 100, time.Now())
	f := newFlusherWith(store, 1, map[int64]int64{1: 100})

	f.consumeCycle(context.Background(), false)
	require.Equal(t, 1, store.lagProbeCount(), "首调必刷（lastLag 零值）")

	f.consumeCycle(context.Background(), false)
	require.Equal(t, 1, store.lagProbeCount(), "节流窗内跳过（Stats 允许 ≤1s 陈旧度）")

	f.consumeCycle(context.Background(), true)
	require.Equal(t, 2, store.lagProbeCount(), "drain 语境绕过节流强制刷新")
}

// TestPackChunksChunkPacking 打包策略纯函数锚（F2-opt D3）：片内连续组保序
// 切分、≤chunkUsersLimit 用户/chunk、尾块余数、空入空出。
func TestPackChunksChunkPacking(t *testing.T) {
	groups := make([]*domain.LedgerGroup, chunkUsersLimit+3)
	for i := range groups {
		groups[i] = &domain.LedgerGroup{UserID: int64(i)}
	}
	chunks := packChunks(groups)
	require.Len(t, chunks, 2)
	require.Len(t, chunks[0], chunkUsersLimit)
	require.Len(t, chunks[1], 3)
	require.Equal(t, int64(0), chunks[0][0].UserID)
	require.Equal(t, int64(chunkUsersLimit), chunks[1][0].UserID, "连续组保序切分")
	require.Empty(t, packChunks(nil))
}

// TestFlusherLockMutualExclusion 会话锁互斥：他实例持锁（ok=false）→ 本周期
// 跳过取批（零 fetch 零消费）；抢锁报错 → Warn + 跳过——双实例绝不重复消费
// 同批（Momus M1 防线）。
func TestFlusherLockMutualExclusion(t *testing.T) {
	t.Run("held by another instance", func(t *testing.T) {
		store := newFakeLedgerStore()
		store.seedRow(1, 1, 100, time.Now())
		store.setBalance(1, 100)
		store.holdLock()
		f := newFlusherWith(store, 1, map[int64]int64{1: 100})

		n := f.consumeCycle(context.Background(), false)
		require.Zero(t, n, "他实例持锁：本周期跳过")
		require.Zero(t, store.fetchCount(), "未取批")
		require.False(t, store.isBilled(1), "零消费")
	})

	t.Run("acquire error warns and skips", func(t *testing.T) {
		store := newFakeLedgerStore()
		logger, out := newTestLogger(t)
		store.seedRow(1, 1, 100, time.Now())
		store.lockOK = false
		f := newFlusherWith(store, 1, map[int64]int64{1: 100})
		f.log = logger

		n := f.consumeCycle(context.Background(), false)
		require.Zero(t, n)
		require.Zero(t, store.fetchCount())
		require.NoError(t, logger.Sync())
		b, err := os.ReadFile(out)
		require.NoError(t, err)
		require.Contains(t, string(b), "billing cursor lock acquire failed")
	})
}

// TestFlusherCloseDrainsCursor Close 排空至游标清空（D2 排空节奏）：单批
// LIMIT 2000 内积压一个取批往返全量消费 + 一次空批确认即退出，预算内完整排空、
// 无截断 Warn；幂等二次 Close 不再消费。
func TestFlusherCloseDrainsCursor(t *testing.T) {
	store := newFakeLedgerStore()
	const total = 1200 // 单批容量内：数据批 + 空批确认 = 2 次取批
	for i := 1; i <= total; i++ {
		uid := int64(i%3 + 1)
		store.seedRow(int64(i), uid, 10, time.Now())
		store.setBalance(uid, 1_000_000)
	}
	f := newFlusherWith(store, 4, map[int64]int64{1: 1_000_000, 2: 1_000_000, 3: 1_000_000})
	logger, out := newTestLogger(t)
	f.log = logger

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, f.Close(ctx))

	require.Equal(t, 0, store.unbilledCount(), "排空至游标清空")
	require.Equal(t, 2, store.fetchCount(), "排空节奏：数据批 + 空批确认即退出（一批一 tick 废除）")
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.NotContains(t, string(b), "truncated drain", "预算内完整排空无截断")

	fetches := store.fetchCount()
	require.NoError(t, f.Close(context.Background())) // 幂等：closeOnce 短路
	require.Equal(t, fetches, store.fetchCount(), "二次 Close 不再消费")
}

// blockingDeductStore DeductGroupsAndMark 阻塞至 ctx 取消（模拟慢 DB 在途 chunk
// 事务；取消传播后快速失败——行保持 unbilled）。
type blockingDeductStore struct {
	*fakeLedgerStore
	started chan struct{}
	once    sync.Once
}

func (s *blockingDeductStore) DeductGroupsAndMark(ctx context.Context, groups []domain.LedgerGroup) ([]domain.LedgerGroupOutcome, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestFlusherCloseTruncatesOnBudget 停机排空受 ctx 预算约束：到期 → Cancel
// baseCtx（在途事务快速失败回滚，行保持 unbilled 不丢）+ 截断 Warn（含已消费/
// 剩余行数），不无界阻塞停机。
func TestFlusherCloseTruncatesOnBudget(t *testing.T) {
	inner := newFakeLedgerStore()
	store := &blockingDeductStore{fakeLedgerStore: inner, started: make(chan struct{})}
	inner.seedRow(1, 1, 100, time.Now())
	inner.seedRow(2, 2, 100, time.Now())
	inner.setBalance(1, 100)
	inner.setBalance(2, 100)
	logger, out := newTestLogger(t)
	f := newFlusherWith(inner, 1, map[int64]int64{1: 100, 2: 100})
	f.store = store // 包装注入（阻塞扣费面）
	f.log = logger

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(150*time.Millisecond))
	defer cancel()
	start := time.Now()
	require.NoError(t, f.Close(ctx))
	require.Less(t, time.Since(start), 5*time.Second, "预算到期截断退出（不阻塞停机）")
	<-store.started // 在途事务确实被启动过（截断发生在消费中）

	require.Equal(t, 2, inner.unbilledCount(), "取消后行保持 unbilled（重启收敛）")
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "shutdown budget exceeded, truncated drain")
	require.Contains(t, string(b), `"consumed_rows":0`)
	require.Contains(t, string(b), `"remaining_rows":2`)
}

// ignoreCtxDeductStore DeductGroupsAndMark 忽略 ctx 永久阻塞（模拟 DB 病态卡死
// ——取消路径本身被拖住的极端形态；A-P2-8-2 第二 select 兜底目标）。测试结束
// 即弃置（在途 goroutine 无放行通道，属刻意泄漏）。
type ignoreCtxDeductStore struct {
	*fakeLedgerStore
	started chan struct{}
}

func (s *ignoreCtxDeductStore) DeductGroupsAndMark(ctx context.Context, groups []domain.LedgerGroup) ([]domain.LedgerGroupOutcome, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-make(chan struct{}) // 永久阻塞（不响应 ctx 取消；无发送者，永不返回）
	return nil, nil
}

// TestFlusherCloseAbandonsInflightOnTimeout A-P2-8-2：驱动不尊重 ctx 时 Close
// 不再无界等待——预算到期 → Cancel baseCtx → 收尾宽限超时 → Warn 放弃排空、
// 截断退出（在途事务由编排层强杀收尾，行保持 unbilled 不丢）。
func TestFlusherCloseAbandonsInflightOnTimeout(t *testing.T) {
	old := inflightAbandonGrace
	inflightAbandonGrace = 50 * time.Millisecond
	t.Cleanup(func() { inflightAbandonGrace = old })

	inner := newFakeLedgerStore()
	store := &ignoreCtxDeductStore{fakeLedgerStore: inner, started: make(chan struct{}, 1)}
	// 两用户一组一 chunk（单例组走单组面，不进 chunk 事务面——须多组才命中阻塞点）
	inner.seedRow(1, 1, 100, time.Now())
	inner.seedRow(2, 2, 100, time.Now())
	inner.setBalance(1, 100)
	inner.setBalance(2, 100)
	logger, out := newTestLogger(t)
	f := newFlusherWith(inner, 1, map[int64]int64{1: 100})
	f.store = store
	f.log = logger

	done := make(chan struct{})
	go func() { defer close(done); f.consumeCycle(context.Background(), false) }()
	<-store.started // 在途周期已占住 flushMu

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(100*time.Millisecond))
	defer cancel()
	start := time.Now()
	require.NoError(t, f.Close(ctx))
	require.Less(t, time.Since(start), 2*time.Second, "放弃排空快速退出（不得无界等待在途周期）")

	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "billing flusher close: in-flight cycle not finished in time, abandoning drain")
	require.Contains(t, string(b), `"level":"warn"`)
}

// TestFlusherStartTwiceFails Start 幂等守卫（worker.Worker 契约面）；loop ctx
// 取消后 Close 正常排空退出（游标空即返回）。
func TestFlusherStartTwiceFails(t *testing.T) {
	store := newFakeLedgerStore()
	f := newFlusherWith(store, 1, map[int64]int64{})
	loopCtx, loopCancel := context.WithCancel(context.Background())
	require.NoError(t, f.Start(loopCtx))
	err := f.Start(context.Background())
	require.Error(t, err, "重复 Start 拒绝")
	loopCancel()
	require.NoError(t, f.Close(context.Background()))
}

// —— 排空周期预算（F2-opt G1 审计 D 面回归） ——

// TestFlusherDrainCycleBudget 周期预算到期收尾：持续到达形态（每次取批合成
// 全新未标记行）下，无预算的 drainLoop 永不返回——会话锁 + flushMu 无界持有，
// refreshT/Balances.Reload 停摆 → 新用户预检快照缺失 402。预算注入后
// consumeCycle 必须有限墙钟内让位收尾，且消费有进展、多批取数发生。
func TestFlusherDrainCycleBudget(t *testing.T) {
	restore := drainCycleBudget
	drainCycleBudget = 50 * time.Millisecond
	t.Cleanup(func() { drainCycleBudget = restore })

	store := newFakeLedgerStore()
	store.endlessRows = true
	f := newFlusherWith(store, 2, map[int64]int64{})

	start := time.Now()
	n := f.consumeCycle(context.Background(), false)
	elapsed := time.Since(start)

	require.Greater(t, n, int64(0), "预算期内有真实消费进展")
	require.Less(t, elapsed, 5*time.Second, "预算到期必须让位收尾（无预算形态本调用永不返回）")
	require.GreaterOrEqual(t, store.fetches, 3, "多批取数发生（非单批即止）")
}
