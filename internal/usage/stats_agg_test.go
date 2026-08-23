// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package usage

// 离线聚合 worker 单测（spec 2026-08-14 测试节）：fake store 断言两范围分离
// 计算、watermark 初始化/追赶、advisory lock 跳过、重放幂等参数；真实 PG 的
// LoadAggRange/AggregateRange 行为在 repository/pg_stat_test.go 覆盖。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// fakeStatsAggStore 记录 worker 调用序列（两范围参数 + watermark 状态机）。
type fakeStatsAggStore struct {
	mu         sync.Mutex
	lockOK     bool // AcquireStatsAggLock 返回值（false = 其他实例持锁）
	lockErr    error
	wm         time.Time // 当前 watermark（zero = 未初始化）
	aggErr     error     // AggregateRange 注入失败
	calls      []aggCall // LoadAggRange 调用（from/to）
	aggrCalls  []aggCall // AggregateRange 调用（delFrom/delTo/wmTo）
	initCalls  []time.Time
	rows       []*domain.StatBucket
	entityRows []*domain.EntityStatBucket
	detail     int64
}

type aggCall struct {
	from, to time.Time
}

func (s *fakeStatsAggStore) AcquireStatsAggLock(ctx context.Context) (func(), bool, error) {
	if s.lockErr != nil {
		return nil, false, s.lockErr
	}
	if !s.lockOK {
		return nil, false, nil
	}
	return func() {}, true, nil
}

func (s *fakeStatsAggStore) LoadAggRange(ctx context.Context, from, to time.Time) ([]*domain.StatBucket, []*domain.EntityStatBucket, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, aggCall{from, to})
	return s.rows, s.entityRows, s.detail, nil
}

func (s *fakeStatsAggStore) AggregateRange(ctx context.Context, delFrom, delTo, wmTo time.Time, cube []*domain.StatBucket, entity []*domain.EntityStatBucket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aggrCalls = append(s.aggrCalls, aggCall{delFrom, delTo})
	if s.aggErr != nil {
		return s.aggErr
	}
	s.wm = wmTo // 成功 → watermark 推进（与 DELETE+INSERT 同事务的观测等价）
	return nil
}

func (s *fakeStatsAggStore) LoadStatsAggWatermark(ctx context.Context) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wm, nil
}

func (s *fakeStatsAggStore) InitStatsAggWatermark(ctx context.Context, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initCalls = append(s.initCalls, t)
	if s.wm.IsZero() {
		s.wm = t
	}
	return nil
}

func (s *fakeStatsAggStore) snapshot() (calls, aggr []aggCall, wm time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]aggCall(nil), s.calls...), append([]aggCall(nil), s.aggrCalls...), s.wm
}

// TestStatsAggWorkerTwoRange spec 评审 P1-A 两范围分离：部分小时桶跨周期累积
// 不截断——cycle 1 消费 [H, H+9m) 后 watermark 推进到 T（非 R1）；cycle 2 重算
// 范围仍为小时对齐 [H, H+1h)（DELETE+SELECT 共同边界），bucket 由全量行重建。
// 重放幂等：同范围两跑 LoadAggRange 参数一致。
func TestStatsAggWorkerTwoRange(t *testing.T) {
	h := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	store := &fakeStatsAggStore{lockOK: true}
	w := NewStatsAgg(StatsAggConfig{Interval: time.Minute, Lag: time.Minute}, store, nil)

	// cycle 1：now = H+10m → 初始化 watermark = H+9m；读窗口 [H+9m, H+9m)
	// 空——先跑初始化轮（无数据），再推进。
	w.now = func() time.Time { return h.Add(10 * time.Minute) }
	w.runOnce()
	_, _, wm := store.snapshot()
	require.Equal(t, h.Add(9*time.Minute), wm, "全新库初始化 = now − 滞后")

	// cycle 2：now = H+20m → 读窗口 [H+9m, H+19m)；重算范围 = 小时对齐扩展
	// [H, H+1h)（部分小时桶不截断）。
	store.rows = []*domain.StatBucket{{BucketTime: h, RequestCount: 11}}
	store.detail = 11
	w.now = func() time.Time { return h.Add(20 * time.Minute) }
	w.runOnce()
	calls, aggr, wm := store.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, h, calls[0].from, "重算范围下界 = trunc_hour(W)")
	require.Equal(t, h.Add(time.Hour), calls[0].to, "重算范围上界 = trunc_hour(T)+1h（小时对齐扩展）")
	require.Len(t, aggr, 1)
	require.Equal(t, h, aggr[0].from, "DELETE 下界 = 重算范围下界")
	require.Equal(t, h.Add(time.Hour), aggr[0].to, "DELETE 上界 = 重算范围上界")
	require.Equal(t, h.Add(19*time.Minute), wm, "watermark 只推进到 T（≠ R1——推进到 R1 会永久跳过 [T,R1) 的行，P1-A）")

	// 观测面：watermark/上轮桶数/上轮行数已推进（cycle 2 后取值）
	st := w.Stats().(StatsAggWorkerStats)
	require.Equal(t, h.Add(19*time.Minute).UnixMilli(), st.WatermarkUnixMs)
	require.Equal(t, int64(1), st.LastBuckets)
	require.Equal(t, int64(11), st.LastRows)
	require.GreaterOrEqual(t, st.LastDurationMs, int64(0))

	// cycle 3：时钟前进（now = H+30m）→ 重放同范围（数据不变）→ LoadAggRange
	// 参数一致（幂等重放：同范围重跑结果一致）。
	w.now = func() time.Time { return h.Add(30 * time.Minute) }
	w.runOnce()
	calls, _, _ = store.snapshot()
	require.Len(t, calls, 2)
	require.Equal(t, h, calls[1].from)
	require.Equal(t, h.Add(time.Hour), calls[1].to)
}

