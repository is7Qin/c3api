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

// laneCall 车道结算调用观测（车道序/路由断言面）。
type laneCall struct {
	fefo   bool
	marked int64
}

// fakeTemp 临时额度内存态（FEFO 消耗断言面；expiresAt 零值 = 永久 NULLS LAST）。
type fakeTemp struct {
	id        int64
	amount    int64
	expiresAt time.Time
}

// fakeLedgerStore 六方法全实现：rows 即游标真值（billed 翻转 = 退出游标），
// balances 模拟用户余额（缺失用户 = quarantined 出口），temps 模拟临时额度
// （FEFO 车道谓词与消耗），failLeft 注入语句级结构错误（整语句失败形态），
// failMark 注入标记面故障。全部方法持锁——-race 下安全。
type fakeLedgerStore struct {
	mu          sync.Mutex
	rows        map[int64]*fakeLedgerRow
	balances    map[int64]int64
	temps       map[int64][]fakeTemp
	tempSeq     int64
	failLeft    map[int64]int
	failMark    bool // MarkBilledBulk 恒失败（整库故障注入）
	lockOK      bool // false → AcquireBillingLock 报错
	lockHeld    bool // 已持有 → ok=false（互斥面）
	fetches   int
	lagProbes int
	laneCalls []laneCall
	markCalls [][]int64
}

