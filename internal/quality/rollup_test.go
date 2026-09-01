// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// rollup worker 单测：fake store 复刻 repo 事务语义（成功 = dirty 清除 +
// watermark 推进到该桶；失败 = 状态原样），断言选择缝下界、最老优先、失败
// 中断保序、下 tick 重试、两道隔离与生命周期。真实 PG 行为在 rollup_pg_test.go。

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeRollupStore struct {
	mu       sync.Mutex
	wm       map[string]time.Time
	dirty    map[string][]time.Time
	rolled   map[string][]time.Time
	listFrom map[string]time.Time
	fail     map[string]error // kind|bucketRFC3339 → 注入失败
	getErr   error
	listErr  error
	entered  chan struct{} // 非 nil：首次 ListDirtyMinutes 关闭一次
	block    chan struct{} // 非 nil：Rollup* 等待其关闭
}

func newFakeRollupStore() *fakeRollupStore {
	return &fakeRollupStore{
		wm:       map[string]time.Time{},
		dirty:    map[string][]time.Time{},
		rolled:   map[string][]time.Time{},
		listFrom: map[string]time.Time{},
		fail:     map[string]error{},
	}
}

func bucketKey(kind string, b time.Time) string { return kind + "|" + b.UTC().Format(time.RFC3339) }

func (s *fakeRollupStore) GetWatermark(_ context.Context, kind string, _ int16) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return time.Time{}, s.getErr
	}
	wm, ok := s.wm[kind]
	if !ok {
		return time.Time{}, sql.ErrNoRows
	}
	return wm, nil
}