// TestStatsAggWorkerCatchUpLimit 追赶上限（评审 P2-1）：停摆恢复后单周期窗口
// ≤ 1h 分批收敛——读窗口被钳制到 W+1h（不一次扫全史），watermark 同步只推进
// 到钳制后的 T。
func TestStatsAggWorkerCatchUpLimit(t *testing.T) {
	h := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeStatsAggStore{lockOK: true, wm: h} // 停摆前 watermark = 8 月 1 日
	w := NewStatsAgg(StatsAggConfig{Interval: time.Minute, Lag: time.Minute}, store, nil)

	// now = 8 月 3 日：落后 2 天 → 单周期窗口钳制到 1h
	w.now = func() time.Time { return h.Add(48 * time.Hour) }
	w.runOnce()
	calls, aggr, wm := store.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, h, calls[0].from)
	require.Equal(t, h.Add(2*time.Hour), calls[0].to, "重算范围上界 = trunc_hour(W+1h)+1h")
	require.Equal(t, h.Add(time.Hour), wm, "watermark 只推进到钳制后的 T = W+1h")
	require.Len(t, aggr, 1, "钳制后单周期仍走单事务")
}

// TestStatsAggWorkerLockSkip 并发防护：抢锁失败 → 本轮跳过（watermark 不动、
// 无 LoadAggRange）；锁错误 → Warn 跳过。
func TestStatsAggWorkerLockSkip(t *testing.T) {
	h := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)

	t.Run("not acquired", func(t *testing.T) {
		store := &fakeStatsAggStore{lockOK: false, wm: h}
		w := NewStatsAgg(StatsAggConfig{Interval: time.Minute}, store, nil)
		w.runOnce()
		calls, _, wm := store.snapshot()
		require.Empty(t, calls, "抢锁失败不读数据")
		require.Equal(t, h, wm, "watermark 不动")
	})

	t.Run("lock error warns and skips", func(t *testing.T) {
		store := &fakeStatsAggStore{lockErr: errors.New("boom")}
		w := NewStatsAgg(StatsAggConfig{Interval: time.Minute}, store, nil)
		w.runOnce() // 无 logger → 静默跳过（不 panic）
		calls, _, _ := store.snapshot()
		require.Empty(t, calls)
	})
}

// TestStatsAggWorkerNoDataSkip 无新数据（T ≤ W）→ 本轮跳过，不推进 watermark、
// 不读重算范围；观测面 watermark 位置仍记录当前值。
func TestStatsAggWorkerNoDataSkip(t *testing.T) {
	h := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	store := &fakeStatsAggStore{lockOK: true, wm: h.Add(10 * time.Minute)}
	w := NewStatsAgg(StatsAggConfig{Interval: time.Minute, Lag: time.Minute}, store, nil)
	w.now = func() time.Time { return h.Add(10 * time.Minute) } // T = now−lag = W
	w.runOnce()
	calls, aggr, wm := store.snapshot()
	require.Empty(t, calls, "无新数据不读重算范围")
	require.Empty(t, aggr)
	require.Equal(t, h.Add(10*time.Minute), wm, "watermark 不动")
	st := w.Stats().(StatsAggWorkerStats)
	require.Equal(t, wm.UnixMilli(), st.WatermarkUnixMs, "观测面记录当前位置")
}

// TestStatsAggWorkerFailureKeepsWatermark 失败不推进（幂等/重放核心）：聚合
// 失败 → Warn + watermark 不动 → 下轮重试同范围不双计。
func TestStatsAggWorkerFailureKeepsWatermark(t *testing.T) {
	h := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	store := &fakeStatsAggStore{lockOK: true, wm: h.Add(9 * time.Minute), aggErr: errors.New("db down")}
	w := NewStatsAgg(StatsAggConfig{Interval: time.Minute, Lag: time.Minute}, store, nil)
	w.now = func() time.Time { return h.Add(20 * time.Minute) }
	w.runOnce()
	_, _, wm := store.snapshot()
	require.Equal(t, h.Add(9*time.Minute), wm, "失败轮 watermark 不推进（重算恢复不双计）")
	st := w.Stats().(StatsAggWorkerStats)
	require.Zero(t, st.LastBuckets, "失败轮不更新观测（保留上轮值）")
}

// TestStatsAggDisabledInterval Interval <= 0 = 禁用聚合（Start 直接返回，不启动
// 循环）；Close 幂等。
func TestStatsAggDisabledInterval(t *testing.T) {
	store := &fakeStatsAggStore{lockOK: true}
	w := NewStatsAgg(StatsAggConfig{Interval: 0}, store, nil)
	require.NoError(t, w.Start(context.Background()))
	require.NoError(t, w.Close(context.Background()))
	calls, _, _ := store.snapshot()
	require.Empty(t, calls, "禁用聚合不执行任何周期")
}