func newFakeLedgerStore() *fakeLedgerStore {
	return &fakeLedgerStore{
		rows:     map[int64]*fakeLedgerRow{},
		balances: map[int64]int64{},
		temps:    map[int64][]fakeTemp{},
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

// SettleBalanceBatch/SettleFefoBatch 语句化结算面模拟（三车道拓扑）：车道谓词
// 互斥（temp-active 路由）→ 候选批（id 升序 LIMIT）→ 结构错误预检（整语句失败
// 形态）→ 按用户聚合 FEFO 消耗/spill → 条件扣/透支/幽灵隔离 → 标记。与 SQL
// 语句语义同构（单元级；位级行为归 repository PG 测试族）。
func (s *fakeLedgerStore) SettleBalanceBatch(ctx context.Context, limit int) (domain.SettlementSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settleLocked(limit, false)
}

func (s *fakeLedgerStore) SettleFefoBatch(ctx context.Context, limit int) (domain.SettlementSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settleLocked(limit, true)
}

func (s *fakeLedgerStore) settleLocked(limit int, fefo bool) (domain.SettlementSummary, error) {
	now := time.Now()
	tempActive := func(uid int64) bool {
		for _, tp := range s.temps[uid] {
			if tp.amount > 0 && (tp.expiresAt.IsZero() || tp.expiresAt.After(now)) {
				return true
			}
		}
		return false
	}
	ids := make([]int64, 0, len(s.rows))
	for id, r := range s.rows {
		if !r.billed {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var cands []domain.LedgerRow
	for _, id := range ids { // 车道谓词互斥：balance NOT-IN / fefo IN temp-active
		r := s.rows[id]
		if r.row.Cost <= 0 || tempActive(r.row.UserID) != fefo {
			continue
		}
		cands = append(cands, r.row)
		if len(cands) >= limit {
			break
		}
	}
	res := domain.SettlementSummary{BatchRows: int64(len(cands))}
	if len(cands) == 0 {
		return res, nil
	}
	for _, r := range cands { // 结构错误预检：任一候选行注入失败 → 整语句失败形态
		if n := s.failLeft[r.ID]; n > 0 {
			s.failLeft[r.ID] = n - 1
			return domain.SettlementSummary{}, errors.New("injected structural failure")
		}
	}
	s.laneCalls = append(s.laneCalls, laneCall{fefo: fefo, marked: int64(len(cands))})
	type agg struct {
		delta int64
		rows  []domain.LedgerRow
	}
	order := make([]int64, 0, 4)
	byUID := make(map[int64]*agg, 4)
	for _, r := range cands { // 按用户聚合（保首见序——确定性）
		a := byUID[r.UserID]
		if a == nil {
			a = &agg{}
			byUID[r.UserID] = a
			order = append(order, r.UserID)
		}
		a.delta += r.Cost
		a.rows = append(a.rows, r)
	}
	for _, uid := range order {
		a := byUID[uid]
		spill := a.delta
		if fefo {
			spill -= s.drawTempsLocked(uid, a.delta, now)
		}
		bal, exists := s.balances[uid]
		switch {
		case !exists: // 幽灵用户：跳扣仍标记全部行（不变量 #1 尾语义）
			res.Quarantined += int64(len(a.rows))
			s.markRowsLocked(a.rows, false)
		case spill <= 0: // temp 全覆盖：零资金移动，纯标记
			s.markRowsLocked(a.rows, false)
		default:
			od := bal < spill // 条件扣未命中 → 无条件透支补刀
			s.balances[uid] = bal - spill
			res.Balances = append(res.Balances, domain.UserBalance{UserID: uid, Balance: bal - spill})
			if od {
				res.ForcedUsers++
			} else {
				res.DebitedUsers++
			}
			s.markRowsLocked(a.rows, od)
		}
	}
	res.Marked = res.BatchRows // fake 无并发抢标面——marked==batch 守卫恒一致
	return res, nil
}

// drawTempsLocked FEFO 消耗（expires ASC NULLS LAST——零值永久排最后；返回实际
// 消耗和，行级条件扣在单线程 fake 内恒成功）。
func (s *fakeLedgerStore) drawTempsLocked(uid, delta int64, now time.Time) int64 {
	tps := s.temps[uid]
	idx := make([]int, 0, len(tps))
	for i, tp := range tps {
		if tp.amount > 0 && (tp.expiresAt.IsZero() || tp.expiresAt.After(now)) {
			idx = append(idx, i)
		}
	}
	sort.Slice(idx, func(a, b int) bool {
		ia, ib := idx[a], idx[b]
		za, zb := tps[ia].expiresAt.IsZero(), tps[ib].expiresAt.IsZero()
		if za != zb {
			return zb // 非 NULL 前，NULL（永久）后
		}
		if tps[ia].expiresAt.Equal(tps[ib].expiresAt) {
			return tps[ia].id < tps[ib].id
		}
		return tps[ia].expiresAt.Before(tps[ib].expiresAt)
	})
	remain := delta
	for _, i := range idx {
		if remain <= 0 {
			break
		}
		take := min(tps[i].amount, remain)
		tps[i].amount -= take
		remain -= take
	}
	return delta - remain
}

// markRowsLocked 标记翻转（调用方持锁）：od 回写仅作用于本次翻转的行。
func (s *fakeLedgerStore) markRowsLocked(rows []domain.LedgerRow, od bool) {
	for _, r := range rows {
		if fr, ok := s.rows[r.ID]; ok && !fr.billed {
			fr.billed = true
			fr.od = od
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

// seedTemp 种子临时额度行（返回行 id；expiresAt 零值 = 永久 NULLS LAST）。
func (s *fakeLedgerStore) seedTemp(userID, amount int64, expiresAt time.Time) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tempSeq++
	s.temps[userID] = append(s.temps[userID], fakeTemp{id: s.tempSeq, amount: amount, expiresAt: expiresAt})
	return s.tempSeq
}

func (s *fakeLedgerStore) tempAmount(id int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, tps := range s.temps {
		for _, tp := range tps {
			if tp.id == id {
				return tp.amount
			}
		}
	}
	return -1
}

func (s *fakeLedgerStore) laneSnapshot() []laneCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]laneCall(nil), s.laneCalls...)
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

func (s *fakeLedgerStore) markSnapshot() [][]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]int64(nil), s.markCalls...)
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

// TestFlusherConsumesAndMarksBilled 正常消费链：三车道顺序消费 → billed 翻转 +
// 余额精确扣减（同用户成本聚合）+ 余额快照定向刷新 + lastFlush/unbilledN 观测
// 推进。
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

	calls := store.laneSnapshot()
	require.NotEmpty(t, calls)
	require.False(t, calls[0].fefo, "balance 车道先行（§〇-b 周期序）")
	require.Equal(t, int64(3), calls[0].marked, "余额-only 用户全批 balance 车道结算")

	require.True(t, store.isBilled(r1.ID), "billed 翻转")
	require.True(t, store.isBilled(r2.ID))
	require.True(t, store.isBilled(r3.ID))
	require.False(t, store.overdraftOf(r1.ID), "余额充足不透支")
	require.Equal(t, int64(600), store.balanceOf(1), "1000-400 同用户成本聚合")
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

// TestFlusherPersistentFailureReplays 车道语句持续失败形态：Warn 归零本周期
// （毒行梯子已在仓库侧收敛——隔离行以 Quarantined 计数随 summary 返回，见
// repository PG 梯子族），行保持 unbilled 下周期重放，不误隔离不热旋。
func TestFlusherPersistentFailureReplays(t *testing.T) {
	store := newFakeLedgerStore()
	logger, out := newTestLogger(t)
	r1 := store.seedRow(1, 1, 100, time.Now())
	r2 := store.seedRow(2, 1, 100, time.Now())
	store.setBalance(1, 500)
	store.setFail(r1.ID, 1<<30)
	store.setFail(r2.ID, 1<<30)
	f := newFlusherWith(store, 1, map[int64]int64{1: 500})
	f.log = logger

	n := f.consumeCycle(context.Background(), false)
	require.Zero(t, n, "语句失败 = 无进展")
	require.False(t, store.isBilled(r1.ID) || store.isBilled(r2.ID), "行保持 unbilled 下周期重放")
	require.Zero(t, f.quarantined.Load(), "编排层不隔离（隔离归仓库梯子）")
	require.Equal(t, int64(500), store.balanceOf(1), "零扣费（不丢不重——初值原样）")
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "billing settle lane failed")
}

// TestFlusherTransientFailureRetried 瞬态失败自愈：首周期语句失败归零（行保持
// unbilled），次周期重放成功恰扣一次——不丢不重。
func TestFlusherTransientFailureRetried(t *testing.T) {
	store := newFakeLedgerStore()
	row := store.seedRow(1, 1, 100, time.Now())
	store.setBalance(1, 500)
	store.setFail(row.ID, 1) // 仅首次失败
	f := newFlusherWith(store, 1, map[int64]int64{1: 500})

	n := f.consumeCycle(context.Background(), false)
	require.Zero(t, n, "首周期瞬态失败无进展")
	require.False(t, store.isBilled(row.ID))

	n = f.consumeCycle(context.Background(), false)
	require.Equal(t, int64(1), n, "次周期重放成功")
	require.True(t, store.isBilled(row.ID))
	require.Equal(t, int64(400), store.balanceOf(1), "恰扣一次（无重复扣费）")
	require.Zero(t, f.quarantined.Load(), "瞬态失败不计隔离")
}

// TestFlusherQuarantineMissingUser 用户缺失（不变量 #1 尾语义）：跳过扣减仍
// 标记全部行、Quarantined 行数随 summary 返回 → QuarantinedRows 计数 + Warn
// ——毒用户不卡游标。
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
	require.Contains(t, string(b), "rows marked without deduction")
}