func (s *fakeRollupStore) ListDirtyMinutes(_ context.Context, kind string, _ int16, from time.Time, limit int) ([]time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entered != nil {
		select {
		case <-s.entered:
		default:
			close(s.entered)
		}
	}
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.listFrom[kind] = from
	out := []time.Time{}
	for _, b := range s.dirty[kind] {
		if len(out) >= limit {
			break
		}
		if !b.Before(from) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (s *fakeRollupStore) rollup(kind string, b time.Time) error {
	if s.block != nil {
		<-s.block
	}
	key := bucketKey(kind, b)
	if err, ok := s.fail[key]; ok {
		return err
	}
	// 成功事务语义：dirty 清除 + watermark 推进到该桶（advanceWatermarkTx 同款）
	for i, d := range s.dirty[kind] {
		if d.Equal(b) {
			s.dirty[kind] = append(s.dirty[kind][:i], s.dirty[kind][i+1:]...)
			break
		}
	}
	if cur, ok := s.wm[kind]; !ok || b.After(cur) {
		s.wm[kind] = b
	}
	s.rolled[kind] = append(s.rolled[kind], b)
	return nil
}

func (s *fakeRollupStore) RollupQuality(_ context.Context, b time.Time, _ int16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rollup(rollupKindQuality, b)
}

func (s *fakeRollupStore) RollupFlow(_ context.Context, b time.Time, _ int16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rollup(rollupKindFlow, b)
}

func (s *fakeRollupStore) snapshot() (rolledQ, rolledF []time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.rolled[rollupKindQuality]...),
		append([]time.Time(nil), s.rolled[rollupKindFlow]...)
}

func newTestRollupWorker(store *fakeRollupStore) *RollupWorker {
	w := NewRollupWorker(store, RollupConfig{Interval: time.Hour}, nil)
	return w
}

// TestRollupWorkerNoop 空脏集（fresh watermark 缺失 + 无 dirty 行）→ 纯
// no-op：不调用任何 Rollup*，观测零推进。
func TestRollupWorkerNoop(t *testing.T) {
	store := newFakeRollupStore()
	w := newTestRollupWorker(store)
	w.runOnce(context.Background())
	q, f := store.snapshot()
	require.Empty(t, q)
	require.Empty(t, f)
	st := w.Stats().(RollupStats)
	require.Zero(t, st.QualityRolled)
	require.Zero(t, st.FlowRolled)
	require.Zero(t, st.Failed)
	require.Empty(t, st.LastError)
}

// TestRollupWorkerSuccessBothKinds 最老优先 + 状态成功后推进：quality 两桶、
// flow 一桶全部滚成；fake 中 dirty 清空、watermark 到最老…最新位置；选择缝
// 下界 = 读到的 watermark（fresh 库 ErrNoRows → zero）。
func TestRollupWorkerSuccessBothKinds(t *testing.T) {
	m1 := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	m2 := m1.Add(time.Minute)
	store := newFakeRollupStore()
	store.dirty[rollupKindQuality] = []time.Time{m1, m2}
	store.dirty[rollupKindFlow] = []time.Time{m1}
	w := newTestRollupWorker(store)

	w.runOnce(context.Background())

	q, f := store.snapshot()
	require.Equal(t, []time.Time{m1, m2}, q, "quality 最老优先逐桶")
	require.Equal(t, []time.Time{m1}, f, "flow 独立处理")
	st := w.Stats().(RollupStats)
	require.Equal(t, int64(2), st.QualityRolled)
	require.Equal(t, int64(1), st.FlowRolled)
	require.Equal(t, m2.UnixMilli(), st.WatermarkQualityUnixMs)
	require.Equal(t, m1.UnixMilli(), st.WatermarkFlowUnixMs)

	store.mu.Lock()
	require.Empty(t, store.dirty[rollupKindQuality], "成功桶 dirty 已清")
	store.mu.Unlock()

	// 第二轮：无脏 → no-op（不重复滚）
	w.runOnce(context.Background())
	q2, _ := store.snapshot()
	require.Equal(t, q, q2, "已滚成桶不重复")
}

// TestRollupWorkerSelectionLowerBound watermark 已存在时，选择缝以 watermark
// 为下界（低于它的迟到脏分钟留给 watermark-ordering 车道）。
func TestRollupWorkerSelectionLowerBound(t *testing.T) {
	m0 := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)
	m1 := m0.Add(time.Hour)
	m2 := m1.Add(time.Minute)
	store := newFakeRollupStore()
	store.wm[rollupKindQuality] = m1
	// m0 低于 watermark（不可滚成，必须不被选中）；m1 等于 watermark 允许重算。
	store.dirty[rollupKindQuality] = []time.Time{m0, m1, m2}
	w := newTestRollupWorker(store)

	w.runOnce(context.Background())

	q, _ := store.snapshot()
	require.Equal(t, []time.Time{m1, m2}, q, "选择下界=watermark，m0 不被选中")
	store.mu.Lock()
	require.Equal(t, m1, store.listFrom[rollupKindQuality])
	store.mu.Unlock()
}

// TestRollupWorkerFailureRetryOrder 失败中断保序 + 下 tick 重试：m2 失败 →
// m3 本轮不碰（否则 watermark 跳过 m2 永久丢分钟）；m2 恢复后下一轮从 m2
// 重试并继续 m3。quality 道失败不影响 flow 道（损失分离）。
func TestRollupWorkerFailureRetryOrder(t *testing.T) {
	m1 := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	m2 := m1.Add(time.Minute)
	m3 := m2.Add(time.Minute)
	fm := m1
	store := newFakeRollupStore()
	store.dirty[rollupKindQuality] = []time.Time{m1, m2, m3}
	store.dirty[rollupKindFlow] = []time.Time{fm}
	store.fail[bucketKey(rollupKindQuality, m2)] = errors.New("db down")
	w := newTestRollupWorker(store)

	w.runOnce(context.Background())

	q, f := store.snapshot()
	require.Equal(t, []time.Time{m1}, q, "m2 失败即中断，m3 不越序")
	require.Equal(t, []time.Time{fm}, f, "flow 道不受 quality 失败影响")
	st := w.Stats().(RollupStats)
	require.Equal(t, int64(1), st.Failed)
	require.Contains(t, st.LastError, "db down")
	require.Equal(t, m1.UnixMilli(), st.WatermarkQualityUnixMs, "watermark 观测停在最后成功桶")

	// 恢复 m2 → 下一轮从失败桶重试并推进剩余。
	store.mu.Lock()
	delete(store.fail, bucketKey(rollupKindQuality, m2))
	store.mu.Unlock()
	w.runOnce(context.Background())

	q, _ = store.snapshot()
	require.Equal(t, []time.Time{m1, m2, m3}, q, "失败桶下轮重试后全部滚成")
	st = w.Stats().(RollupStats)
	require.Equal(t, int64(3), st.QualityRolled)
}