// TestFlusherZeroCostFastMark cost=0 快速路径（零价 sweep 车道）：免费/吸收态
// 行由 sweep 一次 MarkBilledBulk 纯标记，不进结算语句（零资金移动）。
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
	require.Equal(t, [][]int64{{z1.ID, z2.ID}}, store.markSnapshot(), "零价行单次 MarkBilledBulk（sweep 车道）")
	require.True(t, store.isBilled(paid.ID), "付费行 balance 车道扣费标记")
	require.Equal(t, int64(900), store.balanceOf(1), "付费行恰扣一次；零价行零资金移动")
	require.False(t, store.overdraftOf(z1.ID) || store.overdraftOf(z2.ID), "零价行 overdraft 出生 false 保持")
}

// TestFlusherTwoLaneDisjointness 两车道互斥路由（§〇-b）：temp-active 用户恒入
// FEFO 车道、余额-only 用户恒入 Balance 车道——同周期双种群各自结算互不越道。
func TestFlusherTwoLaneDisjointness(t *testing.T) {
	store := newFakeLedgerStore()
	tempRow := store.seedRow(1, 7, 70_000, time.Now()) // temp-active：FEFO 车道
	balRow := store.seedRow(2, 8, 30_000, time.Now())  // 余额-only：Balance 车道
	tp := store.seedTemp(7, 50_000, time.Now().Add(time.Hour))
	store.setBalance(7, 100_000)
	store.setBalance(8, 100_000)
	f := newFlusherWith(store, 1, map[int64]int64{7: 80_000, 8: 100_000})

	n := f.consumeCycle(context.Background(), false)
	require.Equal(t, int64(2), n, "两车道同周期各自结算")

	calls := store.laneSnapshot()
	require.False(t, calls[0].fefo, "balance 车道先行")
	require.Equal(t, int64(1), calls[0].marked, "balance 车道只吃余额-only 用户")
	require.True(t, calls[1].fefo, "fefo 车道随后")
	require.Equal(t, int64(1), calls[1].marked, "fefo 车道只吃 temp-active 用户")

	// FEFO 消耗 50000 + spill 20000 条件扣 → 余额 80000；余额-only 用户直扣 30000
	require.Equal(t, int64(80_000), store.balanceOf(7), "temp-active 用户 spill 补差精确")
	require.Zero(t, store.tempAmount(tp), "临时额度恰被 FEFO 消耗")
	require.Equal(t, int64(70_000), store.balanceOf(8), "余额-only 用户直扣")
	require.True(t, store.isBilled(tempRow.ID) && store.isBilled(balRow.ID))
	require.False(t, store.overdraftOf(tempRow.ID) || store.overdraftOf(balRow.ID))
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

// blockingSettleStore SettleBalanceBatch 阻塞至 ctx 取消（模拟慢 DB 在途结算
// 事务；取消传播后快速失败——行保持 unbilled）。
type blockingSettleStore struct {
	*fakeLedgerStore
	started chan struct{}
	once    sync.Once
}

func (s *blockingSettleStore) SettleBalanceBatch(ctx context.Context, limit int) (domain.SettlementSummary, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return domain.SettlementSummary{}, ctx.Err()
}

// TestFlusherCloseTruncatesOnBudget 停机排空受 ctx 预算约束：到期 → Cancel
// baseCtx（在途事务快速失败回滚，行保持 unbilled 不丢）+ 截断 Warn（含已消费/
// 剩余行数），不无界阻塞停机。
func TestFlusherCloseTruncatesOnBudget(t *testing.T) {
	inner := newFakeLedgerStore()
	store := &blockingSettleStore{fakeLedgerStore: inner, started: make(chan struct{})}
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

// ignoreCtxSettleStore SettleBalanceBatch 忽略 ctx 永久阻塞（模拟 DB 病态卡死
// ——取消路径本身被拖住的极端形态；A-P2-8-2 第二 select 兜底目标）。测试结束
// 即弃置（在途 goroutine 无放行通道，属刻意泄漏）。
type ignoreCtxSettleStore struct {
	*fakeLedgerStore
	started chan struct{}
}

func (s *ignoreCtxSettleStore) SettleBalanceBatch(ctx context.Context, limit int) (domain.SettlementSummary, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-make(chan struct{}) // 永久阻塞（不响应 ctx 取消；无发送者，永不返回）
	return domain.SettlementSummary{}, nil
}

// endlessSettleStore 持续到达形态（周期预算回归）：每次 Balance 车道结算前合成
// 一行全新未标记行——持续到达下每轮恒有进展，无预算的 drainLoop 永不返回。
type endlessSettleStore struct {
	*fakeLedgerStore
	nextID int64
}

func (s *endlessSettleStore) SettleBalanceBatch(ctx context.Context, limit int) (domain.SettlementSummary, error) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.rows[id] = &fakeLedgerRow{row: domain.LedgerRow{ID: id, UserID: id, Cost: 100,
		Model: "gpt-4o", BillingTier: "auto", CallCount: 1, Format: "openai-chat"}}
	s.mu.Unlock()
	return s.fakeLedgerStore.SettleBalanceBatch(ctx, limit)
}

// TestFlusherCloseAbandonsInflightOnTimeout A-P2-8-2：驱动不尊重 ctx 时 Close
// 不再无界等待——预算到期 → Cancel baseCtx → 收尾宽限超时 → Warn 放弃排空、
// 截断退出（在途事务由编排层强杀收尾，行保持 unbilled 不丢）。
func TestFlusherCloseAbandonsInflightOnTimeout(t *testing.T) {
	old := inflightAbandonGrace
	inflightAbandonGrace = 50 * time.Millisecond
	t.Cleanup(func() { inflightAbandonGrace = old })

	inner := newFakeLedgerStore()
	store := &ignoreCtxSettleStore{fakeLedgerStore: inner, started: make(chan struct{}, 1)}
	// 单行即命中阻塞点（首车道结算语句调用即挂起）
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

// TestFlusherDrainCycleBudget 周期预算到期收尾：持续到达形态（每次 Balance 车道
// 结算前合成全新未标记行——endlessSettleStore 包装注入）下，无预算的 drainLoop
// 永不返回——会话锁 + flushMu 无界持有，refreshT/Balances.Reload 停摆 → 新用户
// 预检快照缺失 402。预算注入后 consumeCycle 必须有限墙钟内让位收尾，且消费有
// 进展、多批取数发生。
func TestFlusherDrainCycleBudget(t *testing.T) {
	restore := drainCycleBudget
	drainCycleBudget = 50 * time.Millisecond
	t.Cleanup(func() { drainCycleBudget = restore })

	inner := newFakeLedgerStore()
	store := &endlessSettleStore{fakeLedgerStore: inner}
	f := newFlusherWith(inner, 2, map[int64]int64{})
	f.store = store // 包装注入（持续到达形态）

	start := time.Now()
	n := f.consumeCycle(context.Background(), false)
	elapsed := time.Since(start)

	require.Greater(t, n, int64(0), "预算期内有真实消费进展")
	require.Less(t, elapsed, 5*time.Second, "预算到期必须让位收尾（无预算形态本调用永不返回）")
	require.GreaterOrEqual(t, inner.fetches, 3, "多批取数发生（非单批即止）")
}