// TestRollupWorkerStoreErrors watermark/list 读失败 → Warn 计数、不滚任何桶、
// 下轮自愈（无状态可损坏）。
func TestRollupWorkerStoreErrors(t *testing.T) {
	store := newFakeRollupStore()
	store.dirty[rollupKindQuality] = []time.Time{time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)}
	store.listErr = errors.New("select boom")
	w := newTestRollupWorker(store)
	w.runOnce(context.Background())
	q, f := store.snapshot()
	require.Empty(t, q)
	require.Empty(t, f)
	st := w.Stats().(RollupStats)
	require.Equal(t, int64(2), st.Failed, "两道的 list 各失败一次")
	require.Contains(t, st.LastError, "select boom")
}

// TestRollupWorkerStartupTickAndShutdown Start 立即执行启动 tick（channel
// 屏障等待，非 sleep）；Close 等待在途 tick 完成后返回且幂等。
func TestRollupWorkerStartupTickAndShutdown(t *testing.T) {
	store := newFakeRollupStore()
	store.entered = make(chan struct{})
	w := NewRollupWorker(store, RollupConfig{Interval: time.Hour}, nil)
	require.Equal(t, "routing-rollup", w.Name())
	require.NoError(t, w.Start(context.Background()))
	<-store.entered // 启动 tick 已到达选择缝
	require.NoError(t, w.Close(context.Background()))
	require.NoError(t, w.Close(context.Background()), "Close 幂等")
	select {
	case <-w.done:
	default:
		t.Fatal("loop did not exit after Close")
	}
}

// TestRollupWorkerCloseWaitsInFlight Close 必须等在途 tick 退出（barrier：
// Rollup* 阻塞在 channel 上，Close 不得先返回）。
func TestRollupWorkerCloseWaitsInFlight(t *testing.T) {
	m1 := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	store := newFakeRollupStore()
	store.dirty[rollupKindQuality] = []time.Time{m1}
	store.entered = make(chan struct{})
	store.block = make(chan struct{})
	w := NewRollupWorker(store, RollupConfig{Interval: time.Hour}, nil)
	require.NoError(t, w.Start(context.Background()))
	<-store.entered // 启动 tick 已进入选择缝

	closed := make(chan error, 1)
	go func() { closed <- w.Close(context.Background()) }()
	select {
	case err := <-closed:
		require.Failf(t, "Close returned while tick in flight", "err=%v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(store.block)
	require.NoError(t, <-closed)
	q, _ := store.snapshot()
	require.Equal(t, []time.Time{m1}, q, "在途桶完成后才退出")
}

// TestRollupWorkerDefaults Interval<=0 → 默认 5s（构造期钳制）；重复 Start
// 显式报错（单次生命周期，对齐 worker.Manager 契约）。
func TestRollupWorkerDefaults(t *testing.T) {
	w := NewRollupWorker(newFakeRollupStore(), RollupConfig{}, nil)
	require.Equal(t, defaultRollupInterval, w.cfg.Interval)
	require.NoError(t, w.Start(context.Background()))
	require.Error(t, w.Start(context.Background()), "二次 Start 报错")
	require.NoError(t, w.Close(context.Background()))
}
